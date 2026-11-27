# Draft v1.0.1 RF2/RF3 sync-quorum write-stall gate

Status: DRAFT, not executed on product binaries. Merge only after v1.0.0.
Keep runner #189 as the separate primary-loss/co-located-guest row. This row
faults only a replica while the primary and its local guest remain running.
No cloud launch, prior launch token, qualification result or cost approval is
created by this packet.

## Contract and pending owner decision

The v1.0.1 contract supports RF2 and RF3 with sync-quorum. RF3 continues ACKing
with one replica absent. RF2 must not ACK a new write while its other replica
is absent; after that replica returns, automatic catch-up must restore writes.
This packet observes those existing contracts and changes no product state
machine, authority rule, lease, HEAD writer or estimator.

Ruling received: the worst-case guest stall is **strictly less than 5 s per
fault**, measured as the longest gap between ACKed writes across the fault
window; **zero guest I/O errors**, and every ACKed write read back. The host
NVMe I/O timeout is 30 s; it is not the permitted stall. A 5,000 ms gap is a
FAIL, not an inclusive limit. No separate write-latency or recovery bound was
specified; report those observations without inventing another gate.

The gate definition still needs the designated gate owner's read. In
particular, RF2 cannot ACK while its only peer is absent, and the draft's
10 s replica-off dwell already exceeds the 5 s ACK-gap bar before reboot and
catch-up. This is a contract conflict to resolve, not a sample to exclude.
The draft's RF2 outage exclusion and allowance for guest refusal are NOT an
approved gate. Do not warm the fixture, shorten the fault, acknowledge on one
copy, or hide the absent-peer interval to manufacture PASS.

Current implementation remains a non-executable-by-default draft with empty
`write_budget_ms`, `resume_budget_ms`, and `budget_ref`. Its proposed judge
has not yet been adapted to this ruling: it allows provisional RF2 outage
exclusion/refusals and additional timing inputs. It must not be used for a
counted result. Next change, after the gate owner's RF2 disposition: enforce
the approved strict full-window ACK-gap predicate and zero guest errors;
remove unapproved exclusions and timing gates. Test literals such as 500 ms
are controls, not release limits.

## Sequence and roles

```text
shared #189 preflight/build/start (sync-quorum throughout)
  -> RF3: primary r1 + guest node-2; r2 node-1; fault r3 node-3
  -> restore replica; primary peer view Healthy; keep writing
  -> stop writer; read every ACK; dropped-write control; judge timing
  -> restore on every exit; collect before cleanup
repeat with a fresh RF2 root: primary r1 node-2; fault r2 node-3
```

Use `scenarios/v3block/quorum-stall.yaml`. The three-node profile, inventory
SSH users, host-key pins, exact documented standalone argv, loopback NVMe
settings, sync-quorum flags and cleanup come from the shared R4 includes.
No cross-host guest or external NVMe bind is introduced. Product source and
runner source require full 40-hex pins; pin the eventual v1.0.1 candidate,
not an earlier preview run. Baseline and post-return observation are 10 s
each, after at least 20 ACKs. Replica-off dwell is 10 s by default; VM
power API/boot time is also measured. One RF3 and one RF2 cell per invocation;
repeat count is not yet a release policy.

The existing cloud pack transports approved inventory and executes RF3 then
RF2, stopping at the first failure. The new entry is
`python3 quorum-stall-pack.py ASSET_ROOT`. In addition to the existing R4
assets, the manifest must hash `cloud-pack.py`, `quorum-stall.py`, and
`quorum-stall-policy.json` (exactly the three required policy keys above).
The controller separately pins the entry digest. This draft does not provision
or authorize a paid run. Each scenario is bounded to 60 min plus 10 min cleanup.

## Writer and observation

The guest on node-2 issues sequential unique 4 KiB writes using
`dd oflag=direct,dsync`, with one operation in flight and 10 ms pacing. Each
attempt gets a durable intent before I/O, then an ACK only on successful I/O
completion; failures retain their intent and measured result. It never
overwrites an ACKed block. A timed-out or unfinished operation cannot be
removed from the denominator or yield PASS. This is an observed offered load,
not a claim to saturate throughput. Unexpected primary/guest failure fails
the row; this local ledger does not replace #189's off-host primary-loss ledger.

All write/phase timestamps use the primary guest's monotonic clock and boot
identity. `fault-pending` precedes cloud dispatch; `fault` follows confirmed
power-off and records the runner's node-3 fault key. RF2 refusal is allowed
from fault-pending until peer recovery, since power-off can precede the API
return. RF2's no-new-ACK assertion uses attempts begun after confirmed off
and before return. `recover` is immediately before restarting the holder,
after the VM boots; `after` requires the returning peer Healthy at epoch 1
in the unchanged primary's `/status/peers`. The full off/boot gap is retained.
The scenario never restarts a failed guest writer to manufacture progress.

Before any data readback, the writer cooperatively stops and both its stopped
receipt and actual PID exit are required. The existing `r4.sh verify` reads
EVERY ACKed offset/SHA, followed by its existing dropped-write negative
control. Only then does the timing judge run with a real-block-device guard.
Regular-file controls and injected dd commands cannot count as qualification.

## Pass/fail and evidence

| Cell | Required outcome |
| --- | --- |
| RF3 one replica off | Zero guest I/O errors; full fault-window ACK gap strictly < 5 s |
| RF2 one replica off | No single-copy ACK; no outage exemption approved; gate owner must resolve the 10 s fault vs < 5 s bar |
| Either replica returns | Primary unchanged; peer Healthy at epoch 1; automatic return and continued ACK flow; report recovery time |
| Any cell | Every ACK read back, no unmatched attempt, actual writer ended, no lost data |
| Any exit | Replica restored, journals/argv/timing/ledger collected before cleanup, no owned leftovers |

Report attempts/ACKs/refusals, p50/p99/max write latency, global max ACK gap,
bounded ACK gap, recovery-to-first-ACK, policy value/source, RF, mode, exact
product/runner pins and binary SHA256s. A timing PASS alone is not the gate:
data verification, negative control and cleanup must also pass. Preserve
raw JSONL, stopped receipt, primary/peer snapshots and unit journals on failure.
A preview-code rejoin or durability failure remains a product finding;
never lengthen a deadline or remove an assertion to relabel it PASS.

## Preflight and validation status

Public seams: runner scenario parsing/action dispatch; the CLI writer and
judge over actual completion records; the existing kernel-NVMe readback;
primary status/peer HTTP receipts. No alternate authority or data oracle.
Windows controls cover measured slow writes, long ACK gaps, unfinished writes,
RF2 single-copy ACK refusal, recovery delay, finite sourced budgets, actual
file-control write failure without an ACK, all executed asset pins, optimized
Python refusal and policy forwarding through both RF cells. Scenario tests pin
the shared lifecycle and replica-only fault. These controls do not qualify a
Linux block device, actual RF2/RF3 rejoin, or a cloud fault.

Still required: gate-owner RF2 disposition and judge alignment to the strict
5 s / zero-error ruling, reviewed merged runner pin after v1.0.0,
exact v1.0.1 product pin, no-resource preflight, then the WHOLE local rehearsal
(both cells, same product pin, replica process kill instead of VM power-off)
with the documented standalone settings. Record approved local namespace
adaptations and all ACK readbacks. Only after a passing rehearsal may a new
seal/budget/window/LAUNCH be requested. A cloud attempt needs fresh empty-RG
proof, explicit budget and abort thresholds; none are inferred here.

Current workstation verification: 12 new Python controls PASS; 51 existing R4
Python controls PASS with six Linux-only skips; affected R4/quorum Go tests
PASS; five removal mutations are caught. Full Windows `go test ./...` is NOT
green: untouched NVMe JSON golden comparisons see CRLF working-tree bytes,
and an existing seaweed CLI test cannot find `python3` on Windows. These are
recorded limitations, not a full Linux-suite verdict.
