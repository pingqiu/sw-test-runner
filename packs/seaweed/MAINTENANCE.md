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
| ec_repair, ec_vacuum | C1-single-kill, C4-meta-restart | ENVIRONMENT / NOT_RUN |

EC emits EC_WORKER_LICENSE_UNAVAILABLE and exits 78 before artifact checks
or SSH. No approved license reference/binding exists under the 2026-11-09
ruling. Dormant EC prototypes are neither enabled nor qualified. Never inject
license_valid; no PASS or product finding is earned. C3/C6 remain NOT_RUN;
C5 stays deferred.

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
