# Read-only restart load: slice 2

Base `78eaadf9b8fe` (accepted Draft #25). This slice is local-only and adds no
registered scenario, host action or claim of application recovery.

The existing `kv_smoke.py` CLI now accepts exact `--stage` and `--read-path`
arguments produced by `seaweed.ReadLoadArgs`. The accepted
`soak_native_worker.py` and `soak_oracle.py` leaves are unchanged. No native
bridge, old controller, scheduler or transport implementation is added.

| Stage | Path | Behavior |
|---|---|---|
| seed | dc | Existing settled SET/metadata/ACK/exact-GET smoke, 1–16 keys |
| load | dc or http | GET only, fixed 120 s, serial operations with 250 ms pacing |
| recovery | dc or http | One exact named GET per pre-seeded key |

All stages share the same joined-inventory root and deterministic key/seed set.
No overwrite or new SET is allowed during load or recovery. A wrong byte or an
unknown native completion is immediately fatal; unknown DMA buffers retain the
accepted leaf's ownership through process teardown. Completed unsuccessful GETs
in the load are bounded observations, never automatic expected-error credit.
Recovery requires every GET exact. Terminal stdout is bounded and captured via
the existing owned-job output route; there is no new authority/receipt type.
`first_exact` and `last_exact` are completion times, not submission times; the
scenario must use those boundaries when proving reads around its fault.

`StartApprovedPackTask` starts an independently pinned finite job without a
blocking Wait; `WaitApprovedPackTask` later validates its exact terminal output
and exit. Both use the original scope, live authority and existing custody.
The task waiter refuses service-generation handles before backend Wait. A pack
must retain the original volume handle **without starting a service Wait
watcher**: restart owns that gate for Kill and old-generation settlement. A
blocked task Wait operates on a different handle and cannot seize the victim
gate. Tests exercise that composition through the actual approved Engine path.

`CheckReadTaskOutput` checks the captured seed ACK/digest/FID rows, named run/key
namespace, duration, completed-failure ownership, and DC/HTTP path counters. It
does not label any failure expected: slice 3 must bind every completed failure
to the exact witnessed outage, join new-generation readiness and recovery, and
require successful reads around that episode. Stats and stdout are observations
of the protected worker, not independent server-write accounting.

Still pending: pack phase/scenario binding, retained service handles, replacement
readiness under the new generation, outage attribution, and M01/M02 end-to-end
DC proof. Resource-soak and native mid-transfer exposure are outside this first
case. Keep all Python leaves and historical controllers until reviewed parity
and Ping cutover; this slice retires no file.
