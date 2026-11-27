# Draft v1.0.1 RF2/RF3 sync-quorum write-stall gate

Status: DRAFT. Merge only after v1.0.0. No real-product gate has run for this
scenario. Keep #189 as the independent primary-loss/co-located-guest row.
This packet authorizes no cloud run, seal, prior launch token or spending.

## Approved bar

The gate-owner ruling is recorded in message `6b0a1756d82cd3d6137887b9`.
All profiles use sync-quorum and read back EVERY ACKed write.

| Row | Pass line |
| --- | --- |
| RF3 one replica faulted | Longest ACK gap strictly < 5 s across the full fault window; zero guest I/O errors; faulted peer automatically Healthy within 120 s of process up |
| RF2 only peer absent | No ACK during the PRIMARY'S observed peer-down to peer-Healthy interval; any ACK = FAIL; guest errors in that interval expected and reported |
| RF2 outside that interval | Zero guest errors and < 5 s ACK gaps; the separately ruled first-ACK allowance is <= 10 s after Healthy, then the < 5 s progress rule applies |
| Both | Unchanged primary identity, automatic rejoin, every ACK read back, negative data control catches a dropped write, writer stopped and cleanup complete |

The host NVMe I/O timeout is 30 s, not the allowed stall. Exactly 5,000 ms is
FAIL. Write-operation latency, p50/p99/max and the full RF2 outage ACK gap are
reported; no extra operation-latency gate is invented. The RF2 10 s first-ACK
allowance is explicit and does not permit I/O errors after Healthy.

`write_budget_ms=5000`, `resume_budget_ms=10000`, and `budget_ref` are pinned
in the scenario. The policy CLI rejects different values, missing source,
non-finite numbers and test I/O overrides. They cannot loosen the gate at run
time. No product state machine, authority fact, HEAD writer or lease changes.

## Roles and sequence

```text
shared #189 preflight/build/start, sync-quorum throughout
 -> RF3: r1 + local loopback guest on node-2; r2 node-1; fault r3 node-3
 -> replica restored, process started, automatic rejoin while guest writes
 -> stop writer -> read every ACK -> dropped-write control -> timing judge
 -> restore on every exit -> collect before cleanup
repeat with fresh RF2 root: r1 + local guest node-2; fault r2 node-3
```

Use `scenarios/v3block/quorum-stall.yaml`. Setup and evidence collection are
shared YAML includes with #189, preserving one owner of documented standalone
argv, loopback NVMe connect settings, sync-quorum, inventory user/key pins and
cleanup. No external NVMe bind or cross-host guest. Full product/runner source
pins are required; use the eventual v1.0.1 candidate, not an old preview result.

Before the writer starts, the primary's peer view must show its peer Healthy.
Observe baseline for 10 s after at least 20 ACKs. Then power off node-3, dwell
10 s, restore it, start its holder, and require automatic Healthy plus at least
20 later ACKs and another 10 s of writes. One fresh RF3 and one fresh RF2 cell
per invocation; repetition count is not yet a release policy. The controller
stops on the first failed cell. Resource ceilings are 60 min per scenario plus
10 min cleanup; RF2's 180 s peer-wait ceiling is a harness limit, not an added
120 s product-recovery bar (that bar belongs to RF3).

## Measurement, with primary-owned RF2 boundaries

The guest writes unique sequential 4 KiB blocks using `dd oflag=direct,dsync`,
with one operation in flight and 10 ms pacing. A durable intent precedes I/O;
an ACK is journaled only after successful completion. An I/O failure keeps its
intent and measured error. Never overwrite an ACKed offset. A pending or
timed-out write cannot disappear from the denominator. One blocked request
may span the whole RF2 outage and complete after Healthy; no completion is
required while the peer is down. This is measured offered load, not saturation.

A concurrent observer samples the PRIMARY'S `/status` and `/status/peers`
every 50 ms while writes run. It records raw responses, same-host monotonic
time and boot id. The selected peer's initial Healthy -> non-Healthy -> Healthy
transition defines the RF2 interval; `degraded`, `catching_up`, `needs_rebuild`
and `unknown` all mean non-Healthy. Never use cloud dispatch, boot, a sleep,
or test-set phase markers as the RF2 interval. Every ACK is classified by its
COMPLETION time, including requests started before the down observation.
An observer error, missing peer/epoch/primary, a gap > 250 ms, missing coverage,
or a second down transition refuses the measurement rather than granting an
unknown interval. This is sampled diagnostic evidence at 50 ms resolution,
not a claim of an atomic status/I/O clock. Raw rows remain reviewable.

`fault` separately binds the actual node-3 cloud fault key. `recover` is marked
on the guest BEFORE dispatching the remote holder start; the scenario also
captures its actual positive systemd MainPID and `/proc/PID/stat`. Since host
monotonic clocks cannot be subtracted, RF3 uses this EARLIER start-request
mark to establish a conservative upper bound on process-up-to-Healthy time.
Passing <= 120 s from this mark implies <= 120 s from process up; it cannot
add grace. RF2 resume is measured directly from primary-observed Healthy to
first ACK, on the guest's same monotonic clock. The complete off/boot ACK gap
is reported even when RF2's approved interval is excluded from the 5 s bar.

Before readback, cooperative stop requires both the stopped receipt and the
actual writer PID exit. A final primary peer receipt closes sampling coverage.
The existing `r4.sh verify` reads EVERY ACK offset/SHA, then its existing
`drop-control` must reject an injected dropped ACKed block. Only then can the
real-block-device timing judge run. File-control devices and injected dd
commands are forbidden for qualification. The local ACK ledger is adequate
only because the primary/guest host stays up; primary/guest loss fails this
row and does not replace #189's off-host ledger proof.

## Entry, results and cleanup

Entry: `python3 quorum-stall-pack.py ASSET_ROOT`. Reuse the existing reviewed
cloud inventory/controller and three-node profile. Besides existing R4 assets,
manifest hashes must include `pins.json`, `swblock`, `cloud-pack.py`,
`quorum-stall.py`, `quorum-stall-policy.json`; that policy has exactly the three
pinned keys above. The controller separately pins the entry digest. The
existing pack runs RF3 then RF2 and returns the first failing exit.

Report RF/mode, policy source, attempted/ACKed/failed operations, expected RF2
interval errors, p50/p99/max operation latency, full max ACK gap, bounded ACK
gap, peer down/Healthy timestamps, RF3 conservative recovery upper bound,
RF2 Healthy-to-first-ACK, product/runner pins and binary SHA256s. Timing PASS
alone is not the gate: data verification, negative control and cleanup must
also pass. Always restore node-3 before collecting its journals. Preserve raw
peer/primary rows, phase markers, I/O records, intents/ACKs, PID receipts and
unit journals before cleanup; no TLS private keys are exported. Unexpected
primary identity or peer flaps fail. Never extend a deadline or discard a
product failure to obtain PASS.

## Validation and remaining work

Public seams: scenario/action execution, writer/judge CLI completion records,
primary status HTTP, and existing kernel-NVMe data oracle. Workstation controls
exercise strict 5 s boundary, RF3 errors, RF2 errors/ACK completion classification,
primary-owned interval unaffected by test markers, foreign peer view, 120 s
RF3 recovery, 10 s RF2 resume, pending writes, sample gaps, fixed policy, queued
RF2 writes, actual local HTTP observation during file I/O, manifest coverage,
and existing #189 regressions. File I/O is scaffolding, not kernel qualification.

Still required: two reader reviews, Linux controls and the WHOLE exact-head
local rehearsal (both RF cells, same product pin, replica process kill instead
of VM power-off), then a separately approved seal/budget/window/LAUNCH. Record
local namespace adaptations and all data readbacks. Current preview-code
failures remain product findings. Merge is held until v1.0.0; no resources or
cloud costs are authorized by this draft.
