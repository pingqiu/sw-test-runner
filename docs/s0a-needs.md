# S0a NEED-1 Through NEED-5: Corrective Head And Cut Line

The 900-line budget ruling below is historical. The final 1,150-line ruling
and subsequent concrete-draft stop are recorded in s0a-final-cut-line.md.

Owner ruling 6ae3b7bd9c9fd1ba9ca5275b: S0a hard 900 production Go lines;
NEED-1/2/3 are correctness, Wait and witnessed Kill are mandatory. NEED-6/7
are not implemented here and stay with the packs unless separately repriced.
This supersedes the earlier full-S0 reprice in s0-run-safety.md. Source starts
at 11ea436; accepted baseline remains c6462193. Worktree is Codex01's private
`C:/work/rdma/worktrees/sw-test-runner-s0`, not the shared runner checkout.

## Corrected Scope

- NEED-1: Start registers POSSIBLE under mu, releases it before Enroll/Authorize,
  and rechecks closure before authorization. Entry readiness fences settlement.
  Two gated fake enrollments must arrive together, not serialize on scope mu.
- NEED-2: apply/restore/verify execute outside scope mu. Per-entry ready/gate
  channels serialize only that resource and honor the waiting context. A fault
  callback can Start a job; its restoration can Stop it. A callback must not
  synchronously clear its own still-applying fault. Close closes admission first.
- NEED-3: Stop removes the pending entry only after absence proof. Wait can also
  retire it with a fully bound terminal receipt. Close snapshots only pending
  entries; a concurrent Stop is idempotent through the per-job gate. The slowed
  1,000-one-shot control leaves zero pending jobs and zero extra settle RPCs.
  Log-path reservations remain to prevent evidence overwrite; no historical job
  RPCs are replayed. Unknown Stop/Wait remains pending.
- NEED-4: Wait returns ExitInfo (identity, native code/signal, stdout/stderr paths, Settled).
  Missing absence proof, wrong identity or wrong paths cannot retire custody.
  A nonzero exit is returned as such, not rewritten to success; the pack grades
  its meaning. A signal keeps Code=-1 and its signal number; it is not fabricated
  as an ordinary exit. Each job defaults to unique paths under RunAdmission.OutputDir;
  explicit paths must stay there and cannot be reused, even after settlement.
  Wait is cancelled before final Stop so it cannot occupy the gate indefinitely.
- NEED-5: Kill returns identity/AliveBefore/Signal/AppliedAt, rejects unknown
  enrollment, dead or replaced victims, and never treats the signal as absence.
  Wait or Stop must still prove exit/descendant settlement. This is a backend
  contract validated with fakes, not evidence of a real kernel signal.

## Updated Interface

```go
scope.Start(JobSpec) (JobHandle, error)
scope.Stop(context.Context, JobHandle) error
scope.Wait(context.Context, JobHandle) (ExitInfo, error)
scope.Kill(context.Context, JobHandle) (KillWitness, error)
```

JobSpec adds StdoutPath/StderrPath (optional, defaulting to unique run paths).
RunAdmission adds required OutputDir (absolute, normalized, not filesystem root).
OwnedJobBackend adds Wait and Kill with the above return types. It must create
logs without following foreign symlinks or overwriting another run's evidence;
lexical scope checks are not a substitute for concrete filesystem ownership.
Queue/GO binding remains S0b; the current verifier is not real launch authority.

Only a test fake implements this backend. NEED-1/2/3 are corrected in the actual
RunScope, but NEED-4/5's **concrete SSH execution remains open**, not closed by
their fake receipts. There is no agent endpoint, background service, native
helper, pack state registry or in-process goroutine supervisor hidden here.
No S0a acceptance, real controller-death proof, launch or retirement is claimed.

## Budget Stop

Final baseline-diff production is **503 added / 6 removed** versus c6462193:
engine 71, registry 4, argv 31, RunScope 397. This correction is +220/-64 Go
production relative to 11ea436. Conservatively charging the original 347 plus
all 220 new additions, without credit for the 64 removed lines, gives **567
charged**, leaving **333** of 900. Tests and docs are separate, not minified.

The remaining mandatory implementation no longer fits my responsible estimate:

| Remaining S0a item | Estimated Go |
|---|---:|
| SSH inert-entry/session lifecycle and loss handling | 85-100 |
| Owned-child/pidfd and descendant settlement, including late/unknown enrollment | 95-115 |
| Actual Wait status and owned log collection | 45-55 |
| Actual Kill alive-at-application witness | 40-50 |
| Deployed executable hash / concrete transport binding | 35-40 |
| TierChaos bounded apply/effect/restore/absence integration | 150-180 |
| Controller work/cleanup envelope integration | 20-30 |
| **Remaining estimate** | **470-570** |
| **Complete S0a projection, conservative charged ledger 567** | **1,037-1,137** |

**Cut line: 333 remain on the conservative ledger, but my remaining estimate is
470-570: at least +137 beyond the hard 900. Even the final-diff ledger projects
973-1,073, +73 over at its low end. These are estimates, not measured native
implementation or proof that no smaller safe design exists.** I stopped before
implementing that excess. The
existing Node.Run/StreamRun only exposes command output/status and SSH signals;
neither provides an inert child, atomic generation-safe signal, independently
owned descendants or a late-issuer fence. `ps; kill PID`, shell group liveness,
or a successful SSH signal is not an implementation of those guarantees.

I underestimated that concrete transport work again when pricing the earlier
200-230 SSH adapter. A real one-shot custodian and its SSH binding must both be
counted; they cannot be disguised as a test helper, Python asset or uncounted
shell/native framework. No safety compression or weaker proof is proposed.

Owner decision needed at this cut line: explicitly revise/resequence the
remaining S0a work or supply a reviewed existing owned-process primitive to
reuse. The old 1,100-1,650 full-S0 request remains withdrawn. S0b stays unapproved.
Claude02 can independently review this Scope correction while that decision is
pending. No more production features are added under the current budget.

## Verification Scope

Only local fake-node Go tests, local shell printf and local compilation.
The owner-provided NEEDS.md SHA256 is
228cff5519cb0f4481b1f8db05cc923063eb3f6ec5171383808de41e83513014;
its retained 3/3 old-draft observations are preserved. Our new controls and
private mutations distinguish the corrected Scope, not a native SSH backend.
Race execution remains unavailable with the current local cgo toolchain.
