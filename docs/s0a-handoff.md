# S0a SSH Substrate Handoff

Option C, relayed by Claude01 6ae3c8e39c9fd1ba9ca5292c, lifts the 1,150-line
ceiling and assigns the whole gradual migration to codex03. This substrate is
submitted for Claude02's independent review, not self-accepted or lab-qualified.
After acceptance Codex01 becomes migration reviewer; codex03 authors one small
PR at a time. Product reviews continue to take priority.

R1-R3 at e0700a34 were independently accepted in Claude02 review b62b3323.
This successor closes the carried transport, executable and log-descriptor
boundaries and adds the shared witnessed-TierChaos contract. No Python launcher
is retired, wrapped or selected as the new platform.

## Owned Jobs

`SSHJobs` opens a separate SSH connection for each custodian, not one session per
job on the shared `Node.client`. Eight concurrent jobs are admitted per backend;
further enrollments wait inside their existing context. Proven completed entries
and capacity are released. UNKNOWN entries remain obligations, not fake exits.
Closing an owned TCP connection unblocks channel writes even when the peer stops
reading. It does not close unrelated jobs or the node's ordinary connection.

The decoder lives through HardDeadline, independent of work cancellation. The
custodian refuses ordinary authorization after WorkDeadline and retains its
last-resort timer through HardDeadline. Lost replies stay UNKNOWN. A fully sent
authorization may have executed before its reply was lost; cancellation is not
evidence that it never ran.

Linux executable bytes are copied to a memfd, hashed against JobSpec.SHA256 and
sealed against write/grow/shrink before enrollment. The child execs that exact
descriptor after authorization. Only regular executable files <=512 MiB qualify.
This hashes the primary executable, not shared libraries, script interpreters or
the deployed runner itself. Deployment/SSH host trust remains an external/S0b
precondition; the inherited Node host-key policy is not upgraded in this slice.

The output root must already exist, be owned by the custodian UID, have no group
or other permission bits and have no symlink ancestor. Linux openat2 pins it;
logs are exclusive, no-symlink, beneath-root descriptor-relative creates. Missing
kernel support or unsafe roots refuse, with no pathname fallback. Use a canonical
private run directory, not /var/run aliases or a shared writable directory.

The custodian is a fresh single-job child subreaper. It uses pidfds and reaps all
owned/adopted descendants before a settled receipt. Wait preserves native exit
status and log paths. Kill witnesses the exact live leader only; it is NOT
descendant absence, completed DMA or source-memory reclamation. Wait/Kill remain
work-phase calls; packs must leave collection time before WorkDeadline. Stop is
absence cleanup, not permission to fabricate an exit code when Wait was missed.

Jobs must remain signalable by their custodian. Do not launch UID-changing sudo
children from an unprivileged custodian and assume they can be settled. A
privileged plan needs an explicitly approved privileged node/custodian and its
own correctly owned private output root. No implicit escalation was added.

## Witnessed TierChaos

```go
scope.ApplyWitnessedFault(ctx, plan.Key(), plan)
scope.ClearFault(ctx, plan.Key())
receipts := scope.FaultReceipts()
```

`WitnessedFault` contains Kind, Node, Resource, reviewed Before/During state maps,
hash-bound Apply/Restore JobSpecs and a read-only Observe provider. Resource keys
are node/resource, shared across fault kinds. Providers enumerate COMPLETE
touched state, including ownership identifiers and explicit absence; command
return codes or an empty/missing enumeration are never substitute observations.
The scope joins node/boot/resource and the command's boot identity, brackets
reads locally, compares exact state, and returns defensive receipt snapshots.

The restoration obligation exists before dispatch. Both commands use owned-job
custody and native exit receipts. A possible apply issuer must settle before
restoration. The restore preflight refuses changed boot/ownership/configuration;
it permits only the reviewed before/during states per field, including partial
apply. Already-restored state is marked RestoreNotNeeded, not given a fabricated
native exit. The final probe still runs after a failed restore and at repeated
clear/close. An unwitnessed or failed apply remains a run failure even if the
action used ignore_error and cleanup later succeeds.

Only the pinned restoration command can be admitted during cleanup; Start
overwrites any caller-provided Restoration bit and cannot revive work admission.
Cleanup uses the original HardDeadline, never a fresh lease. Unknown settlement
or restoration fails the run and must retain the external quarantine/lease
disposition. A controller crash is NOT automatic restoration of persistent
iptables/qdisc/mount state: no such hardware claim is made by this local layer.

`inject_partition`, `inject_netem`, `fill_disk` and `clear_fault` now require
ActionContext.Safety plus a matching ActionContext.FaultPlans[params.plan]. They
refuse the old mutable cleanup strings and the /tmp fill default. Existing YAML
without an approved provider now refuses rather than silently using the unsafe
legacy path. The lower-level legacy infra helpers are not newly qualified.

## Codex03's Next Slice

S1/D2 builds the real providers and YAML against this contract; they are NOT
implemented or hardware-proven by fake state maps. Start with v3B TCP
control/HTTP/RC-control partition and the owner-approved run-created loop ext4
image, <=16 GiB, under /data/nvme/<run_root>/enospc.img. RoCE disruption remains
NOT_PROVEN. Never fill /tmp or a shared filesystem. Include mount, loop and image
identities/absence in the real provider and teardown proof, not just the fillfile.
Default qdisc/iptables state must not be overwritten to make a test fit.

Keep the existing queue/lease/owner-GO path unchanged. This code supplies no
permissive verifier or new authorization source. Agent/coordinator distributed
phase state, deployed-runner/config provenance and full envelope integration are
S0b, not silently completed. No dual-launcher cutover or Python retirement before
retained-result parity, review and Ping's explicit cutover.

## Verification Boundary

Local fake nodes and encrypted loopback SSH fixtures only; no host SSH, remote
build, privileged fault, cloud or lab. Controls cover a blackholed SSH peer,
12 simultaneously held connections, cancelled session creation, job retirement,
wrong executable digest, seal bypass, symlink ancestors, false effect/restore,
partial apply, UNKNOWN issuer, changed boot, ignored failure and action cancel.

Local WSL runs the real Linux custodian, sealed executable, output descriptors,
pidfd/subreaper escaped descendant, EOF cleanup with UNKNOWN receipt, and a real
run-owned marker fault restored after WorkDeadline. That marker is not network
partition or ENOSPC evidence. Windows has no race-detector toolchain here; neither
ordinary repeats nor loopback tests are reported as race or hardware coverage.

L1's delayed writer makes the join control deterministic; L2 explicitly checks
pre-cancelled calls write zero bytes and leave an unused channel usable. Original
failed captures remain separate from final green captures in the evidence bundle.
