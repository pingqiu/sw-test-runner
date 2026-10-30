# Azure one-VM smoke controller

One Linux command: pinned Terraform init/plan/apply → shared HTTP pack under
normal S0b admission → Terraform destroy → independent Azure resource list.
This replaces #20's outer-run/identity-fence design. No cloud call is made by
`pins` or `plan`. The Draft has local/fake evidence only; **no launch token**.

## Inputs and prerequisites

- One dedicated existing RG, one existing lease/run at a time. RG/RBAC stay.
- Linux controller with Terraform 1.17.1 and Azure CLI already authenticated;
  credentials never on tp01, VMs or shared evidence. Public SSH key only to VM.
- Reviewed source includes `provision/terraform/azure` and `azure-validation`.
  Exact module/wiring/lock/cloud-init hashes are frozen below. Init uses
  `-backend=false -lockfile=readonly`; no provider repair/update or new backend.
- JSON tfvars matching the wiring root: explicit subscription/RG, westus2,
  Standard_D2s_v5, pinned image/artifacts, public key/controller /32 and all
  existing required declarations. `run_declaration` contains fixed work/hard UTC
  deadlines, 600–1800 s cloud cleanup reserve, budget ≤3 USD, abort estimate ≤
  budget. Hard deadline is minute-aligned and ≤60 min from run entry.
- The existing S0b service must observe/resolve the export, install the pinned
  artifacts through its normal procedure and provide an authentic claimed
  request/protected policy at the configured paths. These are normal pack
  prerequisites, not a second admission. The command neither self-signs them nor
  learns guest keys by TOFU. If they are not ready when apply/export finishes,
  admission fails and teardown runs; no arbitrary bootstrap hook is executed.
- The work/hard gap declares a best-effort cloud cleanup window; normal S0b
  process settlement can consume part of it. This is not a guaranteed minimum
  provider settlement duration; the original hard end is never extended.
  Exact module export
  must match scenario `seaweed_plan.requested_inventory`, resolved Join and the
  normal approved bindings. Only one Azure/HTTP host is accepted.

Build `go build -o /private/bin/seaweed-cloud ./cmd/seaweed-cloud`. Keep config,
tfvars, output/state and results private. Plans/state must not go to shared V:.

```sh
seaweed-cloud pins --source /private/repo --variables /private/run.tfvars.json --json
```

Put returned hashes into this config (absolute paths):

```json
{
  "source": "/private/repo",
  "module_sha256": "<pins result>",
  "variables": "/private/run.tfvars.json",
  "variables_sha256": "<pins result>",
  "output": "/private/new-smoke-run",
  "request": "/protected/service/running/request.env",
  "policy": "/protected/service/bootstrap.json",
  "estimated_usd": 0.12
}
```

Price is a reviewed input, not a price-service lookup/billing guarantee. Reprice
before the token. Output must be fresh for run, with parent already present.
Snapshots/state/command intent-output-status stay there (0700 dirs/0600 files).
Module pin hashes a sorted list of `relative-path NUL file-sha256 LF` for
top-level `.tf`, lock and cloud-init files in the two fixed module directories.
Tests, credentials and `.terraform` are not copied. TF_VAR/TF_CLI_ARGS/workspace/
data-dir/CLI-config overrides are dropped.

## Plan, run and recovery

```sh
seaweed-cloud plan --config /private/config.json --json  # local only, effects=[]
# Only after manager token, normal prerequisites, current price and lease checks:
seaweed-cloud run --config /private/config.json --json
```

The daily native shutdown schedule uses the hard deadline's UTC HHmm, enabled
with expiry tag. It deallocates compute; disks/IPs bill until destroy. Date is in
the expiry tag; a schedule is not an absolute provider-completion deadline.
Registration/permissions/provider failure can prevent arming after VM create,
so UNKNOWN always requires recovery/reporting. [AzureRM schedule semantics](https://registry.terraform.io/providers/hashicorp/azurerm/latest/docs/resources/dev_test_global_vm_shutdown_schedule).

Before apply, the private `apply-started` stamp is flushed to disk; only a
zero-exit apply writes `apply-succeeded`. Missing/malformed success evidence
stays `create_unresolved=true`, including old output folders lacking receipts.
Recovery destroy and an empty list do not resolve a pending provider create:
they return `CREATE_UNRESOLVED`, never COMPLETE/EMPTY. The operator must resolve
the original operation with Azure and report it separately; this small slice
adds no provider-operation resolver or override/marker-clear command. Keep the
original evidence; do not remove it to force a clean verdict.

Every apply attempt, including failure/UNKNOWN, is followed by destroy and full
read-only RG list within the original hard deadline. No apply retry, state-rm,
force-unlock or same-ID replacement fence. Dedicated RG exclusivity is assumed.
On session death/cleanup failure, keep output/state, tell the manager and use:

```sh
seaweed-cloud plan --config /private/config.json --json  # shows destroy step too
seaweed-cloud destroy --config /private/config.json --json
seaweed-cloud verify --config /private/config.json --json
```

Recovery destroy is an explicit new operator action bounded to 15 min, with no
provision/reapply. Changed source/vars snapshots refuse. Original journal stays;
recovery has separate record numbers. Verifier lists all RG resources via CLI,
independently of Terraform state, with no whitelist hiding leftovers. It never
deletes the RG/RBAC. An empty JSON array is required; missing/invalid list is
UNKNOWN. Run owners coordinate the single lease; this adds no lease framework.
Standalone verify has a 30-second local read deadline (or an earlier caller
deadline). Timeout is non-clean with `remaining_observed=false`. Command records
currently retain argv/stdout/status and error stderr; UTC timing and successful
stderr are not promised by this corrective slice.

CLI always emits `seaweed.cloud-smoke/v1` JSON. Exit0: `PLAN_ONLY`, `COMPLETE`,
`EMPTY`. Nonzero: `USAGE`, `INPUT_REFUSED`, `LINUX_REQUIRED`, `DEADLINE_REFUSED`,
`RG_NOT_EMPTY`, `PLAN_FAILED`, `APPLY_FAILED_OR_UNKNOWN`, `INVENTORY_FAILED`,
`EVIDENCE_FAILED`, `PACK_FAILED`, `CLEANUP_INCOMPLETE`, `RESOURCES_REMAIN`.
`CREATE_UNRESOLVED` also exits nonzero, even with an observed empty list.
Every result has `code`, `remedy`, `effects`, `remaining_resource_ids`,
`remaining_observed` and `create_unresolved`; the first is only the current list
snapshot, not proof that a pending create settled. `bundle` is normal
pack evidence. Clean teardown never turns workload FAIL into PASS. Notify the
owner on failures; the command does not send external messages automatically.

## Local verification scope

### Fresh-host materialization (Draft successor of #22/#20)

Native `run` now requires `/etc/sw-test-runner/codex02/cloud-approval.json`
before issuing any cloud command. Candidate Config cannot nominate another
approval, template or setup program. This protected `seaweed.fresh-approval/v1`
record pins `template`, `executable` and `recipe` FilePins. The executable must
be this controller's real installed path and hash. Pins/ownership/cancellation
are rechecked before publication. Missing approval returns
`FRESH_APPROVAL_REFUSED` with zero creates. `plan` remains local, no effects;
it does not imply these launch prerequisites exist.

The template is the canonical exact JSON export of private `freshTemplate`
(`seaweed.fresh-template/v1`), not a patch map or general string interpolator.
Manager pre-approval fixes Config/module/variables, raw inventory shape and exact
resource IDs, one node, public host key, MTU bounds, Binding, full
policy/files/UID/jobs, P/Plan, scenario phases, GO source and bootstrap paths.
Request/GO pins are recomputed; no authority field is learned from the guest.
Canonical re-encoding must equal the approved template bytes, so aliases,
duplicates, unknown fields and alternate legacy field spellings refuse.

Only a canonical unicast IP literal from the exact Terraform export, fixed public
key into the resolved empty slot, a stable boot, declared blank NIC/MTU slots and
bounded capture evidence are resolved. Fixed data IP, resource IDs, roles,
ports, UID, files, argv, lease, deadlines and budget stay unchanged. The initial
slice has no job-argv/config/topology substitution slots: use fixed private data
endpoints and the ordinary approved host/deployment SSH endpoint/boot slots.
Requested bytes remain exact. The ordinary strict pack performs independent
readback and live lease checks again.
Export shape/resource/node/role/IP qualification occurs BEFORE the mutating setup
recipe. No DNS or post-setup-only qualification. Dynamic disk resolution is
omitted: nonempty unresolved device IDs refuse. Any declared disk must already
have its exact independently approved by-id path and format=false; an unused
managed disk does not imply mounted-filesystem proof. The current Terraform
producer's blank disk slot still needs a separately reviewed static-profile
decision before live use; no device identity is invented here.

The protected setup recipe is a separately pinned **launch prerequisite**, not
supplied/implemented by this materializer. Its only argv is the private exact
requested-inventory path. It must complete bounded pre-generated host-key
cloud-init installation, fixed artifact/UID setup and pinned-SSH fact collection,
then emit <=16KiB exact `freshFacts` JSON, no free text/keys/private material:
`export_sha256`, stable `boot`/`boot_after`, fixed `data_address`, `nic`, `mtu`,
and actual `start`/`end` times. Disk facts and other unknown fields refuse.
Failed exit with positive stdout refuses. The controller calls the existing
`VerifyDeployment` against fixed expected files/key/boot/UID afterward.
No installer service or automatic remote build is added.

All generated files are exclusive and atomically linked in their protected
directories; bootstrap is published last. A per-run marker refuses a second
materialization. `derivation.json` binds template/executable/recipe/source,
exact export, observed probe/readback and generated file hashes. Failures enter
the existing destroy/list path (`MATERIALIZATION_REFUSED`), without changing
sticky UNKNOWN apply semantics. The receipt is provenance, not a second oracle.

Cloud-init, saved state/plan and any generated host private key remain private;
Terraform plan/apply/init/destroy stdout is now withheld from command transcripts.
Only the exact nonsecret inventory output and RG list are retained there. Do not
archive private variables/state/plan. Protected recipe errors do not echo its
stdout. This PR generates no keypair and installs no recipe/tool/credential.

Azure must hold/renew/release its separately approved `locks/azure-<rg>.lock`
and ordinary ledger records through the protected launch procedure. This
materializer reuses `ReadLiveLease` before create/publication and refuses missing,
foreign, released or stale records; it does not create a lease broker or pretend
an absent acquire/renew/release writer is installed. The installed setup/launch
recipe, key/cloud-init wiring and Azure lock/ledger lifecycle remain explicit
runtime prerequisites to qualify before a live token. M01/M02 dry run and
native controller/tool approvals remain separate. No live cloud/install/lease
qualification is claimed by the CPU controls.

`go test ./provision/smoke ./cmd/seaweed-cloud`: pinned inputs, no-effect dry-run,
single apply, failure/UNKNOWN, cancellation, residual/unknown list, sticky
workload failure and explicit recovery. Linux controls execute fake CLIs and
reach real missing-S0b refusal; #19 retains the pack's strict positive/negative
controls. Neither proves cloud deployment, artifact installation or live KV.
Terraform wiring tests mock the provider and check enabled UTC schedule, exact
exported VM ID and expiry. No account authentication/API is used by these tests.
