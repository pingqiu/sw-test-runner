# S0b-min PR3: existing worker admission and retained cleanup

This stacks on PR2 `4b61546e1f40` and S0a `eef35ec2`. It implements the
accepted S0b-min proposal and amendment `0766b2d4`: one controller, the existing
queue/claim/flock/result transitions, and one S0a RunScope. It adds no broker,
lease owner, deployment installer or agent lifecycle. Local controls qualify
composition and refusal; M01/M02 and Azure deployment gates remain separate.

Responsibility views: LC2-C authority/host identity, LC4-B revocation,
LC5-A/E ownership and bounded settlement, LC6-B/D boot change and concurrent
dispatch. Reuse S0a rather than introducing another transport/scheduler.

## Entry and trust

```text
existing worker claims <request_id>.env and holds existing flock
  service-pinned adapter: approved-run --request <claimed> --policy <bootstrap>
    strict JSON snapshots + independent protected GO + existing live lease
    protected deployment readback over pinned SSH
    one ApprovedRun / ActionContext.Safety
      exact job admission -> inert enrollment -> fresh authorization
      close work -> settle ALL possible issuers -> registered restore -> proof
    existing result bundle and done/failed transition
```

The worker enters this path when service configuration supplies
`TESTOPS_APPROVED_ADAPTER`, `TESTOPS_APPROVED_ADAPTER_SHA256` and
`TESTOPS_APPROVED_POLICY`. Partial configuration refuses. It never sources the
claimed request or falls back to RUN_SCRIPT on an approved-path refusal.
The adapter is a protected service/root-owned executable with protected,
non-symlink ancestors. Python 3 checks its stable identity/metadata/hash, then
executes the verified open descriptor. Access time is not a stability field.
This does not enable S0b automatically for legacy worker deployments.

The `.env` extension is retained for the existing queue, but its S0b body is
strict versioned JSON, not shell. Files are bounded to 1 MiB; duplicate,
unknown or differently cased fields, trailing data, noncanonical paths,
symlinks, ownership changes and unstable captures refuse. The worker service
owns the claimed request; root or the service owns protected policy/GO files
and ancestors, without group/world write. These are installation prerequisites,
not permissions this adapter changes. Keep service authority separate from
workload-writable storage, and use trusted base-OS tools/startup environment
as required by PR2. Do not put credentials in job argv or approval records.

`ReadTrustedPolicy(path, serviceUID)` captures an opaque bootstrap. Its
`s0b-bootstrap-v1` schema is `PolicyBootstrap` in approved_run.go: independent
GO and run-policy FilePins, exact running/ledger/cancel/results directories,
and the service-owned node-to-key-file map. Request data cannot select it.
The GO pin is outside run-policy bytes to avoid circular GO/policy digests.

`s0b-request-v1` contains the exact `RunBinding`: request/run IDs, kv|s3,
http|rdma, inventory/scenario/policy/artifact FilePins, explicit node/endpoint/
user/public-key/root set, existing lease ID/owner/resource, work/hard deadlines
and cleanup reserve. The independently protected `s0b-go-v1` export matches
the entire binding AND exact request-byte digest. A source Huly ID alone is
not authorization. An operator must supply approved export bytes and revoke
them via the protected cancellation path or changed/unreadable authority.
Nothing here generates GO exports or auto-approves a queue submission.

`ReadLiveLease` ports the adopted existing-record checks: exact lower-case
32-hex ID and owner; finite, nonfuture acquire/renew clocks; latest renewal or
acquire, cadence <=600 s and TTL <1800 s; release absence before and after.
It acquires/renews nothing. The acquire `host` is the broker, not an allowed
host set. The independently approved lab profile must name the existing
`/mnt/smb/work/share/testops/locks/rdma-lab.lock.ledger` resource and explicit
M01/M02 hosts. Azure requires its own separately approved existing authority
and ledger path. Flock alone is never authority.

Inventory and artifacts use `ApprovedInventory` / `ApprovedArtifacts` versioned
JSON. Their exact bytes are pinned; inventory hosts match GO, and the complete
artifact deployment set matches run-policy. PR2 validates root-owned protected
remote files, roles, native set, boot, UID and metadata. PR3 also binds the
custodian runner path and every job executable/hash to that qualified set.
RDMA requires a nonempty native set. Initial readback freezes observed boot;
fresh Enroll and Authorize each repeat authority and deployment/boot checks.

## Execution and cleanup

`OpenApprovedRun(ctx, claimedPath, trustedPolicy, runtime)` defaults a nil runtime
to actual pinned SSH/PR2 readback. The optional runtime is a compiled dependency
injection seam for CPU controls; no JSON, CLI flag or queue field enables it.
Execute uses the already parsed pinned scenario, installs the same Safety in
every phase, sets mutating consent from independent GO, propagates
watch cancellation, and refuses raw NodeRunner/automatic target/coordinator
execution. The approved CLI registers the shared Seaweed pack plus `approved_job` (one
approved job label, exact argv); it does not register legacy build/shell packs.
Future packs supply trusted typed handlers to RunApprovedRequest. They must
honor context and use Safety for effects, not start alternate executors.

The pre-created remote output root has the same approved absolute path on each
host and must satisfy S0a's private workload-owned 0700 directory requirement.
The adapter neither creates it through unowned shell nor changes its ownership.

Scenarios must be already expanded: includes, automatic cluster/target/agent
lifecycle, local nodes and foreign topology refuse. Native script bodies are
pinned/scanned inputs (at most 128 bodies and 8 MiB in total). Interpreter jobs support a protected read-back script
path (optionally Python `-I`) whose digest also names a scanned source pin;
unlisted bodies, eval/module/stdin forms refuse pending a reviewed pack.
The existing insecure-option/nested-SSH guard also scans pinned argv.

Before a persistent TierChaos apply, independent GO must explicitly consent
and grant exact kind/node/resource, Before/During ownership state, apply and
restore executable/hash/argv, original deadline and boot-qualified plan.
The scope privately binds the obligation to a custody job ID before dispatch.
Public Restoration is overwritten; copying a command or setting that bit
cannot create cleanup permission, even if it also appears in ordinary jobs.

WORK loss cancels admission. Close first fences work and settles all possible
issuers, including lost/delayed enrollments. UNKNOWN withholds restoration
and a clean release. Registered restoration survives revoked/unreadable WORK
records, but still requires the original grant, boot, protected bytes and
resource/ownership state. Foreign state or expired authority remains UNKNOWN.
Already-Before records RestoreNotNeeded without fabricating an executed restore.

Named bounds: initial record capture 5 s; authority watch every 1 s with a 2 s
read bound; SSH/readback uses PR2's 15 s bound, clipped to original work time.
Close caches min(close+reserve, original hard deadline), reserve <=60 s.
An approved `ClearFault` also enters this terminal cleanup transition, even
with live WORK authority: it fences admission and settles all other possible
issuers before restoration. Continuing ordinary work after an early clear is
not supported by this minimal adapter. Legacy non-approved clearing is unchanged.
Approved always phases run after owned settlement, cannot reopen work, and use
that cached cleanup deadline. Watch/readers join under the same bound; an
unsettled read remains UNKNOWN. Engine join is bounded by original hard time.
Renewal does not extend it. Existing S0a controller-loss/remote settlement
guarantees are unchanged; this is not automatic cloud teardown.

Registered cleanup effects, their observation/proof and dispatch contexts are
additionally clipped to the original grant. Custody `EffectDeadline` carries
min(grant, current cleanup envelope) separately from `HardDeadline`, which
retains the original settlement obligation. The concrete custodian acknowledges
this effect bound before authorization, checks it again at the gate and stops
the owned child at expiry without a controller stop request. An absent/mismatched
echo refuses authorization, including an older custodian that ignores the field.
Expiry or refused late authorization does not discard the settlement channel;
absence proof can still be collected inside original custody time. No grant is
extended to obtain a successful cleanup outcome.

Already-Before qualifies the same retained grant, boot, resource/ownership and
protected bytes before recording RestoreNotNeeded; final closure qualifies them
again under the grant bound. An expired grant or changed restore bytes therefore
cannot bypass qualification through a no-command result.

Custody evidence retains up to 10,000 ordinary job observations and 128 reserved
cleanup observations. Capacity exhaustion refuses new execution. The existing
bundle preserves exact request/GO/inventory/policy/artifact snapshots and frozen
scenario, plus independent pins, lease observation, deployment receipts, final
owned-job states/exit receipts and witnessed fault restoration. No SSH private
key is copied. A failed cleanup or evidence write cannot produce PASS.

## Controls and limits

- Actual Bash worker claim/flock/adapter dispatch: two owned phases, bundle
  snapshots and settled receipts; foreign GO, shell body and wrong adapter pin
  fail without submitted shell execution. The helper backend is explicitly CPU
  fake; PR1/PR2 real encrypted loopback SSH tests qualify their separate seams.
- Individual request dimensions, stale/released lease, bad GO permissions or
  symlink, changed scenario before Enroll/Authorize, changed boot/bytes at either
  boundary, and mismatched deployed runner/native/roles refuse execution.
- ACTIVE partition + revoked and unreadable WORK: ordinary work refuses, issuers
  settle, retained exact restoration succeeds. Forged bit/command/obligation,
  foreign boot/resource/owner/bytes, expired grant and delayed POSSIBLE enrollment
  do not authorize restore or clean release. Already-Before is distinguished.
- Engine uses one scope, cancels an active phase on revocation, refuses inherited
  YAML/registry consent, and keeps always phases in the original envelope.
- Named mutants restore shell sourcing, skip fresh authority or byte/binding
  checks, demand live WORK for restore, exempt copied commands, trust public
  Restoration, swallow UNKNOWN, or mint a fresh always deadline. Controls must
  fail for each. Tests and mutation logs are reported with the PR evidence.

No lab hosts, devices, leases, deployed worker configuration or cloud resources
are changed by these CPU controls. Passing them is not provider qualification.

Approved terminal ClearFault failures remain on their restoration obligation.
A retry still qualifies and verifies exact Before without another mutation,
but successful state verification cannot erase the failure from final Close.
Legacy non-approved clearing keeps its existing retry disposition.


## Protected worker-owned lease mode (profile review correction)

A service bootstrap may opt into `"worker_lease":{"lock_file":"/mnt/smb/work/share/testops/locks/rdma-lab.lock"}` with the existing matching `ledger_dir` (`lock_file + ".ledger"`). This field is protected service configuration, never queue data or a submitted held-lock bit. The default external-ledger mode is unchanged.

The strict entry first validates the claimed UID/bytes and independent GO, complete policy/artifact/inventory/host/script binding and original deadlines. A worker-owned lifetime is at most ten minutes. It then verifies inherited FD9 against the canonical path's device/inode and establishes nonblocking exclusive ownership; a separate open description must observe exclusion. It never opens a new holder, recursively waits on flock, unlocks FD9, changes the worker script or mints GO. The unchanged worker retains canonical flock across claim, strict execution, evidence and exit. The ledger directory must already exist. Any unreleased acquire (including stale/unknown records), or any prior record for the exact GO lease ID, refuses; no takeover or overwrite.

Acquire is exclusive-create for the exact GO ID/owner. The in-process writer renews every 240 seconds within the original hard deadline, rechecking exact own acquire and existing live-lease semantics. Ordinary OpenApprovedRun and watcher still require ReadLiveLease. On authority loss work cancels while the existing retained cleanup grant remains bounded. Release requires joined engine/watch/readers, every issuer/job settled and every restoration cleared. A failed test with proven settlement may release; lost proof retains UNKNOWN/unreleased acquire. `worker-lease.json` and existing authority/job receipts report the result. The UID-only missing-GO probe creates no lease and authorizes zero jobs.

Controller death can still drop local canonical exclusion before a remote custodian observes EOF/settles. The owner accepted this narrowly as a disposable test-only remote-tail limit; it is not a distributed fence or global-absence proof. An uninterruptible task is not proven absent merely because its hard deadline passed. UNKNOWN cannot be labelled RELEASED; a later operator must resolve it, without rewriting the original GO or lease.

Worker-owned acquisition also checks the existing m01/m02 quarantine directory under canonical FD9 before creating acquire: uncleared records and missing/unreadable/unknown state refuse with zero jobs. Final resource release requires fresh restoration qualification during Close; historical successful ClearFault is not final proof. Failed workload with proven cleanup may still release.
