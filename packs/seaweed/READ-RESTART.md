# One volume restart under read-only KV load

This slice is stacked on `a7088a3` (#26), using the accepted generation provider
from #25. It adds no native code, controller, transport, readiness poller, or
receipt type. LC6 (restart/error ownership), LC4 (generation change), and LC5
(settlement before cleanup) are the affected lifecycle views. The reuse choice
is the existing runner custody/provider and accepted worker/oracle leaves;
a separate fault controller or cross-host clock protocol would duplicate them.

The review-fix ceiling is 790 Go additions across all three slices and #26 fixes.
The approved cut is applied: **DC spans the restart. HTTP is a pre-restart and
post-recovery exact-byte control only. HTTP outage coverage is DEFERRED.**
Write-under-fault, unknown-write quarantine, native bridge, VF/ENOSPC faults,
rotation qualification, and resource flatness remain outside this first case.

## Pinned scenario shape

Use the existing `ProfileArgs`, `ReadLoadArgs`, and `MarshalPlan` producers and
the same five-phase S0b scenario. Replace only the workload action with
`seaweed_read_restart` (TierChaos). Keep cleanup `always`, no ignored errors,
retries, parallel phases or caller parameters. The independently approved
policy must include:

- Original volume service and a single `GenerationRestarts[episode]` pairing it
  with a replacement job. Replacement argv is the same `ProfileArgs` volume
  command plus `--replacement-volume`; custody, consent and authority remain
  checked by the existing provider.
- Seed job: `ReadLoadArgs(..., "seed", "dc")`.
- DC load job: `ReadLoadArgs(..., "load", "dc")`.
- DC recovery and HTTP control jobs: corresponding `"recovery"` commands.
- `Plan.Restart` with the exact episode, DC load, DC recovery and HTTP control
  labels. The M01/M02 profile and original ready probes stay in the plan.

The new helper bytes must be installed and independently read back under a new
pinned release before a runtime GO. This PR provides no deployment or launch GO.
No historical binary/worker hashes can be reused for modified helper bytes.

## Execution and proof

1. Settle seed SET/GETs, including ACK/FID and exact counter snapshots.
2. Run HTTP exact-byte control, then start one bounded DC task (GETs only).
3. At 30 seconds, call the existing one-shot generation provider. It binds
   pre-Kill intent, live-before witness and matching old-generation exit before
   starting the replacement. No background Wait occupies the live victim gate.
4. Reuse both ready probes. M02 must identify the exact replacement PID/start/
   boot plus independently read-back product hash. The replacement preserves
   the owned volume directory and refuses a still-live old generation.
5. After binding the old-generation exit intent, the M01 runner captures P with
   Linux CLOCK_MONOTONIC immediately before the backend Kill dispatch. It stores
   P in the existing generation Kill receipt. Failure to read this clock
   refuses the Kill; a remote wall timestamp is never substituted. Python GET
   outcomes use the same M01 kernel clock and admitted boot. M01 ready-probe
   entry still records its own monotonic marker, as an observation only.
6. Settle the fixed 120-second DC task. Each outcome/failure must join its exact
   indexed key, op ID and monotonic interval inside the load bounds. Errors may
   form only one contiguous window, from preceding exact success to next exact
   success, entirely inside [P,P+60 s]. Any pre-P, late, second or unbounded
   window fails. Zero errors is allowed. No ready-entry or data-fitted anchor
   can replace the generation receipt's P.
7. Only after load settlement and successful attribution, issue named DC and
   HTTP recovery GETs. Thus every credited window closes before recovery GETs.
   Any wrong byte is fatal in the unchanged worker/oracle; it never becomes an
   outage observation. Existing custody/cleanup closes all generations.

The existing profile ready probe remains bounded by 45 seconds/original work.
The 60-second recovery claim is ONLY the DC window close <= P+60 s.
Named recovery GETs at load end check byte correctness; they have no additional
Kill+60 timing gate, and remain bounded by the original work/hard envelope.
A zero-error fast restart is allowed.
Load lasts 120 seconds with <=1024 outcomes and <=256 failure observations;
unknown completion remains fatal. Original action/work/hard deadlines bound
all dispatch and settlement. No resource or hardware qualification is claimed
by CPU controls.

## CPU review evidence

Controls use the real strict entry, policy/GO checks, Engine, custody, output
checker and profile CLI, replacing SSH/native hardware at explicit fake
boundaries. The positive path checks sequence and task settlement. Negatives
cover stale/missing/wrong-product readiness, wrong Kill identity, wrong recovery,
second/overlong/pre-ack windows, missing or reordered outcomes and wrong keys.
A live-victim Wait is refused by the transport fake, so the positive Engine case
also exercises the required restart ordering. Actual CLI controls preserve
seed data through replacement, refuse live/foreign-boot old generations, require
owned TCP listeners and exact-generation CM/DC log witnesses.

## Pre-Kill anchor correction (r10)

R10's real 402-GET trace had three misses and returned to exact reads before the
ready-entry marker. Its sealed FAIL remains unchanged. The trace fixture keeps
all raw outcomes/counters; P was NOT recorded in that run. Its replay-only P is
recorded load start plus the independently fixed 30s fault schedule, not a
measured r10 dispatch timestamp or a value chosen from failed GET positions.
A separate actual provider/caller control proves new receipt capture between
intent binding and backend Kill entry; a late ready observation cannot change P.

Choices: reuse the Linux monotonic clock in the existing custody boundary;
a Python probe adds dispatch latency, and Go process-relative elapsed time does
not share Python's absolute clock coordinate. Non-Linux generation restart
refuses instead of inventing an offset. This reuses our runner machinery;
no new Mooncake/3FS/NIXL protocol or separate timing framework is introduced.
The change concerns LC6 timing/error ownership and LC4 generation transition;
LC5 settlement and the original work/hard deadlines remain unchanged.
The runner boot must match the admitted M01 observation before seed/load; a
foreign controller clock refuses attribution rather than mixing clock domains.

The requested entire-window lower bound also constrains the preceding successful
GET. If that last good sample predates P even though every miss follows P, the
case still refuses. This conservative sampling limit is not normalized away;
only the independently reviewed contract may change it, never a fitted P.
