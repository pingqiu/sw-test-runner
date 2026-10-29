# S0 Run Safety: Partial Implementation And Interface Draft

Status: **not S0-complete; not launch-ready; blocked on scope repricing**.
Current S0a scope, NEED-1..5 correction and budget stop: see s0a-needs.md.
Base: c646219321adef0c4b2ed8ce6733d0da1d3b092e.
Request: Claude01 6ae3b20b9c9fd1ba9ca5271f; accepted port plan
`V:/share/rdma/codex01-testrunner-port-plan-20261029/PLAN.md`.

## Implemented

The in-process Engine now fails the overall run when always-cleanup fails,
attempts all cleanup actions, and does not honor `ignore_error` in always phases.
An action panic becomes FAIL so always phases can run. Cancelled actions are not
dispatched. Parallel actions receive isolated variable snapshots; outputs merge
after all actions finish. Conflicting values fail without publishing that key.
Non-conflicting partial outputs survive failure for cleanup.

`ActionContext.Safety` is a shared typed `RunScope`, not a variable-map value.
The scope provides the following **draft** interface for the S1/S3 local spikes:

```text
NewRunScope(parent, admission, ownedBackend, verifyAdmission)
  Start(JobSpec{Node, Executable, SHA256, Argv}) -> opaque JobHandle
  Stop(ctx, handle)                           -> settle proof or error
  ApplyFault(resourceKey, apply, restore, verifyRestored)
  ClearFault(ctx, resourceKey)
  Close()                                    -> settle all or error
```

Admission binds run/workload/current request/manifest and absolute work/hard
deadlines, reserving cleanup inside the hard deadline. The verifier is a required
trusted adapter, not a permissive YAML flag. Neither a default verifier nor an
historical request allowlist is supplied. KV and S3 positives and a cross-workload
negative are tested using a local fake verifier.

Start records POSSIBLE before contacting the backend, enrolls inert, checks the
returned full identity, then authorizes. A lost enrollment reply retains its
opaque handle for final settlement. Failed authorization retains custody too.
The backend must check identity atomically, not signal a numeric PID after a
separate check. Stop cannot accept a handle from another scope.

Fault restoration is recorded before apply. Failed/partial apply, failed restore,
or failed verification cannot erase the obligation. Conflicting resource keys
are refused. Clear is reverified on repeated calls and on final close. Closing
admission, restoring faults and settling jobs do not consult queue health. Each
cleanup error or panic is retained while other obligations are attempted.

`infra.ShellArgv`/`Node.RunArgv` quote all positional arguments, including an empty
one. A composed local-shell test covers empty/30/quote/dollar/newline arguments.
It is not a real SSH or hardware test and does not repair the old Python STOP.

## What Is Not Implemented

**The only OwnedJobBackend implementation is a test fake.** No CLI flag, YAML
action, HTTP endpoint, queue entry or native node constructs a RunScope yet.
In-process cancellation/deadline tests do not prove survival of controller death.
The backend contract requires independent agent-side enforcement and proof;
declaring that interface is not implementing its guarantees. Fake-generation
controls test the contract, not kernel pidfd signaling or process-group custody.

Still required before S0 can be accepted:

1. Existing agent/node lifecycle: an owned-child/identity-safe backend with inert
   enrollment, possible-issuer settlement, descendants, absolute deadline and
   controller-loss handling. No new broker, daemon or ready/go file protocol.
2. Agent/coordinator protocol: retain a typed run scope across phase requests,
   carry bound run identity/deadlines, and settle independently of request loss.
   `Agent.executePhase` currently creates a fresh ActionContext per request;
   `PhaseRequest` has no run/admission/deadline field. The parallel coordinator
   path also marshals shared vars while merging other replies; the engine fix
   in this head does not fix that separate distributed path.
3. Distributed cleanup: coordinator always phases currently reuse cancelled ctx;
   their errors do not affect overall PASS. Agent executes its own action loop,
   not Engine.Run. Those paths need the same tested settle-all/failure semantics.
4. TierChaos integration: current actions still use mutable `__cleanup_*` shell
   strings. They do not call RunScope, and their existing fault implementation
   is not qualified by the fake restore controls. Add per-resource ownership,
   actual effect/restoration witnesses and bounded ENOSPC/partition adapters.
5. Queue authority and deployed-byte readback: the current submit/worker .env
   path has request metadata but no bound owner GO, workload manifest or absolute
   deadline. Read both scripts; do not infer this contract from submission alone.
   A real adapter must verify deployed executable/config/worker bytes and record
   YAML/runner/product/policy identities in the bundle.
6. Race-detector coverage with an appropriate local toolchain. Windows Go 1.25.6
   has no cgo compiler in the current environment; the available WSL Go is 1.18.1,
   below this module's Go 1.25 requirement. Do not substitute a lab run or claim
   that repeated ordinary tests are race-detector evidence.

## Repricing Stop

The original S0 estimate was 450-750 changed production Go lines. The scoped
engine/interface work is only the first part. Source inspection exposed two
independent execution paths and per-request agent state, not a reusable typed
background-job lifecycle. I underestimated that integration in the port plan.

Proposed revised S0 total: **1,100-1,650 production Go lines**, including this
partial head, with tests separate. Remaining approximate allocations are
300-450 agent-owned backend, 250-400 agent/coordinator lifecycle and cleanup,
and 200-350 TierChaos plus authority/deployment binding. These are estimates,
not a new authorization or a reason to compress safety code.

Stop further S0 feature implementation here for Claude01's ruling: accept the
reprice or reduce/resequence the slice explicitly. This partial head can be
reviewed by Claude02 for engine correctness and interface feasibility. It is
not a request to ACCEPT full S0, launch S1/S3, or retire any old harness.
S1/S3 spikes remain non-pushable and should report constraints against this
draft; they must not bypass the missing backend with raw PID commands.

## Execution Scope

Only scoped Go tests, local shell argv verification and local compile checks.
No SSH, lab jobs, cloud calls, privileged fault injection, artifact deployment,
product changes, reference moves in other repositories, or harness retirement.
Real ENOSPC remains separately owner-approved only on the run-created ext4 image
under M02 `/data/nvme/<run_root>/enospc.img`, at most 16 GiB. Partition evidence
is TCP control/HTTP/RC-control; RoCE data disruption remains NOT_PROVEN.
