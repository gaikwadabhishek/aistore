// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018-2024, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/ais/backend"
	"github.com/NVIDIA/aistore/ais/s3"
	"github.com/NVIDIA/aistore/api/apc"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/archive"
	"github.com/NVIDIA/aistore/cmn/atomic"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/cmn/feat"
	"github.com/NVIDIA/aistore/cmn/fname"
	"github.com/NVIDIA/aistore/cmn/kvdb"
	"github.com/NVIDIA/aistore/cmn/mono"
	"github.com/NVIDIA/aistore/cmn/nlog"
	"github.com/NVIDIA/aistore/core"
	"github.com/NVIDIA/aistore/core/meta"
	"github.com/NVIDIA/aistore/ec"
	"github.com/NVIDIA/aistore/ext/dload"
	"github.com/NVIDIA/aistore/ext/dsort"
	"github.com/NVIDIA/aistore/ext/etl"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/fs/health"
	"github.com/NVIDIA/aistore/memsys"
	"github.com/NVIDIA/aistore/mirror"
	"github.com/NVIDIA/aistore/reb"
	"github.com/NVIDIA/aistore/res"
	"github.com/NVIDIA/aistore/stats"
	"github.com/NVIDIA/aistore/transport"
	"github.com/NVIDIA/aistore/volume"
	"github.com/NVIDIA/aistore/xact/xreg"
	"github.com/NVIDIA/aistore/xact/xs"
)

const dbName = "ais.db"

const clusterClockDrift = 5 * time.Millisecond // is expected to be bounded by

type (
	regstate struct {
		mu       sync.Mutex  // serialize metasync Rx, shutdown, transition to standby; enable/disable backend
		disabled atomic.Bool // true: standing by
		prevbmd  atomic.Bool // special
	}
	backends map[string]core.Backend
	// main
	target struct {
		htrun
		backend      backends
		fshc         *health.FSHC
		fsprg        fsprungroup
		reb          *reb.Reb
		res          *res.Res
		transactions transactions
		regstate     regstate
	}
)

type redial struct {
	t         *target
	dialTout  time.Duration
	totalTout time.Duration
	inUse     string
}

// interface guard
var (
	_ cos.Runner  = (*target)(nil)
	_ htext       = (*target)(nil)
	_ core.Target = (*target)(nil)
	_ fs.HC       = (*target)(nil)
)

func (*target) Name() string { return apc.Target } // as cos.Runner

// as htext
func (*target) interruptedRestarted() (interrupted, restarted bool) {
	interrupted = fs.MarkerExists(fname.RebalanceMarker)
	restarted = fs.MarkerExists(fname.NodeRestartedPrev)
	return
}

//
// target
//

func (t *target) initBackends(tstats *stats.Trunner) {
	config := cmn.GCO.Get()
	aisbp := backend.NewAIS(t, tstats)
	t.backend[apc.AIS] = aisbp // always present

	if aisConf := config.Backend.Get(apc.AIS); aisConf != nil {
		if err := aisbp.Apply(aisConf, "init", &config.ClusterConfig); err != nil {
			nlog.Errorln(t.String()+":", err, "- proceeding to start anyway")
		} else {
			nlog.Infoln(t.String()+": remote-ais", aisConf)
		}
	}

	if err := t._initBuiltTagged(tstats, config); err != nil {
		cos.ExitLog(err)
	}
}

// - remote (e.g. cloud) backends  w/ empty stubs unless populated via build tags
// - enabled/disabled via config.Backend
func (t *target) _initBuiltTagged(tstats *stats.Trunner, config *cmn.Config) error {
	var enabled, disabled, notlinked []string

	for provider := range apc.Providers {
		var (
			add core.Backend
			err error
		)
		switch provider {
		case apc.AWS:
			add, err = backend.NewAWS(t, tstats)
		case apc.GCP:
			add, err = backend.NewGCP(t, tstats)
		case apc.Azure:
			add, err = backend.NewAzure(t, tstats)
		case apc.HT:
			add, err = backend.NewHT(t, config, tstats)
		case apc.AIS:
			continue
		default:
			return fmt.Errorf(cmn.FmtErrUnknown, t, "backend provider", provider)
		}
		t.backend[provider] = add

		configured := config.Backend.Get(provider) != nil
		switch {
		case err == nil && configured:
			enabled = append(enabled, provider)
		case err == nil && !configured:
			disabled = append(disabled, provider)
		case err != nil && configured:
			notlinked = append(notlinked, provider)
		case err != nil && !configured:
			_, ok := err.(*cmn.ErrInitBackend) // error type to indicate a _mock_ backend
			if !ok {
				return fmt.Errorf("%s: failed to initialize [%s] backend, err: %v", t, provider, err)
			}
		}
	}

	var (
		ln = len(notlinked)
		ld = len(disabled)
		le = len(enabled)
	)
	switch {
	case ln > 0:
		err := fmt.Errorf("%s backend%s: %v configured but missing in the build", t, cos.Plural(ln), notlinked)
		if le > 0 || ld > 0 {
			err = fmt.Errorf("%v (enabled: %v, disabled: %v)", err, enabled, disabled)
		}
		return err
	case ld > 0:
		nlog.Warningf("%s backend%s: %v present in the build but disabled via (or not present in) the configuration",
			t, cos.Plural(ld), disabled)
	case le == 0:
		nlog.Infoln(t.String(), "backends: none")
	default:
		nlog.Infoln(t.String(), "backends:", enabled)
	}

	return nil
}

func (t *target) aisbp() *backend.AISbp {
	bendp := t.backend[apc.AIS]
	return bendp.(*backend.AISbp)
}

func (t *target) init(config *cmn.Config) {
	t.initSnode(config)

	// (a) get node ID from command-line or env var (see envDaemonID())
	// (b) load existing node ID (replicated xattr at roots of respective mountpaths)
	// (c) generate a new one (genDaemonID())
	// - in that exact sequence
	tid, generated := initTID(config)
	if generated && len(config.FSP.Paths) > 0 {
		var recovered bool
		// in an unlikely event when losing all mountpath-stored IDs but still having a volume
		tid, recovered = volume.RecoverTID(tid, config.FSP.Paths)
		generated = !recovered

		// TODO: generated == true will not sit well with loading a local copy of Smap
		// later on during startup sequence - and not finding _this_ target in it
	}
	t.si.Init(tid, apc.Target)

	cos.InitShortID(t.si.Digest())

	memsys.Init(t.SID(), t.SID(), config)

	// new fs, check and add mountpaths
	vini := volume.IniCtx{
		UseLoopbacks:  daemon.cli.target.useLoopbackDevs,
		IgnoreMissing: daemon.cli.target.startWithLostMountpath,
		RandomTID:     generated,
	}
	newVol := volume.Init(t, config, vini)
	fs.ComputeDiskSize()

	t.initHostIP(config)
	daemon.rg.add(t)

	ts := stats.NewTrunner(t) // iostat below
	startedUp := ts.Init()    // reg common metrics (see also: "begin target metrics" below)
	daemon.rg.add(ts)
	t.statsT = ts

	k := newTalive(t, ts, startedUp)
	daemon.rg.add(k)
	t.keepalive = k

	t.fsprg.init(t, newVol) // subgroup of the daemon.rg rungroup

	sc := transport.Init(ts) // init transport sub-system; new stream collector
	daemon.rg.add(sc)

	t.fshc = health.NewFSHC(t)

	if err := ts.InitCDF(config); err != nil {
		cos.ExitLog(err)
	}
}

func (t *target) initHostIP(config *cmn.Config) {
	hostIP := os.Getenv("AIS_HOST_IP")
	if hostIP == "" {
		return
	}
	extAddr := net.ParseIP(hostIP)
	cos.AssertMsg(extAddr != nil, "invalid public IP addr via 'AIS_HOST_IP' env: "+hostIP)

	extPort := config.HostNet.Port
	if portStr := os.Getenv("AIS_HOST_PORT"); portStr != "" {
		portNum, err := cmn.ParsePort(portStr)
		cos.AssertNoErr(err)
		extPort = portNum
	}
	t.si.PubNet.Hostname = extAddr.String()
	t.si.PubNet.Port = strconv.Itoa(extPort)
	t.si.PubNet.URL = fmt.Sprintf("%s://%s:%d", config.Net.HTTP.Proto, extAddr.String(), extPort)

	nlog.Infoln("AIS_HOST_IP:", hostIP, "pub:", t.si.URL(cmn.NetPublic))

	// applies to intra-cluster networks unless separately defined
	if !config.HostNet.UseIntraControl {
		t.si.ControlNet = t.si.PubNet
	}
	if !config.HostNet.UseIntraData {
		t.si.DataNet = t.si.PubNet
	}
}

func initTID(config *cmn.Config) (tid string, generated bool) {
	if tid = envDaemonID(apc.Target); tid != "" {
		if err := cos.ValidateDaemonID(tid); err != nil {
			nlog.Errorln("Warning:", err)
		}
		return tid, false
	}

	var err error
	if tid, err = fs.LoadNodeID(config.FSP.Paths); err != nil {
		cos.ExitLog(err) // FATAL
	}
	if tid != "" {
		return tid, false
	}

	// this target: generate random ID
	tid = genDaemonID(apc.Target, config)
	err = cos.ValidateDaemonID(tid)
	debug.AssertNoErr(err)
	nlog.Infoln(meta.Tname(tid) + ": ID randomly generated")
	return tid, true
}

func regDiskMetrics(node *meta.Snode, tstats *stats.Trunner, mpi fs.MPI) {
	for _, mi := range mpi {
		for _, disk := range mi.Disks {
			tstats.RegDiskMetrics(node, disk)
		}
	}
}

func (t *target) Run() error {
	if err := t.si.Validate(); err != nil {
		cos.ExitLog(err)
	}
	config := cmn.GCO.Get()
	t.htrun.init(config)

	tstats := t.statsT.(*stats.Trunner)

	core.Tinit(t, tstats, true /*run hk*/)

	fatalErr, writeErr := t.checkRestarted(config)
	if fatalErr != nil {
		cos.ExitLog(fatalErr)
	}
	if writeErr != nil {
		nlog.Errorln("")
		nlog.Errorln(writeErr)
		nlog.Errorln("")
	}

	// register object type and workfile type
	fs.CSM.Reg(fs.ObjectType, &fs.ObjectContentResolver{})
	fs.CSM.Reg(fs.WorkfileType, &fs.WorkfileContentResolver{})

	// Init meta-owners and load local instances
	if prev := t.owner.bmd.init(); prev {
		t.regstate.prevbmd.Store(true)
	}
	t.owner.etl.init()

	smap, reliable := t.loadSmap()
	if !reliable {
		smap = newSmap()
		smap.Tmap[t.SID()] = t.si // add self to initial temp smap
	} else {
		nlog.Infoln(t.String()+": loaded", smap.StringEx())
	}
	t.owner.smap.put(smap)

	if daemon.cli.target.standby {
		tstats.Standby(true)
		t.regstate.disabled.Store(true)
		nlog.Warningln(t.String(), "not joining - standing by")

		// see endStartupStandby()
	} else {
		// discover primary and join cluster (compare with manual `apc.AdminJoin`)
		if status, err := t.joinCluster(apc.ActSelfJoinTarget); err != nil {
			nlog.Errorf("%s failed to join cluster: %v(%d)", t, err, status)
			nlog.Errorln(t.String(), "terminating")
			return err
		}
		t.markNodeStarted()
		go t.gojoin(config)
	}

	// begin target metrics, disks first -------

	avail, disabled := fs.Get()
	if len(avail) == 0 {
		cos.ExitLog(cmn.ErrNoMountpaths)
	}
	regDiskMetrics(t.si, tstats, avail)
	regDiskMetrics(t.si, tstats, disabled)

	tstats.RegMetrics(t.si)

	t.initBackends(tstats) // (+ reg backend metrics)

	// end target metrics -----------------------

	db, err := kvdb.NewBuntDB(filepath.Join(config.ConfigDir, dbName))
	if err != nil {
		nlog.Errorln(t.String(), "failed to initialize kvdb:", err)
		return err
	}

	t.transactions.init(t)

	t.reb = reb.New(config)
	t.res = res.New()

	// register storage target's handler(s) and start listening
	t.initRecvHandlers()

	ec.Init()
	mirror.Init()

	xreg.RegWithHK()

	marked := xreg.GetResilverMarked()
	if marked.Interrupted || daemon.resilver.required {
		go t.goresilver(marked.Interrupted)
	}

	dsort.Tinit(t.statsT, db, config)
	dload.Init(t.statsT, db, &config.Client)

	err = t.htrun.run(config)

	etl.StopAll()                              // stop all running ETLs if any
	cos.Close(db)                              // close kv db
	fs.RemoveMarker(fname.NodeRestartedMarker) // exit gracefully
	return err
}

// apart from minor (albeit subtle) differences between `t.joinCluster` vs `p.joinCluster`
// this method is otherwise identical to t.gojoin (TODO: unify)
func (t *target) gojoin(config *cmn.Config) {
	smap := t.owner.smap.get()
	nsti := t.pollClusterStarted(config, smap.Primary)
	if nlog.Stopping() {
		return
	}

	if nsti != nil {
		// (primary changed)
		primary := nsti.Smap.Primary
		if status, err := t.joinCluster(apc.ActSelfJoinTarget, primary.CtrlURL, primary.PubURL); err != nil {
			nlog.Errorf(fmtFailedRejoin, t, err, status)
			return
		}
	}
	t.markClusterStarted()

	if t.fsprg.newVol && !config.TestingEnv() {
		config := cmn.GCO.BeginUpdate()
		fspathsSave(config)
	}
	nlog.Infoln(t.String(), "is ready")
}

func (t *target) goresilver(interrupted bool) {
	if interrupted {
		nlog.Infoln("Resuming resilver...")
	} else if daemon.resilver.required {
		nlog.Infoln("Starting resilver, reason:", daemon.resilver.reason)
	}
	t.runResilver(res.Args{}, nil /*wg*/)
}

func (t *target) runResilver(args res.Args, wg *sync.WaitGroup) {
	// with no cluster-wide UUID it's a local run
	if args.UUID == "" {
		args.UUID = cos.GenUUID()
		regMsg := xactRegMsg{UUID: args.UUID, Kind: apc.ActResilver, Srcs: []string{t.SID()}}
		msg := t.newAmsgActVal(apc.ActRegGlobalXaction, regMsg)
		t.bcastAsyncIC(msg)
	}
	if wg != nil {
		wg.Done() // compare w/ xact.GoRunW(()
	}
	t.res.RunResilver(args)
}

func (t *target) endStartupStandby() (err error) {
	smap := t.owner.smap.get()
	if err = smap.validate(); err != nil {
		return
	}
	daemon.cli.target.standby = false
	t.markNodeStarted()
	t.markClusterStarted()
	t.regstate.disabled.Store(false)
	tstats := t.statsT.(*stats.Trunner)
	tstats.Standby(false)
	nlog.Infof("%s enabled and joined (%s)", t, smap.StringEx())

	config := cmn.GCO.Get()
	if t.fsprg.newVol && !config.TestingEnv() {
		config = cmn.GCO.BeginUpdate()
		fspathsSave(config)
	}
	return
}

func (t *target) initRecvHandlers() {
	networkHandlers := []networkHandler{
		{r: apc.Buckets, h: t.bucketHandler, net: accessNetAll},
		{r: apc.Objects, h: t.objectHandler, net: accessNetAll},
		{r: apc.Daemon, h: t.daemonHandler, net: accessNetPublicControl},
		{r: apc.Metasync, h: t.metasyncHandler, net: accessNetIntraControl},
		{r: apc.Health, h: t.healthHandler, net: accessNetPublicControl},
		{r: apc.Xactions, h: t.xactHandler, net: accessNetIntraControl},
		{r: apc.EC, h: t.ecHandler, net: accessNetIntraControl},
		{r: apc.Vote, h: t.voteHandler, net: accessNetIntraControl},
		{r: apc.Txn, h: t.txnHandler, net: accessNetIntraControl},
		{r: apc.ObjStream, h: transport.RxAnyStream, net: accessControlData},

		{r: apc.Download, h: t.downloadHandler, net: accessNetIntraControl},
		{r: apc.Sort, h: dsort.TargetHandler, net: accessControlData},
		{r: apc.ETL, h: t.etlHandler, net: accessNetAll},

		{r: "/" + apc.S3, h: t.s3Handler, net: accessNetPublicData},
		{r: "/", h: t.errURL, net: accessNetAll},
	}
	t.regNetHandlers(networkHandlers)
}

func (t *target) checkRestarted(config *cmn.Config) (fatalErr, writeErr error) {
	if fs.MarkerExists(fname.NodeRestartedMarker) {
		red := redial{t: t, dialTout: config.Timeout.CplaneOperation.D(), totalTout: config.Timeout.MaxKeepalive.D()}
		if red.acked() {
			fatalErr = fmt.Errorf("%s: %q is in use (duplicate or overlapping run?)", t, red.inUse)
			return
		}
		t.statsT.SetFlag(cos.NodeAlerts, cos.Restarted)
		fs.PersistMarker(fname.NodeRestartedPrev)
	}
	fatalErr, writeErr = fs.PersistMarker(fname.NodeRestartedMarker)
	return
}

// NOTE in re 'node-restarted' scenario: the risk of "overlapping" aisnode run -
// which'll fail shortly with "bind: address already in use" but not before
// triggering (`NodeRestartedPrev` => GFN) sequence and stealing nlog symlinks
// - this risk exists, and that's why we go extra length
func (red *redial) acked() bool {
	var (
		err   error
		tsi   = red.t.si
		sleep = cos.ProbingFrequency(red.totalTout)
		addrs = []string{tsi.PubNet.TCPEndpoint()}
		once  bool
	)
	if ep := red.t.si.DataNet.TCPEndpoint(); ep != addrs[0] {
		addrs = append(addrs, ep)
	} else if ep := red.t.si.ControlNet.TCPEndpoint(); ep != addrs[0] {
		addrs = append(addrs, ep)
	}
	for _, addr := range addrs {
		for elapsed := time.Duration(0); elapsed < red.totalTout; elapsed += sleep {
			_, err = net.DialTimeout("tcp4", addr, max(2*time.Second, red.dialTout))
			if err != nil {
				break
			}
			once = true
			time.Sleep(sleep)
			// could be shutting down
		}
		if !once {
			return false
		}
		if err == nil {
			if red.inUse == "" {
				red.inUse = addr
			}
			return true
		}
		time.Sleep(sleep)
	}
	return false // got tcp synack at least once but not (getting it) any longer
}

//
// http handlers
//

func (t *target) errURL(w http.ResponseWriter, r *http.Request) {
	if r.URL.Scheme != "" {
		t.writeErrURL(w, r)
		return
	}
	path := r.URL.Path
	if path != "" && path[0] == '/' {
		path = path[1:]
	}
	split := strings.Split(path, "/")
	// "easy URL"
	if len(split) > 0 &&
		(split[0] == apc.GSScheme || split[0] == apc.AZScheme || split[0] == apc.AISScheme) {
		t.writeErrMsg(w, r, "trying to execute \"easy URL\" via AIS target? (hint: use proxy)")
	} else {
		t.writeErrURL(w, r)
	}
}

// verb /v1/buckets
func (t *target) bucketHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		dpq := dpqAlloc()
		t.httpbckget(w, r, dpq)
		dpqFree(dpq)
	case http.MethodDelete:
		apireq := apiReqAlloc(1, apc.URLPathBuckets.L, false)
		t.httpbckdelete(w, r, apireq)
		apiReqFree(apireq)
	case http.MethodPost:
		apireq := apiReqAlloc(1, apc.URLPathBuckets.L, false)
		t.httpbckpost(w, r, apireq)
		apiReqFree(apireq)
	case http.MethodHead:
		apireq := apiReqAlloc(1, apc.URLPathBuckets.L, false)
		t.httpbckhead(w, r, apireq)
		apiReqFree(apireq)
	default:
		cmn.WriteErr405(w, r, http.MethodDelete, http.MethodGet, http.MethodHead, http.MethodPost)
	}
}

// verb /v1/objects
func (t *target) objectHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		apireq := apiReqAlloc(2, apc.URLPathObjects.L, true /*dpq*/)
		t.httpobjget(w, r, apireq)
		apiReqFree(apireq)
	case http.MethodHead:
		apireq := apiReqAlloc(2, apc.URLPathObjects.L, false)
		t.httpobjhead(w, r, apireq)
		apiReqFree(apireq)
	case http.MethodPut:
		apireq := apiReqAlloc(2, apc.URLPathObjects.L, true /*dpq*/)
		if err := t.parseReq(w, r, apireq); err == nil {
			lom := core.AllocLOM(apireq.items[1])
			t.httpobjput(w, r, apireq, lom)
			core.FreeLOM(lom)
		}
		apiReqFree(apireq)
	case http.MethodDelete:
		apireq := apiReqAlloc(2, apc.URLPathObjects.L, false)
		t.httpobjdelete(w, r, apireq)
		apiReqFree(apireq)
	case http.MethodPost:
		apireq := apiReqAlloc(2, apc.URLPathObjects.L, false /*useDpq*/)
		t.httpobjpost(w, r, apireq)
		apiReqFree(apireq)
	case http.MethodPatch:
		apireq := apiReqAlloc(2, apc.URLPathObjects.L, false)
		t.httpobjpatch(w, r, apireq)
		apiReqFree(apireq)
	default:
		cmn.WriteErr405(w, r, http.MethodDelete, http.MethodGet, http.MethodHead,
			http.MethodPost, http.MethodPut)
	}
}

//
// httpobj* handlers
//

// GET /v1/objects/<bucket-name>/<object-name>
//
// Initially validates if the request is internal request (either from proxy
// or target) and calls getObject.
//
// Checks if the object exists locally (if not, downloads it) and sends it back
// If the bucket is in the Cloud one and ValidateWarmGet is enabled there is an extra
// check whether the object exists locally. Version is checked as well if configured.
func (t *target) httpobjget(w http.ResponseWriter, r *http.Request, apireq *apiRequest) {
	err := t.parseReq(w, r, apireq)
	if err != nil {
		return
	}
	err = apireq.dpq.parse(r.URL.RawQuery)
	if err != nil {
		debug.AssertNoErr(err)
		t.writeErr(w, r, err)
		return
	}
	if cmn.Rom.Features().IsSet(feat.EnforceIntraClusterAccess) {
		if apireq.dpq.ptime == "" /*isRedirect*/ && t.isIntraCall(r.Header, false /*from primary*/) != nil {
			t.writeErrf(w, r, "%s: %s(obj) is expected to be redirected (remaddr=%s)",
				t.si, r.Method, r.RemoteAddr)
			return
		}
	}

	lom := core.AllocLOM(apireq.items[1])
	lom, err = t.getObject(w, r, apireq.dpq, apireq.bck, lom)
	if err != nil {
		t._erris(w, r, err, 0, apireq.dpq.silent)
	}
	core.FreeLOM(lom)
}

func (t *target) getObject(w http.ResponseWriter, r *http.Request, dpq *dpq, bck *meta.Bck, lom *core.LOM) (*core.LOM, error) {
	if err := lom.InitBck(bck.Bucket()); err != nil {
		if cmn.IsErrRemoteBckNotFound(err) {
			t.BMDVersionFixup(r)
			err = lom.InitBck(bck.Bucket())
		}
		if err != nil {
			return lom, err
		}
	}

	// two special flows
	if dpq.etlName != "" {
		t.getETL(w, r, dpq.etlName, lom)
		return lom, nil
	}
	if cos.IsParseBool(r.Header.Get(apc.HdrBlobDownload)) {
		var msg apc.BlobMsg
		if err := msg.FromHeader(r.Header); err != nil {
			return lom, err
		}

		// NOTE: make a blocking call w/ simultaneous Tx
		args := &core.BlobParams{
			RspW: w,
			Lom:  lom,
			Msg:  &msg,
		}
		xid, _, err := t.blobdl(args, nil /*oa*/)
		if err != nil && xid != "" {
			// (for the same reason as errSendingResp)
			nlog.Warningln("GET", lom.Cname(), "via blob-download["+xid+"]:", err)
			err = nil
		}
		return lom, err
	}

	// GET: regular | archive | range
	goi := allocGOI()
	{
		goi.atime = time.Now().UnixNano()
		goi.ltime = mono.NanoTime()
		if dpq.ptime != "" {
			if d := ptLatency(goi.atime, dpq.ptime, r.Header.Get(apc.HdrCallerIsPrimary)); d > 0 {
				t.statsT.Add(stats.GetRedirLatency, d)
			}
		}
		goi.t = t
		goi.lom = lom
		goi.dpq = dpq
		goi.req = r
		goi.w = w
		goi.ctx = context.Background()
		goi.ranges = byteRanges{Range: r.Header.Get(cos.HdrRange), Size: 0}
		goi.latestVer = _validateWarmGet(goi.lom, dpq.latestVer) // apc.QparamLatestVer || versioning.*_warm_get
	}
	if dpq.isArch() {
		if goi.ranges.Range != "" {
			details := fmt.Sprintf("range: %s, arch query: %s", goi.ranges.Range, goi.dpq._archstr())
			return lom, cmn.NewErrUnsupp("range-read archived content", details)
		}
		if dpq.arch.path != "" {
			if strings.HasPrefix(dpq.arch.path, lom.ObjName) {
				if rel, err := filepath.Rel(lom.ObjName, dpq.arch.path); err == nil {
					dpq.arch.path = rel
				}
			}
		}
	}

	// apc.QparamOrigURL
	if bck.IsHT() {
		originalURL := dpq.origURL
		goi.ctx = context.WithValue(goi.ctx, cos.CtxOriginalURL, originalURL)
	}

	// do
	if ecode, err := goi.getObject(); err != nil {
		t.statsT.IncErr(stats.ErrGetCount)
		if goi.isIOErr {
			t.statsT.IncErr(stats.IOErrGetCount)
		}

		// handle right here, return nil
		if err != errSendingResp {
			if dpq.isS3 {
				s3.WriteErr(w, r, err, ecode)
			} else {
				t._erris(w, r, err, ecode, !goi.isIOErr /*silent*/)
			}
		}
	}
	lom = goi.lom
	freeGOI(goi)
	return lom, nil
}

func _validateWarmGet(lom *core.LOM, latestVer bool /*apc.QparamLatestVer*/) bool {
	switch {
	case !lom.Bck().IsCloud() && !lom.Bck().IsRemoteAIS():
		return false
	case !latestVer:
		return lom.VersionConf().ValidateWarmGet || lom.VersionConf().Sync // bucket prop
	default:
		return true
	}
}

func (t *target) _erris(w http.ResponseWriter, r *http.Request, err error, code int, silent bool) {
	if silent { // e.g,. apc.QparamSilent, StatusNotFound
		t.writeErr(w, r, err, code, Silent)
	} else {
		t.writeErr(w, r, err, code)
	}
}

// PUT /v1/objects/bucket-name/object-name; does:
// 1) append object 2) append to archive 3) PUT
func (t *target) httpobjput(w http.ResponseWriter, r *http.Request, apireq *apiRequest, lom *core.LOM) {
	var (
		config  = cmn.GCO.Get()
		started = time.Now().UnixNano()
		t2tput  = isT2TPut(r.Header)
	)
	if apireq.dpq.ptime == "" && !t2tput {
		t.writeErrf(w, r, "%s: %s(obj) is expected to be redirected or replicated", t.si, r.Method)
		return
	}
	cs := fs.Cap()
	if errCap := cs.Err(); errCap != nil || cs.PctMax > int32(config.Space.CleanupWM) {
		cs = t.oos(config)
		if cs.IsOOS() {
			// fail this write
			t.writeErr(w, r, errCap, http.StatusInsufficientStorage)
			return
		}
	}

	// init
	if err := lom.InitBck(apireq.bck.Bucket()); err != nil {
		if cmn.IsErrRemoteBckNotFound(err) {
			t.BMDVersionFixup(r)
			err = lom.InitBck(apireq.bck.Bucket())
		}
		if err != nil {
			t.writeErr(w, r, err)
			return
		}
	}

	// load (maybe)
	skipVC := lom.IsFeatureSet(feat.SkipVC) || apireq.dpq.skipVC
	if !skipVC {
		_ = lom.Load(true, false)
	}

	// do
	var (
		handle string
		err    error
		ecode  int
	)
	switch {
	case apireq.dpq.arch.path != "": // apc.QparamArchpath
		apireq.dpq.arch.mime, err = archive.MimeFQN(t.smm, apireq.dpq.arch.mime, lom.FQN)
		if err != nil {
			break
		}
		// do
		lom.Lock(true)
		ecode, err = t.putApndArch(r, lom, started, apireq.dpq)
		lom.Unlock(true)
	case apireq.dpq.apnd.ty != "": // apc.QparamAppendType
		a := &apndOI{
			started: started,
			t:       t,
			config:  config,
			lom:     lom,
			r:       r.Body,
			op:      apireq.dpq.apnd.ty, // apc.QparamAppendType
		}
		if err := a.parse(apireq.dpq.apnd.hdl /*apc.QparamAppendHandle*/); err != nil {
			t.writeErr(w, r, err)
			return
		}
		handle, ecode, err = a.do(r)
		if err == nil && handle != "" {
			w.Header().Set(apc.HdrAppendHandle, handle)
			return
		}
		t.statsT.IncErr(stats.ErrAppendCount)
	default:
		poi := allocPOI()
		{
			poi.atime = started
			if apireq.dpq.ptime != "" {
				if d := ptLatency(poi.atime, apireq.dpq.ptime, r.Header.Get(apc.HdrCallerIsPrimary)); d > 0 {
					t.statsT.Add(stats.PutRedirLatency, d)
				}
			}
			poi.t = t
			poi.lom = lom
			poi.config = config
			poi.skipVC = skipVC // feat.SkipVC || apc.QparamSkipVC
			poi.restful = true
			poi.t2t = t2tput
		}
		ecode, err = poi.do(w.Header(), r, apireq.dpq)
		freePOI(poi)
	}
	if err != nil {
		t.FSHC(err, lom.Mountpath(), "") // TODO -- FIXME: removed from the place where happened, fqn missing...
		t.writeErr(w, r, err, ecode)
	}
}

// DELETE [ { action } ] /v1/objects/bucket-name/object-name
func (t *target) httpobjdelete(w http.ResponseWriter, r *http.Request, apireq *apiRequest) {
	var msg aisMsg
	if err := readJSON(w, r, &msg); err != nil {
		return
	}
	if err := t.parseReq(w, r, apireq); err != nil {
		return
	}
	objName := apireq.items[1]
	if isRedirect(apireq.query) == "" {
		t.writeErrf(w, r, "%s: %s(obj) is expected to be redirected", t.si, r.Method)
		return
	}

	evict := msg.Action == apc.ActEvictObjects
	lom := core.AllocLOM(objName)
	if err := lom.InitBck(apireq.bck.Bucket()); err != nil {
		t.writeErr(w, r, err)
		core.FreeLOM(lom)
		return
	}

	ecode, err := t.DeleteObject(lom, evict)
	if err == nil && ecode == 0 {
		// EC cleanup if EC is enabled
		ec.ECM.CleanupObject(lom)
	} else {
		if ecode == http.StatusNotFound {
			t.writeErrSilentf(w, r, http.StatusNotFound, "%s doesn't exist", lom.Cname())
		} else {
			t.writeErr(w, r, err, ecode)
		}
	}
	core.FreeLOM(lom)
}

// POST /v1/objects/bucket-name/object-name
func (t *target) httpobjpost(w http.ResponseWriter, r *http.Request, apireq *apiRequest) {
	msg, err := t.readActionMsg(w, r)
	if err != nil {
		return
	}
	if msg.Action == apc.ActBlobDl {
		apireq.after = 1
	}
	if t.parseReq(w, r, apireq) != nil {
		return
	}
	if isRedirect(apireq.query) == "" {
		t.writeErrf(w, r, "%s: %s-%s(obj) is expected to be redirected", t.si, r.Method, msg.Action)
		return
	}
	var lom *core.LOM
	switch msg.Action {
	case apc.ActRenameObject:
		lom = core.AllocLOM(apireq.items[1])
		if err = lom.InitBck(apireq.bck.Bucket()); err != nil {
			break
		}
		if err = t.objMv(lom, msg); err == nil {
			t.statsT.Inc(stats.RenameCount)
			core.FreeLOM(lom)
			lom = nil
		} else {
			t.statsT.IncErr(stats.ErrRenameCount)
		}
	case apc.ActBlobDl:
		// TODO: add stats.GetBlobCount and *ErrCount
		var (
			xid     string
			objName = msg.Name
			blobMsg apc.BlobMsg
		)
		lom = core.AllocLOM(objName)
		if err = lom.InitBck(apireq.bck.Bucket()); err != nil {
			break
		}
		if err = cos.MorphMarshal(msg.Value, &blobMsg); err != nil {
			err = fmt.Errorf(cmn.FmtErrMorphUnmarshal, t, "set-custom", msg.Value, err)
			break
		}
		args := &core.BlobParams{
			Lom: lom,
			Msg: &blobMsg,
		}
		if xid, _, err = t.blobdl(args, nil /*oa*/); xid != "" {
			debug.AssertNoErr(err)
			writeXid(w, xid)

			// lom is eventually freed by x-blob
		}
	default:
		t.writeErrAct(w, r, msg.Action)
		return
	}
	if err != nil {
		t.writeErr(w, r, err)
		core.FreeLOM(lom)
	}
}

// HEAD /v1/objects/<bucket-name>/<object-name>
func (t *target) httpobjhead(w http.ResponseWriter, r *http.Request, apireq *apiRequest) {
	if err := t.parseReq(w, r, apireq); err != nil {
		return
	}
	query, bck, objName := apireq.query, apireq.bck, apireq.items[1]
	if cmn.Rom.Features().IsSet(feat.EnforceIntraClusterAccess) {
		// validates that the request is internal (by a node in the same cluster)
		if isRedirect(query) == "" && t.isIntraCall(r.Header, false) != nil {
			t.writeErrf(w, r, "%s: %s(obj) is expected to be redirected (remaddr=%s)",
				t.si, r.Method, r.RemoteAddr)
			return
		}
	}
	lom := core.AllocLOM(objName)
	ecode, err := t.objHead(r, w.Header(), query, bck, lom)
	core.FreeLOM(lom)
	if err != nil {
		t._erris(w, r, err, ecode, cos.IsParseBool(query.Get(apc.QparamSilent)))
	}
}

// NOTE: sets whdr.ContentLength = obj-size, with no response body
func (t *target) objHead(r *http.Request, whdr http.Header, q url.Values, bck *meta.Bck, lom *core.LOM) (ecode int, err error) {
	var (
		fltPresence int
		hasEC       bool
		exists      = true
	)
	if tmp := q.Get(apc.QparamFltPresence); tmp != "" {
		var erp error
		fltPresence, erp = strconv.Atoi(tmp)
		debug.AssertNoErr(erp)
	}
	if err = lom.InitBck(bck.Bucket()); err != nil {
		if cmn.IsErrBucketNought(err) {
			ecode = http.StatusNotFound
		}
		return
	}
	err = lom.Load(true /*cache it*/, false /*locked*/)
	if err == nil {
		if apc.IsFltNoProps(fltPresence) {
			return
		}
		if fltPresence == apc.FltExistsOutside {
			err = fmt.Errorf(fmtOutside, lom.Cname(), fltPresence)
			return
		}
	} else {
		if !cmn.IsErrObjNought(err) {
			return
		}
		exists = false
		if fltPresence == apc.FltPresentCluster {
			exists = lom.RestoreToLocation()
		}
	}

	if !exists {
		if bck.IsAIS() || apc.IsFltPresent(fltPresence) {
			err = cos.NewErrNotFound(t, lom.Cname())
			return http.StatusNotFound, err
		}
	}

	// props
	op := cmn.ObjectProps{Name: lom.ObjName, Bck: *lom.Bucket(), Present: exists}
	if exists {
		op.ObjAttrs = *lom.ObjAttrs()
		op.Location = lom.Location()
		op.Mirror.Copies = lom.NumCopies()
		if lom.HasCopies() {
			lom.Lock(false)
			for fs := range lom.GetCopies() {
				if idx := strings.Index(fs, "/@"); idx >= 0 {
					fs = fs[:idx]
				}
				op.Mirror.Paths = append(op.Mirror.Paths, fs)
			}
			lom.Unlock(false)
		} else {
			fs := lom.FQN
			if idx := strings.Index(fs, "/@"); idx >= 0 {
				fs = fs[:idx]
			}
			op.Mirror.Paths = append(op.Mirror.Paths, fs)
		}
		if lom.ECEnabled() {
			if md, err := ec.ObjectMetadata(lom.Bck(), lom.ObjName); err == nil {
				hasEC = true
				op.EC.DataSlices = md.Data
				op.EC.ParitySlices = md.Parity
				op.EC.IsECCopy = md.IsCopy
				op.EC.Generation = md.Generation
			}
		}
	}

	latest := cos.IsParseBool(q.Get(apc.QparamLatestVer))
	if !exists || latest {
		// cold HEAD
		var oa *cmn.ObjAttrs
		oa, ecode, err = t.HeadCold(lom, r)
		if err != nil {
			if ecode != http.StatusNotFound {
				err = cmn.NewErrFailedTo(t, "HEAD", lom.Cname(), err)
			} else if latest {
				ecode = http.StatusGone
			}
			return
		}
		if apc.IsFltNoProps(fltPresence) {
			return
		}

		if exists && latest {
			if e := op.ObjAttrs.CheckEq(oa); e != nil {
				// (compare with lom.CheckRemoteMD)
				return http.StatusNotFound, cmn.NewErrRemoteMetadataMismatch(e)
			}
		} else {
			op.ObjAttrs = *oa
			op.ObjAttrs.Atime = 0
		}
	}

	// to header
	cmn.ToHeader(&op.ObjAttrs, whdr, op.ObjAttrs.Size)
	if op.ObjAttrs.Cksum == nil {
		// cos.Cksum does not have default nil/zero value (reflection)
		op.ObjAttrs.Cksum = cos.NewCksum("", "")
	}
	errIter := cmn.IterFields(op, func(tag string, field cmn.IterField) (err error, b bool) {
		if !hasEC && strings.HasPrefix(tag, "ec.") {
			return nil, false
		}
		// NOTE: op.ObjAttrs were already added via cmn.ToHeader
		if tag[0] == '.' {
			return nil, false
		}
		v := field.String()
		if v == "" {
			return nil, false
		}
		name := cmn.PropToHeader(tag)
		whdr.Set(name, v)
		return nil, false
	})
	debug.AssertNoErr(errIter)
	return
}

// PATCH /v1/objects/<bucket-name>/<object-name>
// By default, adds or updates existing custom keys. Will remove all existing keys and
// replace them with the specified ones _iff_ `apc.QparamNewCustom` is set.
func (t *target) httpobjpatch(w http.ResponseWriter, r *http.Request, apireq *apiRequest) {
	if err := t.parseReq(w, r, apireq); err != nil {
		return
	}
	if cmn.Rom.Features().IsSet(feat.EnforceIntraClusterAccess) {
		if isRedirect(apireq.query) == "" && t.isIntraCall(r.Header, false) != nil {
			t.writeErrf(w, r, "%s: %s(obj) is expected to be redirected (remaddr=%s)",
				t.si, r.Method, r.RemoteAddr)
			return
		}
	}

	msg, err := t.readActionMsg(w, r)
	if err != nil {
		return
	}
	custom := cos.StrKVs{}
	if err := cos.MorphMarshal(msg.Value, &custom); err != nil {
		t.writeErrf(w, r, cmn.FmtErrMorphUnmarshal, t.si, "set-custom", msg.Value, err)
		return
	}

	lom := core.AllocLOM(apireq.items[1] /*objName*/)
	defer core.FreeLOM(lom)
	if err := lom.InitBck(apireq.bck.Bucket()); err != nil {
		t.writeErr(w, r, err)
		return
	}
	if err := lom.Load(true /*cache it*/, false /*locked*/); err != nil {
		if cos.IsNotExist(err, 0) {
			t.writeErr(w, r, err, http.StatusNotFound)
		} else {
			t.writeErr(w, r, err)
		}
		return
	}
	delOldSetNew := cos.IsParseBool(apireq.query.Get(apc.QparamNewCustom))
	if delOldSetNew {
		lom.SetCustomMD(custom)
	} else {
		for key, val := range custom {
			lom.SetCustomKey(key, val)
		}
	}
	lom.Persist()
}

// called under lock
func (t *target) putApndArch(r *http.Request, lom *core.LOM, started int64, dpq *dpq) (int, error) {
	var (
		mime     = dpq.arch.mime // apc.QparamArchmime
		filename = dpq.arch.path // apc.QparamArchpath
		flags    int64
	)
	if strings.HasPrefix(filename, lom.ObjName) {
		if rel, err := filepath.Rel(lom.ObjName, filename); err == nil {
			filename = rel
		}
	}
	if s := r.Header.Get(apc.HdrPutApndArchFlags); s != "" {
		var errV error
		if flags, errV = strconv.ParseInt(s, 10, 64); errV != nil {
			return http.StatusBadRequest,
				fmt.Errorf("failed to archive %s: invalid flags %q in the request", lom.Cname(), s)
		}
	}
	a := &putA2I{
		started:  started,
		t:        t,
		lom:      lom,
		r:        r.Body,
		filename: filename,
		mime:     mime,
		put:      false, // below
	}
	if err := lom.Load(false /*cache it*/, true /*locked*/); err != nil {
		if !os.IsNotExist(err) {
			return http.StatusInternalServerError, err
		}
		if flags == apc.ArchAppend {
			return http.StatusNotFound, err
		}
		a.put = true
	} else {
		a.put = (flags == 0)
	}
	if s := r.Header.Get(cos.HdrContentLength); s != "" {
		if size, err := strconv.ParseInt(s, 10, 64); err == nil {
			a.size = size
		}
	}
	if a.size == 0 {
		return http.StatusBadRequest, fmt.Errorf("failed to archive %s: missing %q in the request",
			lom.Cname(), cos.HdrContentLength)
	}
	return a.do()
}

func (t *target) DeleteObject(lom *core.LOM, evict bool) (code int, err error) {
	var isback bool
	lom.Lock(true)
	code, err, isback = t.delobj(lom, evict)
	lom.Unlock(true)

	// special corner-case retry (quote):
	// - googleapi: "Error 503: We encountered an internal error. Please try again."
	// - aws-error[InternalError: We encountered an internal error. Please try again.]
	if err != nil && isback {
		if code == http.StatusServiceUnavailable || strings.Contains(err.Error(), "try again") {
			nlog.Errorf("failed to delete %s: %v(%d) - retrying...", lom, err, code)
			time.Sleep(time.Second)
			code, err = t.Backend(lom.Bck()).DeleteObj(lom)
		}
	}

	// stats
	switch {
	case err == nil:
		t.statsT.Inc(stats.DeleteCount)
	case cos.IsNotExist(err, code) || cmn.IsErrObjNought(err):
		if !evict {
			t.statsT.IncErr(stats.ErrDeleteCount) // TODO: count GET/PUT/DELETE remote errors on a per-backend...
		}
	default:
		// not to confuse with `stats.RemoteDeletedDelCount` that counts against
		// QparamLatestVer, 'versioning.validate_warm_get' and friends
		t.statsT.IncErr(stats.ErrDeleteCount)
		if !isback {
			t.statsT.IncErr(stats.IOErrDeleteCount)
		}
	}
	return code, err
}

func (t *target) delobj(lom *core.LOM, evict bool) (int, error, bool) {
	var (
		aisErr, backendErr         error
		aisErrCode, backendErrCode int
		delFromAIS, delFromBackend bool
	)
	delFromBackend = lom.Bck().IsRemote() && !evict
	err := lom.Load(false /*cache it*/, true /*locked*/)
	if err != nil {
		if !cos.IsNotExist(err, 0) {
			return 0, err, false
		}
		if !delFromBackend {
			return http.StatusNotFound, err, false
		}
	} else {
		delFromAIS = true
	}

	// do
	if delFromBackend {
		backendErrCode, backendErr = t.Backend(lom.Bck()).DeleteObj(lom)
	}
	if delFromAIS {
		size := lom.Lsize()
		aisErr = lom.RemoveObj()
		if aisErr != nil {
			if !os.IsNotExist(aisErr) {
				if backendErr != nil {
					// (unlikely)
					nlog.Errorf("double-failure to delete %s: ais err %v, backend err %v(%d)",
						lom, aisErr, backendErr, backendErrCode)
				}
				return 0, aisErr, false
			}
			debug.Assert(aisErr == nil) // expecting lom.RemoveObj() to return nil when IsNotExist
		} else if evict {
			debug.Assert(lom.Bck().IsRemote())
			t.statsT.AddMany(
				cos.NamedVal64{Name: stats.LruEvictCount, Value: 1},
				cos.NamedVal64{Name: stats.LruEvictSize, Value: size},
			)
		}
	}
	if backendErr != nil {
		return backendErrCode, backendErr, true
	}
	return aisErrCode, aisErr, false
}

// rename obj
func (t *target) objMv(lom *core.LOM, msg *apc.ActMsg) (err error) {
	if lom.Bck().IsRemote() {
		return fmt.Errorf("%s: cannot rename object %s from remote bucket", t.si, lom)
	}
	if lom.ECEnabled() {
		return fmt.Errorf("%s: cannot rename erasure-coded object %s", t.si, lom)
	}
	if msg.Name == lom.ObjName {
		return fmt.Errorf("%s: cannot rename/move object %s onto itself", t.si, lom)
	}

	buf, slab := t.gmm.Alloc()
	coiParams := core.AllocCOI()
	{
		coiParams.BckTo = lom.Bck()
		coiParams.ObjnameTo = msg.Name /* new object name */
		coiParams.Buf = buf
		coiParams.Config = cmn.GCO.Get()
		coiParams.OWT = cmn.OwtCopy
		coiParams.Finalize = true
	}
	coi := (*copyOI)(coiParams)
	_, err = coi.do(t, nil /*DM*/, lom)
	core.FreeCOI(coiParams)
	slab.Free(buf)
	if err != nil {
		return err
	}

	// TODO: combine copy+delete under a single write lock
	lom.Lock(true)
	if err := lom.RemoveObj(); err != nil {
		nlog.Warningf("%s: failed to delete renamed object %s (new name %s): %v", t, lom, msg.Name, err)
	}
	lom.Unlock(true)
	return nil
}

// compare running the same via (generic) t.xstart
func (t *target) blobdl(params *core.BlobParams, oa *cmn.ObjAttrs) (string, *xs.XactBlobDl, error) {
	// cap
	cs := fs.Cap()
	if errCap := cs.Err(); errCap != nil {
		cs = t.oos(cmn.GCO.Get())
		if err := cs.Err(); err != nil {
			return "", nil, err
		}
	}

	if oa != nil {
		return _blobdl(params, oa)
	}

	// - try-lock (above) to load, check availability
	// - unlock right away
	// - subsequently, use cmn.OwtGetPrefetchLock to finalize
	// - there's a single x-blob-download per object (see WhenPrevIsRunning)
	lom, latestVer := params.Lom, params.Msg.LatestVer
	if !lom.TryLock(false) {
		return "", nil, cmn.NewErrBusy("blob", lom.Cname())
	}

	oa, deleted, err := lom.LoadLatest(latestVer)
	lom.Unlock(false)

	// w/ assorted returns
	switch {
	case deleted: // remotely
		debug.Assert(latestVer && err != nil)
		return "", nil, err
	case oa != nil:
		debug.Assert(latestVer && err == nil)
		// not latest
	case err == nil:
		// TODO: return http.StatusNoContent
		return "", nil, nil // nothing to do
	case !cmn.IsErrObjNought(err):
		return "", nil, err
	}

	// handle: (not-present || latest-not-eq)
	return _blobdl(params, oa)
}

// returns an empty xid ("") if nothing to do
func _blobdl(params *core.BlobParams, oa *cmn.ObjAttrs) (string, *xs.XactBlobDl, error) {
	if params.WriteSGL == nil {
		// regular lom save (custom writer not present)
		wfqn := fs.CSM.Gen(params.Lom, fs.WorkfileType, "blob-dl")
		lmfh, err := params.Lom.CreateWork(wfqn)
		if err != nil {
			return "", nil, err
		}
		params.Lmfh = lmfh
		params.Wfqn = wfqn
	}
	// new
	xid := cos.GenUUID()
	rns := xs.RenewBlobDl(xid, params, oa)
	if rns.Err != nil || rns.IsRunning() { // cmn.IsErrXactUsePrev(rns.Err): single blob-downloader per blob
		if params.Lmfh != nil {
			cos.Close(params.Lmfh)
		}
		if params.Wfqn != "" {
			if errRemove := cos.RemoveFile(params.Wfqn); errRemove != nil {
				nlog.Errorln("nested err", errRemove)
			}
		}
		return "", nil, rns.Err
	}

	// a) via x-start, x-blob-download
	xblob := rns.Entry.Get().(*xs.XactBlobDl)
	if params.RspW == nil {
		go xblob.Run(nil)
		return xblob.ID(), xblob, nil
	}
	// b) via GET (blocking w/ simultaneous transmission)
	xblob.Run(nil)
	return xblob.ID(), nil, xblob.AbortErr()
}
