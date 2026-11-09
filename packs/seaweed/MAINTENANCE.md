# Maintenance × existing process chaos

Opt-in extension of accepted Stage 2 plan d9109f2e. As-built references:
runner base e7c54b024b48b4011441e917d4d6bf81935d1415, mono
49daaba122ad6d49e3896e73092fcd628c81122b. Reuses the existing six-volume
Rust / Go master and filer deployment, mixed workload and ACK oracle.
No lab, hardware or production-readiness claim is made here.

Use the existing direct command with a pinned two-host onprem inventory,
--timeout 25m, and the normal pinned weed, weed-volume, native.tar, SSH
key and host-key configuration. Add to the normal config:

```json
{"maintenance_task":"volume.balance","cluster_chaos_cases":["C1-single-kill"],"replication":"001"}
```

| Task | Admitted existing cases | Execution scope |
|---|---|---|
| volume.balance | C1-single-kill, C2-rolling | Replicated integration |
| volume.fix.replication | C1-single-kill, C4-meta-restart | Replicated integration |
| ec_repair, ec_vacuum | C1-single-kill, C4-meta-restart | Real plugin detection/execution |

The 09:21Z license correction supersedes the earlier static EC refusal.
With no license input the real enterprise master supplies its default 25 TB,
one-month trial. The runner reads GetMasterConfiguration and records its
actual license id, validity, capacity and expiry in maintenance-master-license.json;
it never injects license_valid or reads the signing key. Actual invalid licenses
remain ENVIRONMENT/NOT_RUN only after verified cleanup. A zero-candidate
detector response is preserved as NO_EC_CANDIDATES / NOT_COVERED, never PASS.
C3/C6 remain NOT_RUN; C5 stays deferred.

EC adds fourteen run-owned SSD volume processes (seven per physical host),
one shard per process, alongside the existing replicated mixed-load cluster.
The existing C1 schedule targets the frozen holder of data shard 0; repair's
setup quarantines shard 13, so the planned loss is two distinct shards at most.
C4 keeps its existing master/filer schedule. No host-failure tolerance is claimed.
The actual erasure_coding worker serves repair and vacuum; both automatic
schedulers are disabled before seeding and exactly one bound proposal is
dispatched. Filer membership must be observed before config persistence.

Fixture-only configuration: the disposable EC master's -deletionRetention=-1s
disables the otherwise one-hour trial retention floor, so fresh acknowledged
tombstones can be vacuumed inside the existing timebox. This is recorded in
the command evidence; it changes neither licenses nor fault timing. Live and
deleted bytes are checked after encode and maintenance.

Observation choice: reuse the product's read-only GetMasterConfiguration,
VolumeList and VolumeEcShardsInfo RPCs (field numbers pinned to mono 49daaba1),
rather than change product code or infer serving state from HTTP/text output.
HTTP /status and volume.list omit serving EC generation. The cost is the
gRPC dependency and a bounded, maintenance-specific protobuf reader. Holder
RPCs sit inside stable process-owner observations and must agree with the
master heartbeat and local active-generation files. Missing, mixed or pending
state stays UNKNOWN. No transport lifecycle mechanism is introduced.

EC work coverage additionally joins the owned worker PID/start/boot, actual
job/request/attempt and VID/collection to one scratch shard file/inode whose
bytes increase across the existing fault bracket. Switching files or workers,
queued/no-op jobs, stale generations and foreign requests cannot earn coverage.
Vacuum also binds its staged VIF's encode identity; repair binds the proposal's
generation. Short jobs can run successfully and still be NOT_COVERED.

## Runtime and evidence

cloud_process.go binds the opt-in seam; maintenance_runtime.go and
maintenance_collection.go freeze the ACKed fixture and all its VID owners.
Balance uses an uneven run-owned collection with five replicated VIDs; only
the ACKed VID earns overlap. Fix replication quarantines one proven copy,
retaining the acknowledged source. Both invoke the real product shell, lock
and exact collection filter. Balance uses the product copy-rate option.
Fast/no-op tasks remain NOT_COVERED.

maintenance_observe.go uses an independent pinned SSH connection. Fault hooks
only snapshot/queue observations, adding no SSH wait to C1/C2/C4. Coverage
requires a live owned shell child, matching executable/helper/config identity,
actual VID and source/target route, and increasing byte progress on the same
operation across dispatch/GONE. The post-GONE observation must START after
GONE is received. Numeric bounds use the task host's clock and boot; victim
monotonic timestamps are never compared with another host's clock.

maintenance_collect.go checks exact bytes independently of task exit/refusal,
whole-collection mounted owners/readonly state, pending files and master
lookups. A settled task must leave a usable product lock: a fresh bounded
lock / own-VID volume.list / unlock must succeed. A clean refusal has its own
count and is never RECOVERED; early refusal is not work coverage. Unknown
effects, lost replies, unconfirmed copies or lost cleanup proof stay UNKNOWN.
Wrong bytes/lost ACKed data remain FAIL even if cleanup fails. The original
mixed-load live/deleted oracle is unchanged.

maintenance_lifecycle.go and embedded worker/maintenance_lifecycle.py arm
both exact-run target backstops before setup, record root inode/owner receipts,
preserve allowlisted diagnostics, stop exact owned units, and independently
check PIDs, TCP/Unix listeners and paths. Root deletion is no-follow,
receipt-bound and validates every root before deleting any. Unknown effects
retain protocol state and both backstops; they cannot produce a clean release.

Bounds: 1200 seconds work + 300 cleanup, at most 1500 total. Setup, concurrent
task/case, and final reads have maxima 240 / 600 / 240 seconds. Each step must
fit its full maximum before admission. Supplied inventory deadlines are
clamped, never extended. The existing inventory cleanup-reserve schema stays
unchanged; maintenance enforces its own 300-second reserve. No qualification
retries.

## Verification scope

Public-caller rehearsal: real loopback SSH with modeled two-host service
responses. Positives and negatives include queued/foreign/no-op work,
pre-GONE RPCs, wrong bytes, pending effects, lock failure, lost prepare and
failed cleanup proof. Local leaf controls execute real Python, a compiled
subprocess, HTTP server and filesystem deletion/refusal. That subprocess is
a protocol fixture, not a Seaweed product binary. These establish runner
controls, not real maintenance-task/fault coverage. A separately reviewed,
tokened product/lab run is still required. No M01/M02 access is authorized.
