// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unsafe"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/filter"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/memsys"
	"github.com/NVIDIA/aistore/stats"
	"github.com/NVIDIA/aistore/transport"
	jsoniter "github.com/json-iterator/go"
)

const (
	rebalanceStreamName = "rebalance"
	rebalanceAcksName   = "remwack" // NOTE: can become generic remote-write-acknowledgment
)

// rebalance stage enum
const (
	rebStageInactive = iota
	rebStageInit
	rebStageTraverse
	rebStageWaitAck
	rebStageFin
	rebStageFinStreams
	rebStageDone
)

type (
	rebStatus struct {
		Tmap        cluster.NodeMap         `json:"tmap"`         // targets I'm waiting for ACKs from
		SmapVersion int64                   `json:"smap_version"` // current Smap version (via smapowner)
		RebVersion  int64                   `json:"reb_version"`  // Smap version of *this* rebalancing operation (m.b. invariant)
		StatsDelta  stats.ExtRebalanceStats `json:"stats_delta"`  // objects and sizes transmitted/received by this reb oper
		Stage       uint32                  `json:"stage"`        // the current stage - see enum above
		Aborted     bool                    `json:"aborted"`      // aborted?
		Running     bool                    `json:"running"`      // running?
	}
	rebManager struct {
		t          *targetrunner
		filterGFN  *filter.Filter
		netd, netc string
		smap       atomic.Pointer // new smap which will be soon live
		streams    *transport.StreamBundle
		acks       *transport.StreamBundle
		lomacks    [fs.LomCacheMask + 1]*LomAcks
		ackrc      atomic.Int64
		tcache     struct { // not to recompute very often
			tmap cluster.NodeMap
			ts   time.Time
			mu   *sync.Mutex
		}
		beginStats atomic.Pointer // *stats.ExtRebalanceStats
		stage      atomic.Uint32  // rebStage* enum
	}
	rebJoggerBase struct {
		m     *rebManager
		xreb  *xactRebBase
		mpath string
		wg    *sync.WaitGroup
	}
	globalRebJogger struct {
		rebJoggerBase
		smap  *smapX // cluster.Smap?
		sema  chan struct{}
		errCh chan error
		ver   int64
	}
	localRebJogger struct {
		rebJoggerBase
		slab *memsys.Slab2
		buf  []byte
	}
	LomAcks struct {
		mu *sync.Mutex
		q  map[string]*cluster.LOM // on the wire, waiting for ACK
	}
)

var rebStage = map[uint32]string{
	rebStageInactive:   "<inactive>",
	rebStageInit:       "<init>",
	rebStageTraverse:   "<traverse>",
	rebStageWaitAck:    "<wack>",
	rebStageFin:        "<fin>",
	rebStageFinStreams: "<fin-streams>",
	rebStageDone:       "<done>",
}

//
// rebManager
//

// via GET /v1/health (cmn.Health)
func (reb *rebManager) fillinStatus(status *rebStatus) {
	var (
		now        time.Time
		tmap       cluster.NodeMap
		config     = cmn.GCO.Get()
		sleepRetry = keepaliveRetryDuration(config)
		rsmap      = (*smapX)(reb.smap.Load())
		tsmap      = reb.t.smapowner.get()
	)
	status.Aborted, status.Running = reb.t.xactions.isRebalancing(cmn.ActGlobalReb)
	status.Stage = reb.stage.Load()
	status.SmapVersion = tsmap.version()
	if rsmap != nil {
		status.RebVersion = rsmap.version()
	}
	// stats
	beginStats := (*stats.ExtRebalanceStats)(reb.beginStats.Load())
	if beginStats == nil {
		return
	}
	curntStats := reb.getStats()
	status.StatsDelta.TxRebCount = curntStats.TxRebCount - beginStats.TxRebCount
	status.StatsDelta.RxRebCount = curntStats.RxRebCount - beginStats.RxRebCount
	status.StatsDelta.TxRebSize = curntStats.TxRebSize - beginStats.TxRebSize
	status.StatsDelta.RxRebSize = curntStats.RxRebSize - beginStats.RxRebSize

	// wack info
	if status.Stage != rebStageWaitAck {
		return
	}
	if status.SmapVersion != status.RebVersion {
		glog.Warningf("%s: Smap version %d != %d", reb.t.si.Name(), status.SmapVersion, status.RebVersion)
		return
	}

	reb.tcache.mu.Lock()
	status.Tmap, tmap = reb.tcache.tmap, reb.tcache.tmap
	now = time.Now()
	if now.Sub(reb.tcache.ts) < sleepRetry {
		reb.tcache.mu.Unlock()
		return
	}
	reb.tcache.ts = now
	for tid := range reb.tcache.tmap {
		delete(reb.tcache.tmap, tid)
	}
	max := rsmap.CountTargets() - 1
	for _, lomack := range reb.lomAcks() {
		lomack.mu.Lock()
		for _, lom := range lomack.q {
			tsi, errstr := hrwTarget(lom.Bucket, lom.Objname, rsmap)
			if errstr != "" {
				continue
			}
			tmap[tsi.DaemonID] = tsi
			if len(tmap) >= max {
				lomack.mu.Unlock()
				goto ret
			}
		}
		lomack.mu.Unlock()
	}
ret:
	reb.tcache.mu.Unlock()
	status.Stage = reb.stage.Load()
}

// main method: 10 stages, potentially with repeats
func (reb *rebManager) runGlobalReb(smap *smapX) {
	var (
		tname    = reb.t.si.Name()
		wg       = &sync.WaitGroup{}
		cancelCh = make(chan *cluster.Snode, smap.CountTargets()-1)
		ver      = smap.version()
		config   = cmn.GCO.Get()
		sleep    = config.Timeout.CplaneOperation
		maxwt    = config.Rebalance.DestRetryTime
		curwt    time.Duration
		aPaths   map[string]*fs.MountpathInfo
		cnt      int
	)
	// 1. check whether other targets are up and running
	for _, si := range smap.Tmap {
		if si.DaemonID == reb.t.si.DaemonID {
			continue
		}
		wg.Add(1)
		go func(si *cluster.Snode) {
			ok := reb.pingTarget(si, config, ver)
			if !ok {
				cancelCh <- si
			}
			wg.Done()
		}(si)
	}
	wg.Wait()
	close(cancelCh)
	if len(cancelCh) > 0 {
		if ver == reb.t.smapowner.get().version() {
			for si := range cancelCh {
				glog.Errorf("%s: skipping rebalance: %s offline, Smap v%d", tname, si.Name(), ver)
			}
		}
		return
	}

	// 2. serialize (rebalancing operations - one at a time post this point)
	//    start new xaction unless the one for the current version is already in progress
	if newerSmap, alreadyRunning := reb.serialize(smap, config); newerSmap || alreadyRunning {
		return
	}
	availablePaths, _ := fs.Mountpaths.Get()
	runnerCnt := len(availablePaths) * 2
	xreb := reb.t.xactions.renewGlobalReb(ver, runnerCnt)
	cmn.Assert(xreb != nil) // must renew given the CAS and checks above

	// 3. init streams and data structures
	reb.beginStats.Store(unsafe.Pointer(reb.getStats()))
	reb.beginStreams(config)
	reb.filterGFN.Reset() // start with empty filters
	reb.tcache.tmap = make(cluster.NodeMap, smap.CountTargets()-1)
	reb.tcache.mu = &sync.Mutex{}
	acks := reb.lomAcks()
	for i := 0; i < len(acks); i++ { // init lom acks
		acks[i] = &LomAcks{mu: &sync.Mutex{}, q: make(map[string]*cluster.LOM, 64)}
	}

	// 4. create persistent mark
	pmarker := persistentMarker(cmn.ActGlobalReb)
	file, err := cmn.CreateFile(pmarker)
	if err != nil {
		glog.Errorln("Failed to create", pmarker, err)
		pmarker = ""
	} else {
		_ = file.Close()
	}

	// 5. ready - can receive objects
	reb.smap.Store(unsafe.Pointer(smap))
	glog.Infoln(xreb.String())

	wg = &sync.WaitGroup{}
	// 6. capture stats, start mpath joggers TODO: currently supporting only fs.ObjectType (content-type)
	reb.stage.Store(rebStageTraverse)
	for _, mpathInfo := range availablePaths {
		var sema chan struct{}
		mpathC := mpathInfo.MakePath(fs.ObjectType, false /*cloud*/)
		if config.Rebalance.Multiplier > 1 {
			sema = make(chan struct{}, config.Rebalance.Multiplier)
		}
		rc := &globalRebJogger{rebJoggerBase: rebJoggerBase{m: reb, mpath: mpathC, xreb: &xreb.xactRebBase, wg: wg},
			smap: smap, sema: sema, ver: ver}
		wg.Add(1)
		go rc.jog()

		mpathL := mpathInfo.MakePath(fs.ObjectType, true /*local*/)
		if config.Rebalance.Multiplier > 1 {
			sema = make(chan struct{}, config.Rebalance.Multiplier)
		}
		rl := &globalRebJogger{rebJoggerBase: rebJoggerBase{m: reb, mpath: mpathL, xreb: &xreb.xactRebBase, wg: wg},
			smap: smap, sema: sema, ver: ver}
		wg.Add(1)
		go rl.jog()
	}
	wg.Wait()
	if xreb.Aborted() {
		glog.Infoln("abrt")
		goto term
	}

	// 7. wait for ACKs
wack:
	reb.stage.Store(rebStageWaitAck)
	curwt = 0
	maxwt += time.Duration(int64(time.Minute) * int64(smap.CountTargets()/10))
	maxwt = cmn.MinDur(maxwt, config.Rebalance.DestRetryTime*2)
	// poll for no more than maxwt while keeping track of the cumulative polling time via curwt
	// (here and elsewhere)
	for curwt < maxwt {
		cnt = 0
		var logged bool
		for _, lomack := range reb.lomAcks() {
			lomack.mu.Lock()
			if l := len(lomack.q); l > 0 {
				cnt += l
				if !logged {
					for _, lom := range lomack.q {
						tsi, errstr := hrwTarget(lom.Bucket, lom.Objname, smap)
						if errstr == "" {
							glog.Infof("waiting for %s ACK from %s", lom, tsi)
							logged = true
							break
						}
					}
				}
			}
			lomack.mu.Unlock()
			if xreb.Aborted() {
				glog.Infoln("abrt")
				goto term
			}
		}
		if cnt == 0 {
			glog.Infof("%s: received all ACKs", tname)
			break
		}
		glog.Warningf("%s: waiting for %d ACKs", tname, cnt)
		time.Sleep(sleep)
		if xreb.Aborted() {
			glog.Infoln("abrt")
			goto term
		}
		curwt += sleep
	}
	if cnt > 0 {
		glog.Warningf("%s: timed-out waiting for %d ACK(s)", tname, cnt)
	}

	// NOTE: requires locally migrated objects *not* to be removed at the src
	aPaths, _ = fs.Mountpaths.Get()
	if len(aPaths) > len(availablePaths) {
		glog.Warningf("%s: mountpath changes detected (%d, %d)", tname, len(aPaths), len(availablePaths))
	}

	// 8. synchronize with the cluster
	glog.Infof("%s: poll other targets for completion", tname)
	reb.pollDoneAll(smap, xreb)

	// 9. retransmit if needed
	cnt = reb.retransmit(xreb, config)
	if cnt > 0 {
		goto wack
	}

term:
	// 10. close streams, end xaction, deactivate GFN (FIXME: hardcoded (3, 10))
	reb.stage.Store(rebStageFin)
	maxwt, curwt = sleep*16, 0 // wait for ack cmpl refcount to zero out
	quiescent := 0             // and stay zeroed out for a while
	for curwt < maxwt {
		if rc := reb.ackrc.Load(); rc <= 0 {
			quiescent++
		} else {
			quiescent = 0
		}
		if quiescent >= 3 {
			break
		}
		time.Sleep(sleep)
		curwt += sleep
	}
	aborted := xreb.Aborted()
	if !aborted {
		if err := os.Remove(pmarker); err != nil && !os.IsNotExist(err) {
			glog.Errorf("%s: failed to remove in-progress mark %s, err: %v", tname, pmarker, err)
		}
	}
	reb.endStreams()
	reb.t.gfn.global.deactivate()
	if !xreb.Finished() {
		xreb.EndTime(time.Now())
	} else {
		glog.Infoln(xreb.String())
	}
	{
		status := &rebStatus{}
		reb.fillinStatus(status)
		delta, err := jsoniter.MarshalIndent(&status.StatsDelta, "", " ")
		if err == nil {
			glog.Infoln(string(delta))
		}
	}
	reb.stage.Store(rebStageDone)
}

func (reb *rebManager) serialize(smap *smapX, config *cmn.Config) (newerSmap, alreadyRunning bool) {
	var (
		tname = reb.t.si.Name()
		ver   = smap.version()
		sleep = config.Timeout.CplaneOperation
	)
	for {
		if reb.stage.CAS(rebStageInactive, rebStageInit) {
			break
		}
		if reb.stage.CAS(rebStageDone, rebStageInit) {
			break
		}
		//
		// vs newer Smap
		//
		nver := reb.t.smapowner.get().version()
		if nver > ver {
			glog.Warningf("%s %s: Smap v(%d, %d) - see newer Smap, not running",
				tname, rebStage[reb.stage.Load()], ver, nver)
			newerSmap = true
			return
		}
		//
		// vs current xaction
		//
		entry := reb.t.xactions.GetL(cmn.ActGlobalReb)
		if entry != nil {
			xact := entry.Get()
			if !xact.Finished() {
				runningXreb := xact.(*xactGlobalReb)
				if runningXreb.smapVersion == ver {
					glog.Warningf("%s %s: Smap v%d - is already running", tname, rebStage[reb.stage.Load()], ver)
					alreadyRunning = true
					return
				}
				if runningXreb.smapVersion < ver {
					runningXreb.Abort()
					glog.Warningf("%s %s: Smap v(%d > %d) - aborting prev and waiting for it to cleanup/exit",
						tname, rebStage[reb.stage.Load()], ver, runningXreb.smapVersion)
				}
			} else {
				glog.Warningf("%s %s: Smap v%d - waiting for %s", tname, rebStage[reb.stage.Load()], ver,
					rebStage[rebStageDone])
			}
		} else {
			glog.Warningf("%s %s: Smap v%d - waiting...", tname, rebStage[reb.stage.Load()], ver)
		}
		time.Sleep(sleep)
	}
	return
}

func (reb *rebManager) abortGlobalReb() { reb.t.xactions.abortGlobalXact(cmn.ActGlobalReb) }

func (reb *rebManager) getStats() (s *stats.ExtRebalanceStats) {
	s = &stats.ExtRebalanceStats{}
	statsRunner := getstorstatsrunner()
	s.TxRebCount = statsRunner.Get(stats.TxRebCount)
	s.RxRebCount = statsRunner.Get(stats.RxRebCount)
	s.TxRebSize = statsRunner.Get(stats.TxRebSize)
	s.RxRebSize = statsRunner.Get(stats.RxRebSize)
	return
}

func (reb *rebManager) beginStreams(config *cmn.Config) {
	cmn.Assert(reb.stage.Load() == rebStageInit)
	if config.Rebalance.Multiplier == 0 {
		config.Rebalance.Multiplier = 1
	} else if config.Rebalance.Multiplier > 8 {
		glog.Errorf("%s: stream-and-mp-jogger multiplier=%d - misconfigured?",
			reb.t.si.Name(), config.Rebalance.Multiplier)
	}
	//
	// objects
	//
	client := cmn.NewClient(cmn.ClientArgs{
		DialTimeout: config.Timeout.SendFile,
		Timeout:     config.Timeout.SendFile,
	})
	sbArgs := transport.SBArgs{
		ManualResync: true,
		Multiplier:   int(config.Rebalance.Multiplier),
		Network:      reb.netd,
		Trname:       rebalanceStreamName,
	}
	reb.streams = transport.NewStreamBundle(reb.t.smapowner, reb.t.si, client, sbArgs)

	//
	// ACKs (using the same client)
	//
	sbArgs = transport.SBArgs{
		ManualResync: true,
		Network:      reb.netc,
		Trname:       rebalanceAcksName,
	}
	reb.acks = transport.NewStreamBundle(reb.t.smapowner, reb.t.si, client, sbArgs)
	reb.ackrc.Store(0)
}

func (reb *rebManager) endStreams() {
	if reb.stage.CAS(rebStageFin, rebStageFinStreams) { // TODO: must always succeed?
		reb.streams.Close(true /* graceful */)
		reb.streams = nil
		reb.acks.Close(true)
	}
}

func (reb *rebManager) pollDoneAll(smap *smapX, xreb *xactGlobalReb) {
	wg := &sync.WaitGroup{}
	for _, si := range smap.Tmap {
		if si.DaemonID == reb.t.si.DaemonID {
			continue
		}
		wg.Add(1)
		go func(si *cluster.Snode) {
			reb.pollDone(si, smap.version(), xreb)
			wg.Done()
		}(si)
	}
	wg.Wait()
}

// wait for the neighbor a) finish traversing and b) cease waiting for my ACKs
func (reb *rebManager) pollDone(tsi *cluster.Snode, ver int64, xreb *xactGlobalReb) {
	var (
		tname      = reb.t.si.Name()
		query      = url.Values{}
		config     = cmn.GCO.Get()
		sleep      = config.Timeout.CplaneOperation
		sleepRetry = keepaliveRetryDuration(config)
		maxwt      = config.Rebalance.DestRetryTime
		curwt      time.Duration
	)
	// prepare fillinStatus() request
	query.Add(cmn.URLParamRebStatus, "true")
	args := callArgs{
		si: tsi,
		req: cmn.ReqArgs{
			Method: http.MethodGet,
			Base:   tsi.URL(cmn.NetworkIntraControl),
			Path:   cmn.URLPath(cmn.Version, cmn.Health),
			Query:  query,
		},
		timeout: defaultTimeout,
	}
	curwt = 0
	for curwt < maxwt {
		time.Sleep(sleep)
		curwt += sleep
		if xreb.Aborted() {
			glog.Infoln("abrt")
			return
		}
		res := reb.t.call(args)
		if res.err != nil {
			time.Sleep(sleepRetry)
			curwt += sleepRetry
			res = reb.t.call(args) // retry once
		}
		if res.err != nil {
			glog.Errorf("%s: failed to call %s, err: %v", tname, tsi.Name(), res.err)
			return
		}
		status := &rebStatus{}
		err := jsoniter.Unmarshal(res.outjson, status)
		if err != nil {
			glog.Errorf("Unexpected: failed to unmarshal %s response, err: %v [%v]",
				tsi.Name(), err, string(res.outjson))
			return
		}
		tver := status.SmapVersion
		if tver > ver {
			glog.Warningf("%s Smap v%d: %s has newer Smap v%d - aborting...", tname, ver, tsi.Name(), tver)
			xreb.Abort()
			return
		}
		if tver < ver {
			glog.Warningf("%s Smap v%d: %s Smap v%d(%t, %t) - more waiting...",
				tname, ver, tsi.Name(), tver, status.Aborted, status.Running)
			time.Sleep(sleepRetry)
			curwt += sleepRetry
			continue
		}
		if status.SmapVersion != status.RebVersion {
			glog.Warningf("%s Smap v%d: %s Smap v%d(v%d, %t, %t) - even more waiting...",
				tname, ver, tsi.Name(), tver, status.RebVersion, status.Aborted, status.Running)
			time.Sleep(sleepRetry)
			curwt += sleepRetry
			continue
		}
		// depending on stage the tsi is:
		if status.Stage > rebStageWaitAck {
			glog.Infof("%s %s: %s %s - done waiting",
				tname, rebStage[reb.stage.Load()], tsi.Name(), rebStage[status.Stage])
			return
		}
		if status.Stage <= rebStageTraverse {
			glog.Infof("%s %s: %s %s - keep waiting",
				tname, rebStage[reb.stage.Load()], tsi.Name(), rebStage[status.Stage])
			time.Sleep(sleepRetry)
			curwt += sleepRetry
			if status.Stage != rebStageInactive {
				curwt = 0 // NOTE: keep waiting forever or until tsi finishes traversing&transmitting
			}
		} else {
			var w4me bool // true: this target is waiting for ACKs from me (on the objects it had sent)
			for tid := range status.Tmap {
				if tid == reb.t.si.DaemonID {
					glog.Infof("%s %s: <= %s %s - keep wack",
						tname, rebStage[reb.stage.Load()], tsi.Name(), rebStage[status.Stage])
					w4me = true
					break
				}
			}
			if !w4me {
				glog.Infof("%s %s: %s %s - not waiting for me",
					tname, rebStage[reb.stage.Load()], tsi.Name(), rebStage[status.Stage])
				return
			}
			time.Sleep(sleepRetry)
			curwt += sleepRetry
		}
	}
}

// pingTarget pings target to check if it is running. After DestRetryTime it
// assumes that target is dead. Returns true if target is healthy and running,
// false otherwise.
func (reb *rebManager) pingTarget(si *cluster.Snode, config *cmn.Config, ver int64) (ok bool) {
	var (
		tname      = reb.t.si.Name()
		maxwt      = config.Rebalance.DestRetryTime
		sleep      = config.Timeout.CplaneOperation
		sleepRetry = keepaliveRetryDuration(config)
		curwt      time.Duration
		args       = callArgs{
			si: si,
			req: cmn.ReqArgs{
				Method: http.MethodGet,
				Base:   si.IntraControlNet.DirectURL,
				Path:   cmn.URLPath(cmn.Version, cmn.Health),
			},
			timeout: config.Timeout.CplaneOperation,
		}
	)
	for curwt < maxwt {
		res := reb.t.call(args)
		if res.err == nil {
			if curwt > 0 {
				glog.Infof("%s: %s is online", tname, si.Name())
			}
			return true
		}
		args.timeout = sleepRetry
		glog.Warningf("%s: waiting for %s, err %v", tname, si.Name(), res.err)
		time.Sleep(sleep)
		curwt += sleep
		nver := reb.t.smapowner.get().version()
		if nver > ver {
			return
		}
	}
	glog.Errorf("%s: timed-out waiting for %s", tname, si.Name())
	return
}

func (reb *rebManager) lomAcks() *[fs.LomCacheMask + 1]*LomAcks { return &reb.lomacks }

func (reb *rebManager) recvObj(w http.ResponseWriter, hdr transport.Header, objReader io.Reader, err error) {
	if err != nil {
		glog.Error(err)
		return
	}
	smap := (*smapX)(reb.smap.Load())
	if smap == nil {
		var (
			config = cmn.GCO.Get()
			sleep  = config.Timeout.CplaneOperation
			maxwt  = config.Rebalance.DestRetryTime
			curwt  time.Duration
		)
		maxwt = cmn.MinDur(maxwt, config.Timeout.SendFile/3)
		glog.Warningf("%s: waiting to start...", reb.t.si.Name())
		time.Sleep(sleep)
		for curwt < maxwt {
			smap = (*smapX)(reb.smap.Load())
			if smap != nil {
				break
			}
			time.Sleep(sleep)
			curwt += sleep
		}
		if curwt >= maxwt {
			glog.Errorf("%s: timed-out waiting to start, dropping %s/%s", reb.t.si.Name(), hdr.Bucket, hdr.Objname)
			return
		}
	}
	var (
		tsid = string(hdr.Opaque) // the sender
		tsi  = smap.GetTarget(tsid)
	)
	// Rx
	lom, errstr := cluster.LOM{T: reb.t, Bucket: hdr.Bucket, Objname: hdr.Objname}.Init()
	if errstr != "" {
		glog.Error(errstr)
		return
	}
	lom.SetAtimeUnix(hdr.ObjAttrs.Atime)
	lom.SetVersion(hdr.ObjAttrs.Version)
	roi := &recvObjInfo{
		started:      time.Now(),
		t:            reb.t,
		lom:          lom,
		workFQN:      fs.CSM.GenContentParsedFQN(lom.ParsedFQN, fs.WorkfileType, fs.WorkfilePut),
		r:            ioutil.NopCloser(objReader),
		cksumToCheck: cmn.NewCksum(hdr.ObjAttrs.CksumType, hdr.ObjAttrs.CksumValue),
		migrated:     true,
	}
	if err, _ := roi.recv(); err != nil {
		glog.Error(err)
		return
	}
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("%s: from %s %s", reb.t.si.Name(), tsid, roi.lom)
	}
	reb.t.statsif.AddMany(
		stats.NamedVal64{stats.RxRebCount, 1},
		stats.NamedVal64{stats.RxRebSize, hdr.ObjAttrs.Size})
	// ACK
	if tsi == nil {
		return
	}
	if stage := reb.stage.Load(); stage < rebStageFin && stage != rebStageInactive {
		hdr.Opaque = []byte(reb.t.si.DaemonID) // self == src
		hdr.ObjAttrs.Size = 0
		if err := reb.acks.SendV(hdr, nil /*reader*/, reb.ackSentCallback, nil /*ptr*/, tsi); err != nil {
			// TODO: collapse same-type errors e.g.: "src-id=>network: destination mismatch ..."
			glog.Error(err)
		} else {
			reb.ackrc.Inc()
		}
	}
}

func (reb *rebManager) ackSentCallback(_ transport.Header, _ io.ReadCloser, _ unsafe.Pointer, _ error) {
	reb.ackrc.Dec()
}

func (reb *rebManager) recvAck(w http.ResponseWriter, hdr transport.Header, objReader io.Reader, err error) {
	if err != nil {
		glog.Error(err)
		return
	}
	lom, errstr := cluster.LOM{T: reb.t, Bucket: hdr.Bucket, Objname: hdr.Objname}.Init()
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("%s: ack from %s on %s", reb.t.si.Name(), string(hdr.Opaque), lom)
	}
	if errstr != "" {
		glog.Errorln(errstr)
		return
	}
	var (
		_, idx = lom.Hkey()
		uname  = lom.Uname()
		lomack = reb.lomAcks()[idx]
	)
	lomack.mu.Lock()
	delete(lomack.q, uname)
	lomack.mu.Unlock()

	// TODO: configurable delay - postponed or manual object deletion
	cluster.ObjectLocker.Lock(uname, true)
	lom.Uncache()
	_ = lom.DelAllCopies()
	if err = os.Remove(lom.FQN); err != nil && !os.IsNotExist(err) {
		glog.Errorf("%s: error removing %s, err: %v", reb.t.si.Name(), lom, err)
	}
	cluster.ObjectLocker.Unlock(uname, true)
}

func (reb *rebManager) retransmit(xreb *xactGlobalReb, config *cmn.Config) (cnt int) {
	smap := (*smapX)(reb.smap.Load())
	aborted := func() (yes bool) {
		yes = xreb.Aborted()
		yes = yes || (smap.version() != reb.t.smapowner.get().version())
		return
	}
	if aborted() {
		return
	}
	var (
		rj    = &globalRebJogger{rebJoggerBase: rebJoggerBase{m: reb, xreb: &xreb.xactRebBase, wg: &sync.WaitGroup{}}, smap: smap}
		tname = reb.t.si.Name()
		query = url.Values{}
	)
	query.Add(cmn.URLParamSilent, "true")
	for _, lomack := range reb.lomAcks() {
		lomack.mu.Lock()
		for uname, lom := range lomack.q {
			if _, errstr := lom.Load(false); errstr != "" {
				glog.Errorf("%s: failed loading %s, err: %s", tname, lom, errstr)
				delete(lomack.q, uname)
				continue
			}
			if !lom.Exists() {
				glog.Warningf("%s: %s %s", tname, lom, cmn.DoesNotExist)
				delete(lomack.q, uname)
				continue
			}
			tsi, _ := hrwTarget(lom.Bucket, lom.Objname, smap)
			// HEAD obj
			args := callArgs{
				si: tsi,
				req: cmn.ReqArgs{
					Method: http.MethodHead,
					Base:   tsi.URL(cmn.NetworkIntraControl),
					Path:   cmn.URLPath(cmn.Version, cmn.Objects, lom.Bucket, lom.Objname),
					Query:  query,
				},
				timeout: config.Timeout.MaxKeepalive,
			}
			res := reb.t.call(args)
			if res.err == nil {
				if glog.FastV(4, glog.SmoduleAIS) {
					glog.Infof("%s: HEAD ok %s at %s", tname, lom, tsi.Name())
				}
				delete(lomack.q, uname)
				continue
			}
			// send obj
			if err := rj.send(lom, tsi, lom.Size()); err == nil {
				glog.Warningf("%s: resending %s => %s", tname, lom, tsi.Name())
				cnt++
			} else {
				glog.Errorf("%s: failed resending %s => %s, err: %v", tname, lom, tsi.Name(), err)
			}
		}
		lomack.mu.Unlock()
		if aborted() {
			return 0
		}
	}
	return
}

//
// globalRebJogger
//

func (rj *globalRebJogger) jog() {
	if rj.sema != nil {
		rj.errCh = make(chan error, cap(rj.sema)+1)
	}
	if err := filepath.Walk(rj.mpath, rj.walk); err != nil {
		if rj.xreb.Aborted() {
			glog.Infof("Aborting %s traversal", rj.mpath)
		} else {
			glog.Errorf("%s: failed to traverse %s, err: %v", rj.m.t.si.Name(), rj.mpath, err)
		}
	}
	rj.xreb.confirmCh <- struct{}{}
	rj.wg.Done()
}

func (rj *globalRebJogger) objSentCallback(hdr transport.Header, r io.ReadCloser, lomptr unsafe.Pointer, err error) {
	var (
		lom   = (*cluster.LOM)(lomptr)
		uname = lom.Uname()
		tname = rj.m.t.si.Name()
	)
	cluster.ObjectLocker.Unlock(uname, false)

	if err != nil {
		glog.Errorf("%s: failed to send o[%s/%s], err: %v", tname, hdr.Bucket, hdr.Objname, err)
		return
	}
	cmn.AssertMsg(hdr.ObjAttrs.Size == lom.Size(), lom.String()) // TODO: remove
	rj.m.t.statsif.AddMany(
		stats.NamedVal64{stats.TxRebCount, 1},
		stats.NamedVal64{stats.TxRebSize, hdr.ObjAttrs.Size})
}

// the walking callback is executed by the LRU xaction
func (rj *globalRebJogger) walk(fqn string, fi os.FileInfo, inerr error) (err error) {
	var (
		lom    *cluster.LOM
		tsi    *cluster.Snode
		errstr string
	)
	if rj.xreb.Aborted() {
		return fmt.Errorf("%s: aborted, path %s", rj.xreb, rj.mpath)
	}
	if inerr == nil && len(rj.errCh) > 0 {
		inerr = <-rj.errCh
	}
	if inerr != nil {
		if errstr = cmn.PathWalkErr(inerr); errstr != "" {
			glog.Errorf(errstr)
			return inerr
		}
		return nil
	}
	if fi.Mode().IsDir() {
		return nil
	}
	lom, errstr = cluster.LOM{T: rj.m.t, FQN: fqn}.Init()
	if errstr != "" {
		if glog.FastV(4, glog.SmoduleAIS) {
			glog.Infof("%s, err %s - skipping...", lom, errstr)
		}
		return nil
	}

	// rebalance, maybe
	tsi, errstr = hrwTarget(lom.Bucket, lom.Objname, rj.smap)
	if errstr != "" {
		return errors.New(errstr)
	}
	if tsi.DaemonID == rj.m.t.si.DaemonID {
		return nil
	}
	nver := rj.m.t.smapowner.get().version()
	if nver > rj.ver {
		rj.xreb.Abort()
		return fmt.Errorf("%s: Smap v%d < v%d, path %s", rj.xreb, rj.ver, nver, rj.mpath)
	}

	// skip objects that were already sent via GFN (due to probabilistic filtering
	// false-positives, albeit rare, are still possible)
	uname := []byte(lom.Uname())
	if rj.m.filterGFN.Lookup(uname) {
		rj.m.filterGFN.Delete(uname) // it will not be used anymore
		return nil
	}

	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("%s %s => %s", lom, rj.m.t.si.Name(), tsi.Name())
	}
	if rj.sema == nil { // rebalance.multiplier == 1
		err = rj.send(lom, tsi, fi.Size())
	} else { // // rebalance.multiplier > 1
		rj.sema <- struct{}{}
		go func() {
			ers := rj.send(lom, tsi, fi.Size())
			<-rj.sema
			if ers != nil {
				rj.errCh <- ers
			}
		}()
	}
	return
}

func (rj *globalRebJogger) send(lom *cluster.LOM, tsi *cluster.Snode, size int64) (err error) {
	var (
		file                  *cmn.FileHandle
		errstr                string
		hdr                   transport.Header
		cksum                 cmn.Cksummer
		cksumType, cksumValue string
		lomack                *LomAcks
		idx                   int
	)
	uname := lom.Uname()
	cluster.ObjectLocker.Lock(uname, false) // NOTE: unlock in objSentCallback()

	_, errstr = lom.Load(false)
	if errstr != "" || !lom.Exists() || lom.IsCopy() {
		goto rerr
	}
	if cksum, errstr = lom.CksumComputeIfMissing(); errstr != "" {
		goto rerr
	}
	cksumType, cksumValue = cksum.Get()
	if file, err = cmn.NewFileHandle(lom.FQN); err != nil {
		goto rerr
	}
	if lom.Size() != size {
		glog.Errorf("%s: %s %d != %d", rj.m.t.si.Name(), lom, lom.Size(), size) // TODO: remove
	}
	hdr = transport.Header{
		Bucket:  lom.Bucket,
		Objname: lom.Objname,
		IsLocal: lom.BckIsLocal,
		Opaque:  []byte(rj.m.t.si.DaemonID), // self == src
		ObjAttrs: transport.ObjectAttrs{
			Size:       lom.Size(),
			Atime:      lom.Atime().UnixNano(),
			CksumType:  cksumType,
			CksumValue: cksumValue,
			Version:    lom.Version(),
		},
	}
	// cache it as pending-acknowledgement (optimistically - see objSentCallback)
	_, idx = lom.Hkey()
	lomack = rj.m.lomAcks()[idx]
	lomack.mu.Lock()
	lomack.q[uname] = lom
	lomack.mu.Unlock()
	// transmit
	if err := rj.m.t.rebManager.streams.SendV(hdr, file, rj.objSentCallback, unsafe.Pointer(lom) /* cmpl ptr */, tsi); err != nil {
		lomack.mu.Lock()
		delete(lomack.q, uname)
		lomack.mu.Unlock()
		goto rerr
	}
	return nil
rerr:
	cluster.ObjectLocker.Unlock(uname, false)
	if errstr != "" {
		err = errors.New(errstr)
	}
	if err != nil {
		if glog.FastV(4, glog.SmoduleAIS) {
			glog.Errorf("%s, err: %v", lom, err)
		}
	}
	return
}

//======================================================================================
//
// Resilver
//
//======================================================================================

func (reb *rebManager) runLocalReb() {
	var (
		availablePaths, _ = fs.Mountpaths.Get()
		runnerCnt         = len(availablePaths) * 2
		xreb              = reb.t.xactions.renewLocalReb(runnerCnt)
		pmarker           = persistentMarker(cmn.ActLocalReb)
		file, err         = cmn.CreateFile(pmarker)
	)
	// deactivate local GFN
	reb.t.gfn.local.deactivate()

	if err != nil {
		glog.Errorln("Failed to create", pmarker, err)
		pmarker = ""
	} else {
		_ = file.Close()
	}
	wg := &sync.WaitGroup{}
	glog.Infof("starting local rebalance with %d runners\n", runnerCnt)
	slab := gmem2.SelectSlab2(cmn.MiB) // FIXME: estimate

	// TODO: support non-object content types
	for _, mpathInfo := range availablePaths {
		mpathC := mpathInfo.MakePath(fs.ObjectType, false /*cloud*/)
		jogger := &localRebJogger{rebJoggerBase: rebJoggerBase{m: reb, mpath: mpathC, xreb: &xreb.xactRebBase, wg: wg},
			slab: slab}
		wg.Add(1)
		go jogger.jog()

		mpathL := mpathInfo.MakePath(fs.ObjectType, true /*is local*/)
		jogger = &localRebJogger{rebJoggerBase: rebJoggerBase{m: reb, mpath: mpathL, xreb: &xreb.xactRebBase, wg: wg},
			slab: slab}
		wg.Add(1)
		go jogger.jog()
	}
	wg.Wait()

	if pmarker != "" {
		if !xreb.Aborted() {
			if err := os.Remove(pmarker); err != nil && !os.IsNotExist(err) {
				glog.Errorf("%s: failed to remove in-progress mark %s, err: %v", reb.t.si.Name(), pmarker, err)
			}
		}
	}
	xreb.EndTime(time.Now())
}

//
// localRebJogger
//

func (rj *localRebJogger) jog() {
	rj.buf = rj.slab.Alloc()
	if err := filepath.Walk(rj.mpath, rj.walk); err != nil {
		if rj.xreb.Aborted() {
			glog.Infof("Aborting %s traversal", rj.mpath)
		} else {
			glog.Errorf("%s: failed to traverse %s, err: %v", rj.m.t.si.Name(), rj.mpath, err)
		}
	}
	rj.xreb.confirmCh <- struct{}{}
	rj.slab.Free(rj.buf)
	rj.wg.Done()
}

func (rj *localRebJogger) walk(fqn string, fileInfo os.FileInfo, err error) error {
	if rj.xreb.Aborted() {
		return fmt.Errorf("%s aborted, path %s", rj.xreb, rj.mpath)
	}

	if err != nil {
		if errstr := cmn.PathWalkErr(err); errstr != "" {
			glog.Errorf(errstr)
			return err
		}
		return nil
	}
	if fileInfo.IsDir() {
		return nil
	}
	lom, errstr := cluster.LOM{T: rj.m.t, FQN: fqn}.Init()
	if errstr != "" {
		if glog.FastV(4, glog.SmoduleAIS) {
			glog.Infof("%s, err %v - skipping #1...", lom, errstr)
		}
		return nil
	}
	_, errstr = lom.Load(false)
	if errstr != "" {
		if glog.FastV(4, glog.SmoduleAIS) {
			glog.Infof("%s, err %v - skipping #2...", lom, errstr)
		}
		return nil
	}
	// skip local copies
	if !lom.Exists() || lom.IsCopy() {
		return nil
	}
	// check whether locally-misplaced
	if !lom.Misplaced() {
		return nil
	}
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("%s => %s", lom, lom.HrwFQN)
	}
	dir := filepath.Dir(lom.HrwFQN)
	if err := cmn.CreateDir(dir); err != nil {
		glog.Errorf("Failed to create dir: %s", dir)
		rj.xreb.Abort()
		rj.m.t.fshc(err, lom.HrwFQN)
		return nil
	}

	// Copy the object instead of moving, LRU takes care of obsolete copies.
	// Note that global rebalance can run at the same time and by copying we
	// allow local and global rebalance to work in parallel - global rebalance
	// can still access the old object.
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("Copying %s => %s", fqn, lom.HrwFQN)
	}

	// TODO: we take exclusive lock because the source and destination have
	// the same uname and we need to have exclusive lock on destination. If
	// we won't have exclusive we can end up with state where we read the
	// object but metadata is not yet persisted and it results in error - reproducible.
	// But taking exclusive lock on source is not a good idea since it will
	// prevent GETs from happening. Therefore, we need to think of better idea
	// to lock both source and destination but with different locks - probably
	// including mpath (whole string or some short hash) to uname, would be a good idea.
	cluster.ObjectLocker.Lock(lom.Uname(), true)
	dst, erc := lom.CopyObject(lom.HrwFQN, rj.buf)
	if erc == nil {
		erc = dst.Persist()
	}
	if erc != nil {
		cluster.ObjectLocker.Unlock(lom.Uname(), true)
		if !os.IsNotExist(erc) {
			rj.xreb.Abort()
			rj.m.t.fshc(erc, lom.HrwFQN)
			return erc
		}
		return nil
	}
	lom.Uncache()
	dst.Load(true)
	//
	// TODO: remove the object and handle local copies
	//
	cluster.ObjectLocker.Unlock(lom.Uname(), true)
	return nil
}

//
// helpers
//

// persistent mark indicating rebalancing in progress
func persistentMarker(kind string) (pm string) {
	switch kind {
	case cmn.ActLocalReb:
		pm = filepath.Join(cmn.GCO.Get().Confdir, cmn.LocalRebMarker)
	case cmn.ActGlobalReb:
		pm = filepath.Join(cmn.GCO.Get().Confdir, cmn.GlobalRebMarker)
	default:
		cmn.Assert(false)
	}
	return
}
