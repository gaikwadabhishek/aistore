# AIStore Development Guide

## What is AIStore?

AIStore (AIS) is a lightweight distributed storage system for AI workloads providing:
- Multi-cloud unified access (AWS S3, GCS, Azure, OCI)
- Linear scalability with elastic cluster runtime growth
- High-performance I/O with no routing overhead
- AI-optimized features: sharding, ETL, batch ops, PyTorch integration

**Architecture**: Proxy nodes (gateways) + Target nodes (storage) with distributed metadata consensus.

## Tech Stack

**Languages**: Go 1.25 (daemon, CLI), Python 3.x (SDK, PyTorch)
**Key Deps**: AWS/GCP/Azure SDKs, fasthttp, msgp, k8s.io/client-go, reedsolomon, Prometheus
**Build Tags**: `aws`, `gcp`, `azure`, `oci`, `debug`, `mono`, `nethttp`, `oteltracing`

## Project Structure

```
ais/ Main daemon (proxy.go, target.go, htrun.go, metasync, txn)
core/ Interfaces (Node, Target, Backend, LOM), meta/
cmn/ Config, utilities, cos/ (errors, time), nlog/
api/ Go client API, apc/ (constants), env/
cmd/ Binaries: aisnode, cli, aisloader, authn, ishard, xmeta
python/ SDK (aistore/sdk/), PyTorch, boto3 patch
transport/ High-perf object streaming with callbacks
fs/ Mountpath mgmt, health checker (FSHC)
ext/ Extensions: dsort, etl
xact/ Async batch jobs (rebalance, EC, mirror, prefetch)
deploy/ Local/K8s configs, dev/local/ scripts
docs/ Markdown docs
bench/ Benchmarking tools
scripts/ Build/test/CI automation
```

## Essential Commands

### Build
```bash
make node # Build aisnode daemon
TAGS="aws gcp" make node # With cloud backends
make cli # Build CLI (static binary)
make all # Build all (node, cli, authn, aisloader)
MODE=debug make node # Debug build with symbols
```

### Local Dev Cluster
```bash
make deploy # Interactive prompts
make kill deploy <<< $'7\n2\n4\ny\ny\n' # 7 targets, 2 proxies, 4 mountpaths
TAGS="aws gcp" make kill deploy <<< $'5\n1\n' # With backends
make run # Use existing configs
make restart # Kill + run
make kill # Stop cluster
make clean # Full cleanup
```

### Testing
```bash
BUCKET=tmp make test-short # Short tests (~10 min)
BUCKET=mybucket make test-long # Full integration
RE="TestETL" BUCKET=tmp make test-run # Specific test
BUCKET=tmp make ci # Lint + spell + short tests
GORACE='log_path=/tmp/race' make deploy # With race detector
cd cmd/cli && go test -v -tags=debug ./... # CLI tests
cd python && pytest tests/ # Python tests
```

### Linting
```bash
make lint-update # Install golangci-lint
make lint # Go linter
make lint-python # Python linter
make fmt-check # Format check
make fmt-fix # Auto-format
make spell-check # Spell checker
```

### Profiling
```bash
MEM_PROFILE=/tmp/mem make deploy # Memory profiling (needs graceful shutdown)
CPU_PROFILE=/tmp/cpu make deploy # CPU profiling
```

## Key File References

**Startup**: `cmd/aisnode/main.go:21`, `ais/proxy.go:130`, `ais/target.go:90`, `ais/earlystart.go:100`
**HTTP**: `ais/http.go:30` (handlers), `ais/proxy.go:250` (routes), `ais/target.go:400`, `ais/prxs3.go`, `ais/tgts3.go`
**Metadata**: `cmn/gco.go:20` (config), `ais/bucketmeta.go:55` (BMD), `ais/clustermap.go:58` (Smap), `ais/metasync.go:112` (REVS)
**Objects**: `ais/tgtobj.go:200` (PUT), `ais/tgtobj.go:600` (GET), `core/lom.go` (Local Object Metadata)
**Batch**: `ais/prxtxn.go:55` (2PC), `xact/xs/xs.go` (framework), `ais/prxdl.go`(download)

## Additional Documentation

`.claude/docs/architectural_patterns.md` - Design patterns: interfaces, metadata ownership, messaging, transactions, concurrency, error handling, config mgmt, callbacks

**Core Docs** (in `docs/`):
- `overview.md` - Architecture, terminology, diagrams, xactions
- `bucket.md` - Bucket identity, properties, lifecycle
- `networking.md` - Network separation, IPv6, multi-homing
- `batch.md` - 30+ batch operations, xaction lifecycle
- `etl.md` - ETL framework, transforms
- `performance.md` - Tuning guides
- `cli.md` - CLI reference
- `python_sdk.md` - Python SDK guide
- `getting_started.md` - Quick start, deployment options

**Component READMEs**: `cmd/cli/`, `cmd/ishard/`, `python/aistore/sdk/`, `python/aistore/pytorch/`, `transport/`, `CONTRIBUTING.md`

## Common Workflows

**Add Bucket Property**: Modify `cmn.Bprops` (`cmn/api.go`) → validate (`cmn/config.go`) → update BMD (`ais/bucketmeta.go`) → handlers (`ais/prxbck.go`, `ais/tgtbck.go`) → CLI (`cmd/cli/cli/`) → tests

**Add Backend Provider**: Implement `core.Backend` (`ais/backend/`) → register (`ais/target.go:initBackends()`) → add constant (`api/apc/provider.go`) → docs

**Add Xaction**: Define in `xact/xs/` or `xact/xreg/` → implement `xact.Snap` → register (`xact/xreg/xreg.go`) → CLI command

**Debug**: `ais config cluster log.level=4`, use `MODE=debug` builds, `ais log show`, `xmeta -in <path>`, trace `[TID]` in logs

## Python Code Style

- **Docstring args**: Always include the type — `param_name (type): description`. E.g., `node_id (str): Daemon ID` not `node_id: Daemon ID`.
- **Docstrings**: Use single backticks (`` ` ``) for inline code references, not double backticks.

## Git & Commit Rules

- **Commit messages**: Lowercase first word in summary line (e.g. `transformers: bump ...`), then concise bullet points. Never include `Co-Authored-By` lines. Always use `git commit -s` for the `Signed-off-by` line — never write it manually. Always spell-check the commit message before committing and when reviewing MRs.
- **Never push** to any remote without explicitly asking the user first.
- **GitLab commits** require a `Signed-off-by` line (use `git commit -s`).
