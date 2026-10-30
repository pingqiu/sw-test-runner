# Cloud lifecycle/S0b contract — review before implementation

This is the third follow-up in 6ae4d17e. Design only; it implements no account
adapter or launcher. #13 supplies inventory data; #19 supplies explicit HTTP
execution. No legacy PS/py chain, new lease/custody framework, account API, cloud
plan/apply, host contact or spend is authorized here.

## Scope decision required

The accepted RunScope process cleanup reserve is at most 60 seconds
(`run_scope.go:NewRunScope`, `cleanupContext`). The reviewed one-VM cloud shape
reserves 25 minutes for provider deletion/settlement. Treating the latter as a
RunScope reserve, restarting a deadline after failure, or running Terraform
destroy after scope closure would violate the accepted contract. Adding provision
phases to #14 also violates its exact five-phase sealed scenario. We must freeze
this mapping before writing a controller that appears to have authority.

Recommended mapping: reuse two independently admitted S0b runs, not new custody.
The outer Linux controller run owns provision/observation/teardown CLI jobs under
its own original fixed cloud deadline and standard <=60s process reserve. Its
work window includes the declared cloud teardown interval. The child shared-pack
run has the inventory's original work/hard/process envelope and exact five phases;
it cannot extend or mint the outer grant. Both request/GO/artifact identities and
their deadline relation are frozen together by the existing independent admission
service. The controller cannot self-sign a child GO from output JSON or TOFU a
new SSH host key. Trusted cloud/boot/host-key observation and authentic child
admission remain explicit protected-service inputs. Missing admission refuses.

The outer run must have enough time for provider operations BEFORE its work
cutoff; its 60s cleanup only settles owned CLI issuers. Cloud objects are not
proven gone merely because a CLI/pid is gone. UNKNOWN provider operations must
settle read-only before safe deletion; expiry retains UNKNOWN/cost exposure,
never COMPLETE. This mapping needs codex01 review; it is not implemented by the
static module or pack.

## Proposed implementation, one shared pack

A small Go controller uses existing strict RunApprovedRequest/job pins to drive:

1. Local JSON `plan`/dry-run: hashes of module/lock/vars, pinned artifact tuple,
   exact account/RG and intended full IDs, immutable work/hard/budget declarations,
   allowed operations and recovery instructions. effects=[]; stable code/remedy.
   No credentials, providers or account calls in this local planning command.
2. After genuine outer admission only: exact saved Terraform plan, independently
   reviewed resource inventory/scope/cost, then one apply. Raw inventory export
   -> Decode/Resolve/Join -> independently admitted child pack. No cloud deploy
   implementation; #19's same shared pack deploys/runs/collects/settles.
3. Every exit: stop new creates, preserve original results/state/journal, export
   child evidence, reconcile full intended IDs, private state and full RG inventory
   plus provider operations. UNKNOWN receipt is immutable; new observed-settlement
   receipt is separate. Never blind reapply/state-rm/force-unlock/import replacement.
4. After ownership/binding checks: saved destroy plan, exact scope, no RG/RBAC
   deletion. Preserve child generation, VM UUID, disk unique identity, NIC/IP
   bindings and outstanding operation IDs. No tag-only or state-only ownership.
5. Separate `verify` invocation/read authority: all intended/observed IDs absent,
   operations terminal, full RG empty, pre-existing RG/RBAC unchanged. COMPLETE
   only after both settlement and independent verifier; actual cost tails retained.

Credentials stay on the Linux controller (never tp01/guests/shared evidence),
VMs get only public keys. Terraform and observation commands are fixed protected
jobs, not arbitrary argv supplied by plan JSON. No mutation entry runs by default.

## Delete-identity fence remains unqualified

A pre-delete GET followed by stock Terraform destroy is not an atomic
same-ID replacement fence. Provider resource UUID/binding readbacks and Detach
children must be rechecked; that alone must not be advertised as closing the
read/delete race. The exact AzureRM/provider-supported conditional-delete or
exclusive ownership enforcement needs a reviewed contract. Until qualified,
refuse create as well as delete: do not allocate objects whose safe teardown is
unsupported. No fabricated adapter capability or caller boolean can bypass it.

This is an open implementation boundary, not a new permission request to spend.
Azure VM Delete reference lists its supported request surface; it does not itself
qualify a generation-conditional deletion fence:
https://learn.microsoft.com/en-us/rest/api/compute/virtual-machines/delete
Generic resource operations also require provider-specific qualification:
https://learn.microsoft.com/en-us/rest/api/resources/resources

## Local controls required for the code successor

Original GO/scope/deadline/plan drift and no admission -> zero effects; missing
child boot/key proof -> refuse; partial apply with missing outputs -> recover
all intended IDs; unknown create -> no retry or later create; same-ID replacement,
new attachment, cascade, foreign RG and changed actor -> no delete; UNKNOWN CLI
or provider operation -> never COMPLETE; empty Terraform state with live RG
resource -> verifier FAIL; independent absence + preserved RG/RBAC -> COMPLETE.
Session loss and deadlines must retain evidence/exposure, not manufacture absence.

Requested review decision: approve/adjust the outer/child S0b mapping and name
the qualified ownership/delete fence before the mutation adapter code. Keep all
spend/runtime gates held. This design does not claim the requested controller,
destroy or verifier is implemented yet.
