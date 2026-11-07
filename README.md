# sw-test-runner

> **Status:** incubator. Stable enough to drive V3 / V2 product validation today; module path may move once the project finds a permanent home.

A YAML-driven scenario runner for storage-product validation: deploy
binaries to remote nodes, sequence multi-host actions, run benchmarks,
inject faults, collect artifacts, emit JUnit XML.

## What it is for

- end-to-end smoke tests for block-storage products (V2 weed-block, V3 seaweed-block)
- S3 gateway and RDMA lab gates for object/VFS data paths
- iSCSI / NVMe-oF target integration scenarios
- multi-node failover and rebuild scenarios
- chaos / fault injection (`netem`, `iptables`, disk-fill, kill-loop)
- fio / dd performance comparisons across binaries
- regression baselines + Prometheus metric scrapes

It is **not** a generic CI runner; it is a scenario engine that knows about
storage daemons and the protocols they expose.

## Architecture in one paragraph

A **registry** of named **actions** (`exec`, `iscsi_login_direct`, `dd_write`,
`v3_start_blockmaster`, ...) is composed by **packs** (`packs/block` for V2,
`packs/v3block` for V3, ...) on top of a product-agnostic **framework**
(parser, engine, reporter, coordinator) that talks to remote nodes through
an abstract `infra.Node` SSH/local exec interface. Scenarios are YAML files
that sequence actions across phases. Each run produces an artifact bundle
(`result.json`, `result.xml`, per-daemon logs, metrics, optional HTML).

## Quick install

```bash
go install github.com/pingqiu/sw-test-runner/cmd/sw-test-runner@latest

sw-test-runner list                        # show registered actions by tier
sw-test-runner validate scenario.yaml      # parse + lint
sw-test-runner run scenario.yaml           # execute
sw-test-runner validate-bundle results/RUN # offline result/status check
```

## How to run `seaweed-cloud direct`

For the operator-staged tp01 nightly sweep, see [Nightly local regression](docs/nightly-regression.md).

Use one existing Linux host with SSH, systemd, passwordless sudo, Python 3,
`curl`, `tar`, `sha256sum`, and `ss`. A local container reached over SSH with
real systemd is an **onprem** host: set `target: "onprem"`, not `docker`.
For a shared lab host, obtain its run window first and use SSH user `testdev`.
The runner installs under the user's run directory; it does not create a VM.

```sh
go build -o seaweed-cloud ./cmd/seaweed-cloud
cp inventory/testdata/http.requested.json direct-inventory.json
cp direct-inventory.json ec-inventory.json
mkdir -p results
# Edit both inventories and prepare private controller configs as below.

# Default HTTP KV smoke (local container or the allocated lab host):
./seaweed-cloud direct --inventory direct-inventory.json \
  --config kv-config.json --out results/direct-kv-001 --timeout 8m

# EC stage 1 (local container first; fresh run_id/run_root for this run):
./seaweed-cloud direct --inventory ec-inventory.json \
  --config ec-config.json --out results/direct-ec-001 --timeout 8m
```

Inventory fields to edit in the template:

- Keep `schema: "seaweed.inventory/v1"`, `state: "requested"`, `target: "onprem"`,
  `transport: "http"`, and `managed_resource_ids: []`. Use one host with roles
  `master`, `filer`, `volume`, `s3`, `client`; replace the example run ID and
  `artifact_manifest_sha256` with your run's values.
- Set `management.address`, `port`, `user`, and `credential_ref` for the actual
  SSH endpoint. Set `data.address` to that host's reachable, non-loopback data IP
  (the container's own IP for EC peer copies), not the template's example IP.
- Set `run_root` to `/home/<user>/sw-runs/<run_id>`; for root use
  `/root/sw-runs/<run_id>`. Declare the actual disk/mount, with
  `format_allowed: false`; direct does not format it.
- Keep explicit service ports; ensure they are free. EC derives fourteen
  consecutive volume HTTP ports and fourteen gRPC ports from their first ports,
  plus S3 gRPC = S3 HTTP + 10000: all 34 listeners must be distinct/free.
- Set `deadlines.work` and `deadlines.hard` to canonical UTC timestamps ending
  in `Z`, with `cleanup_reserve_ms: 60000` and hard at least 60s after work.

Controller config fields (keep credentials private; do not commit configs):

- `ssh_key`: operator-supplied SSH credential; `host_key`: independently verified
  public host-key pin for that endpoint. No credential is embedded in inventory.
- `files`: map artifact names to `{ "path": ..., "sha256": ... }`, using local
  artifact files and their actual SHA256 values. KV requires `weed`,
  `weed-volume`, and `native.tar` (containing `seaweedkv_native/`, compatible
  with the target's Python ABI and system libraries).
- For EC set `ec_stage1: true` and supply **only** `weed` in `files`.
  For Rust holders, also set `ec_volume_impl: "rust"` and pin `weed-volume`
  in `files`; master, filer and S3 still use Go `weed`. It starts
  one Go master/filer/S3 plus 14 holders, seeds 64 keys, encodes, rejects a
  planted wrong byte, checks both routes, then kills 1–4 holders cumulatively.
  S1 can PASS; S2 stays `NOT_COVERED` without a reconstruction marker.

Create the output parent first; `--out` itself must not already exist. The CLI
prints one JSON result with `code`, `rows`, and `output`, and exits nonzero on
failure. Local output includes `cloud-step-*.json` (commands and raw terminals),
`cloud-kv.jsonl` or `ec-stage1.json`, `cloud-services.log`, and collection,
cleanup and postflight JSON. Cleanup uses its own 30s collection + 90s teardown
bounds even after workload failure/cancellation; budget that beyond `--timeout`.
It stops the run's transient units and requires zero units/listeners. Run data
remains under `run_root`; retain the local evidence before disposing of it.

Optional single-host HTTP master timing: set `master_writes:true` in the KV
config (keep its three artifacts; do not combine with EC/chaos/replication).
The same direct command runs 120s of 64 KiB fresh-key SETs and ACKed-key GETs, kills
the master after 30s, starts a fresh master generation with the same data directory, then reads back
ACKed keys. `master-writes.json` is always `REPORT_ONLY`: counts/latencies
before confirmed GONE, GONE-to-READY and after READY, plus exact/missing readback.
`kill_confirmed_mono` is stamped after the victim is gone. `first_ack_after_kill_s`
measures from that stamp; `first_ack_after_ready_s` measures from leader READY. Completed failures are reported; unknown completion or wrong
bytes fails. Raw worker JSONL, service logs and cleanup receipts remain in OUT.

## Docs and wiki

Full documentation is a MkDocs wiki under `docs/` (mirrors the seaweed-block
wiki). Serve it locally:

```bash
pip install -r requirements-docs.txt
mkdocs serve            # http://127.0.0.1:8000   (or: make wiki)
```

- **Start here:** `docs/testops-handbook.md`, `docs/cross-product-testops-standard.md`, `docs/scenario-spec.md`, `docs/tutorial.md`
- **For development agents:** `docs/agent-testops-runbook.md` — the short
  guide for Block, S3, VFS, and RDMA agents running shared lab tests and
  reporting evidence.
- **Reference:** `docs/wiki/` — code map, packs & binaries, scenario catalog, product testing guides, the live dashboard
- **Live run dashboard/controller:** http://192.168.1.181:9099/ (global run view; M01 can enable RDMA queue submit)

## Agent quick start

For RDMA PR/lab validation, submit to the M01 queue so the lab lock, shared
results, and dashboard metadata are handled consistently:

```bash
ssh testdev@192.168.1.181
cd /opt/rdma-lab-ci/sw-test-runner
TESTOPS_MONO_REF=<branch-or-sha> ./scripts/testops-ci-submit.sh
```

For Block/S3/VFS scenarios, run the product binary with shared results and
metadata:

```bash
swblock run <scenario.yaml> -results-dir /mnt/smb/work/share/testops/results/block-qa \
  -meta project=block-qa -meta team=block -meta run_by=<agent> \
  -meta test_id=<test-id> -meta branch=<branch-or-sha> -meta commit=<sha>
```

Watch runs at http://192.168.1.181:9099/. Report the `project/run_id`, bundle
path, branch/commit, pass/fail, and key metrics.

If the dashboard is started with controller flags, the same page can submit the
RDMA queue:

```bash
curl -X POST http://m01:9099/api/rdma/submit \
  -H 'Content-Type: application/json' \
  -d '{"mono_ref":"main","run_by":"dev-agent"}'
```

## Build

```bash
make build               # all cmd/ binaries into ./bin
make test                # unit tests
make wiki                # serve the wiki locally
```

## Repository layout

```
.
├── README.md
├── LICENSE                       Apache-2.0
├── go.mod                        github.com/pingqiu/sw-test-runner
├── engine.go, parser.go, ...     framework: scenario sequencer + YAML parser
├── registry.go, types.go         action registry + ActionContext
├── reporter.go, baseline.go      JUnit XML, regression baselines
├── coordinator.go, agent.go      multi-node distributed mode
├── actions/                      product-agnostic actions
│   ├── system.go                 exec, sleep, assert_*, ssh, scp
│   ├── fault.go                  netem, partition, kill_pid, fill_disk
│   ├── bench.go, benchmark.go    fio loops, perf parsing
│   ├── iscsi.go, io.go, k8s.go   iSCSI client, dd/mkfs/mount, kubectl
│   ├── recovery.go, results.go   verify, sha256, JSON output
│   └── ...
├── infra/                        agnostic node abstractions
│   ├── node.go                   SSH or local exec
│   ├── target.go, ha_target.go   daemon lifecycle helpers
│   └── iscsi_client.go, fault.go
├── packs/                        product-specific action sets
│   ├── block/                    V2 seaweedfs weed-block
│   ├── kv/                       V2 seaweedfs kv/object
│   ├── s3/                       SeaweedFS S3 gateway
│   ├── rdma/                     M01/M02 RDMA lab gates
│   └── v3block/                  V3 seaweed-block
├── scenarios/                    bundled scenario YAMLs (V2 baselines + V3)
├── cmd/sw-test-runner/           CLI entry
├── cmd/testops-controller/       safe RDMA queue submit/status web API
└── internal/blockapi/            internal HTTP client (V2 master REST)
```

## Writing a scenario

Minimum scenario:

```yaml
name: my-scenario
timeout: 2m
topology:
  nodes:
    target:
      host: 127.0.0.1
      is_local: true
phases:
  - name: hello
    actions:
      - action: exec
        node: target
        cmd: echo hello world
```

Validate, then run:

```bash
sw-test-runner validate my-scenario.yaml
sw-test-runner run my-scenario.yaml
```

Output:

```
results/
└── 20260504-100000-abcd/
    ├── manifest.json     run metadata
    ├── result.json       structured per-phase results
    ├── result.xml        JUnit XML
    ├── result.html       human-readable
    ├── scenario.yaml     echo of input
    └── artifacts/        per-daemon logs, fio JSON, etc.
```

Validate a completed bundle without touching the lab:

```bash
sw-test-runner validate-bundle \
  --profile protocol-release-gate \
  --expect-commit a0175f8 \
  results/protocol-release-gate-run
```

Named profiles encode product-owned suite contracts. For Seaweed Block beta
hardening:

```bash
swblock validate-bundle \
  --profile beta-hardening \
  --expect-commit de165f0 \
  results/beta-hardening-gate-run
```

## Adding a product pack

1. New directory `packs/yourproduct/`.
2. `RegisterPack(r *Registry)` calls `r.RegisterFunc("yourproduct_action", TierDevOps, handler)`.
3. Action handler signature:

   ```go
   func(ctx context.Context, actx *ActionContext, act Action) (map[string]string, error)
   ```

4. Wire it in `cmd/sw-test-runner/main.go::registerAll` (or build a tiny custom main that registers `actions.RegisterCore(r)` + your pack only).

See `packs/v3block/` for a worked example (~600 LOC, 7 actions: spawn three
V3 daemon types, apply cluster spec, wait for primary, parse status, assert
no-V2-authority-leak in logs).

For cross-product storage gates, start with
`docs/storage-testops-platform.md`. It maps the current block, S3, VFS, and
RDMA surfaces and points at the RDMA unified lab scenario.

## Tiers (action categories)

```
core       agnostic    exec, sleep, assert_*, print, grep_log
block      V2 storage  start_target, iscsi_*, dd_*, fio_*, assign, metrics
devops     daemons     start_weed_master, start_weed_volume, v3_start_*
chaos      faults      inject_netem, inject_partition, fill_disk, kill_loop
k8s        K8s         kubectl_apply, kubectl_wait_condition, kubectl_logs
```

A pack registers actions under whichever tier fits.

## Discipline

- Actions must not embed product authority semantics (no `promote`,
  `demote`, `force-failover` shortcuts that bypass the product control plane).
- Actions communicate via shell-out and HTTP/gRPC clients only — no direct
  imports of product daemon internal types. This is what keeps the runner
  pluggable across V2/V3 and cross-language products.
- Test the testrunner on its own (`go test ./... -count=1`); product
  validation is what scenarios run on real labs.

## Status

This is currently an incubator under a personal account. Module path may
relocate to a project org repo once the surface stabilizes. Pin to a commit
hash if you need stability across the move.

## License

Apache-2.0. See `LICENSE`.

## Deployment inventory contract

Cloud provisioning and on-prem deployment share the versioned
[`seaweed.inventory/v1` contract](inventory/README.md). The standalone validator
checks data only; it cannot authorize or launch jobs.

Azure provision-only module: [provision/terraform/azure](provision/terraform/azure/README.md). Cloud execution uses seaweed-cloud plan/run/destroy; existing hosts use seaweed-cloud direct.

### Two-host DC read cases

On the M01 controller, use the direct entry with `read_case` set to
`volume-restart`, `filer-restart`, or `volume-freeze`. Use the same two-host
onprem DC inventory and `soak_profile` / `soak_release` fields as KV soak;
the profile input must omit `soak`. Do not also set `kv_soak` or another case.
The profile carries the current host observations and release files for both hosts.

```sh
seaweed-cloud direct --inventory requested.json --config read-case.json \
  --out new-read-result --timeout 10m
```

Each case seeds exact bytes, runs 120 s of DC GETs, and faults after 30 s.
Restart cases check fresh DC and HTTP recovery; filer HTTP recovery checks
retained metadata through the replacement. Freeze holds the volume for at least
20 s and requires a real overlapping GET plus continuing load after validated
resume; HTTP controls remain deferred for freeze. `read-case.json` in the output
contains the case-only result and receipts; overall success requires the CLI result
and clean per-host cleanup/postflight logs. An uncertain STOP is UNKNOWN: teardown
kills the victim without racing it with CONT. Wrong bytes, missing proof and unclean
postflight fail; missing freeze exposure is NOT_COVERED. Real RDMA runs require a lab token/window.

HTTP process chaos on one existing container host or two physical hosts uses the same direct entry; see [cluster process cases](docs/cluster-process-direct.md) for `cluster_chaos_cases` and the observed two-copy gate.


### KV smoke through the product CLI (M2)

The direct entry can use the mono `swctl` CLI for KV smoke deployment and
teardown. Keep the usual direct SSH settings and inventory, and select:

```json
{"files":{},"swctl":{"binary":{"path":"/controller/bin/swctl","sha256":"<sha256>"},"cluster":"/controller/cluster.yaml","artifacts_dir":"/controller/release"}}
```

Run the same `bin/seaweed-cloud direct --inventory INV.json --config CFG.json
--out NEW_DIR`. This selector accepts an onprem RC/DC inventory matching the
cluster's manifest digest, hosts, roles, data endpoints and pinned SSH identities
(standard port 22). It freezes the cluster file before any apply. Other direct
selectors and the default HTTP smoke keep their existing deployment path.

The cluster must declare absolute `kv.lmcache.base_python` on its LMCache host,
pointing to an existing dependency-bearing environment qualified by `swctl`.
The CLI installs the pinned LMCache/native wheels offline into an overlay; it
never installs dependencies into that base. The adapter freezes this field with
the rest of the cluster file and delegates strict validation to the real CLI.
A missing/unqualified base refuses before apply with `TARGET_BASE_ENV_MISSING`.

The bounded smoke retains its original 16 keys, 1 MiB payloads, native worker
and oracle. Its fixed client tuple requires explicit `num_workers: 4`, `pipes:
8`, and `push_source_range_mr: true`. Filer JWT and gRPC TLS refs are refused by this
adapter because the unchanged smoke has no credential channel; `swctl` itself
supports their installation. Secret values are resolved only by `swctl`.

The adapter calls plan/install, records doctor's complete per-host rows, runs
the unchanged smoke, requires the extra native probe's `PROBE_EXACT`, then
uninstalls and checks for owned units/listeners. The intentional
`native.stats/UNKNOWN/NATIVE_STATS_ABSENT` and quiet-window NOT_PROVEN rows remain
visible in `swctl-doctor.json`; they are not relabelled as PASS. Other unexpected
doctor rows fail the run. Oracle or probe failure still runs teardown. A refused
or unconfirmed install never triggers deletion of a predecessor: retain its
journal for exact CLI reconciliation.

Outputs include the original `cloud-kv.jsonl`, `swctl-*.json`, the private frozen
`swctl-cluster.yaml` and `swctl-state/`, plus helper and postflight receipts.
An unconfirmed uninstall or dirty postflight fails the whole direct run. Data
paths remain governed by the CLI's stopped-snapshot preservation contract.
