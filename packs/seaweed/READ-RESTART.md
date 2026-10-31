# One volume restart under read-only KV load

This slice is stacked on `a7088a3` (#26), using the accepted generation provider
from #25. It adds no native code, controller, transport, readiness poller, or
receipt type. LC6 (restart/error ownership), LC4 (generation change), and LC5
(settlement before cleanup) are the affected lifecycle views. The reuse choice
is the existing runner custody/provider and accepted worker/oracle leaves;
a separate fault controller or cross-host clock protocol would duplicate them.

The final 750-Go-addition cap includes all three slices and the #26 fixes.
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
5. M01 ready-probe entry records client-local monotonic time after the provider
   has acknowledged Kill. The DC task records every completed GET's ID, indexed
   key, success/failure and local monotonic start/end. Both records originate
   from pinned owned M01 jobs on the admitted boot. M02 wall timestamps are
   never used for outage credit.
6. Settle the fixed 120-second DC task. The assessor joins all outcomes to the
   existing failure IDs and counters. Errors must form one contiguous window,
   bounded by a preceding success and the next success, lasting <=60 seconds.
   Its opening must follow the M01 post-Kill marker. Missing bounds, a second
   window, reordered/foreign outcomes and errors before the marker fail.
   The marker is a conservative later bound on Kill acknowledgement; early
   errors receive no credit. This can refuse an otherwise recoverable fault,
   which must be reported rather than normalized.
7. Only after load settlement and successful attribution, issue named DC and
   HTTP recovery GETs. Thus every credited window closes before recovery GETs.
   Any wrong byte is fatal in the unchanged worker/oracle; it never becomes an
   outage observation. Existing custody/cleanup closes all generations.

The existing profile ready probe is bounded by 45 seconds/original work;
replacement readiness must finish within the 60-second recovery budget.
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
