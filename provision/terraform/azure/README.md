# Azure provisioning module (Draft, local mock validation only)

This is the provision layer of the accepted deploy-platform design. It creates a run-owned network and explicit HTTP nodes in an **existing dedicated test resource group** and emits `seaweed.inventory/v1` requested JSON. The shared Seaweed pack owns deployment, preflight, workload and collection across cloud and on-prem. There is no provisioner or cloud-specific deploy script here.

No apply, subscription plan, Azure authentication, price/availability check or VM workload has been run. Local delivery is `fmt`, `validate` and schema-backed mock-provider tests only (no real provider plan/apply). `terraform validate` checks provider schema and configuration consistency, not Azure API behavior. Container Gate 0a and VF Gate 0b precede the future Azure runtime gate; their implementation waits for S0b-min.

## Inputs and output seam

Use the sibling `azure-validation` root as an explicit wiring example. Supply your subscription in the root provider, existing RG, region, exact Ubuntu 24.04 server image version, controller IPv4 /32, public SSH key, pinned artifact-manifest SHA256, a typed `run_declaration` and a node map. There is no hardcoded subscription, region, VM SKU, private key or cloud login in this module. Terraform/provider versions are pinned and the validation root commits its lock file.

Each node supplies a stable private IPv4, VM size, roles, cache-disk GiB, explicit format declaration and required `services = [{name, port}]` (explicit `[]` for a client-only node). Adding a map entry does not renumber other IPs. Volume nodes require a managed cache disk; this initial HTTP gate does not format ephemeral NVMe. No disk is formatted by cloud-init. LUN 0 alone is not device identity: the observer must bind the actual managed disk to a stable `/dev/disk/by-id` before a reviewed pack may format it. Small disposable gates can choose small SKUs and disks; there is no automatic expensive topology.

`inventory_json` is an exact JSON string. After a future authorized apply, export those bytes, validate with the inventory CLI from Draft #11 and keep their SHA256. Host keys, actual data NIC/MTU, boot generations and disk device identities are deliberately pending. Trusted observation fills them in a separate resolved document; `inventory.Join` must preserve scope. This output is data, never GO, host trust, a lease or permission to deploy/format.

`managed_resource_ids` includes VMs, their OS disks, cache disks, NICs, public IPs, subnet, VNet and NSG. Terraform attachment/association pseudo-IDs are not independently billable ARM resources. Partial apply may not produce complete outputs: reconcile the private state and live run-tagged inventory before teardown; an empty/missing output cannot prove absence.

## Admission and teardown launch gates

No execution entrypoint is provided in this Draft. A future controller must bind the authoritative GO, budget/deadlines, exact configuration/artifacts/inventory, state and observed identities through S0b. Use only a Linux controller; credentials and private Terraform state stay there, never tp01 or guests. Guests receive only the SSH public key. This module creates no RG, managed identity, Automation account or RBAC assignment.

The NSG admits controller SSH and listed private-subnet TCP services, then denies other inbound traffic (including Azure's default broad VNet allowance). No RDMA is advertised: all hosts are HTTP with RDMA `not_applicable`. Cloud-init only installs Noble prerequisite package names, including `librdmacm1t64`; it neither uploads Seaweed artifacts nor launches services.

Before any future destroy, compare recorded VM/OS-disk/attachment identity and bindings with live Azure state. AzureRM's VM deletion can remove its OS disk; a changed or replacement child is a refusal, not authorization to delete it. A raw `terraform destroy` alone is not the identity-safe cleanup/absence proof. Run the accepted bounded cleanup and an independent full run-inventory absence check, while preserving the pre-existing RG/RBAC. Failed/unknown operations must settle and be reconciled; there is no automatic unknown-create retry. Review this runtime integration before any spend. VM shutdown, fixed teardown deadlines and billing limits are controller responsibilities; no session-death deletion guarantee is claimed.

## Static checks (no cloud)

```sh
terraform fmt -check -recursive provision/terraform
terraform -chdir=provision/terraform/azure init -backend=false -input=false
terraform -chdir=provision/terraform/azure validate
terraform -chdir=provision/terraform/azure-validation init -backend=false -input=false
terraform -chdir=provision/terraform/azure-validation validate
```

Provider downloads are network access; these commands do not plan/apply or contact an Azure subscription. Do not add real tfvars, credentials, state or plans to git.

References: [validate semantics](https://developer.hashicorp.com/terraform/cli/commands/validate), [pinned Linux VM provider schema and OS disk deletion behavior](https://github.com/hashicorp/terraform-provider-azurerm/blob/v5.12.0/website/docs/r/linux_virtual_machine.html.markdown).

## Deadline/cost split proposed for review

The module accepts a required `run_declaration` object (no defaults): RFC3339 UTC `work_deadline`, `hard_deadline`, positive integer `cloud_cleanup_reserve_seconds`, positive `budget_usd` and `abort_estimate_usd <= budget_usd`. Hard deadline must leave at least the declared cloud cleanup reserve after work deadline. It echoes a `seaweed.provision-declaration/v1` object with run/artifact identity and currency USD. The validation root forwards it and the separate managed-resource ID list. These are typed declarations, **not** a watchdog, price check, cost guarantee, provider timeout or authentic approval. Static validate does not exercise live values/API behavior.

The future reviewed controller owns: authentic GO and its current budget/deadlines; fresh regional compute/disk/IP/network/tax-tail estimates; the exact input/config/plan/declaration/inventory hashes; process/lease admission and work stop; the fixed hard end and cleanup; cost accrual/abort checks; retained journal/state; and independent absence. It must refuse an expired declaration, a cost estimate at/above its authorized abort threshold, a changed price/topology/binding or a declaration that expands authentic authority. The module does not infer authority from successful parsing or Terraform state. Cloud cleanup reserve is separate from S0a's <=60-second process reserve and must include observed provider settlement time.

The accepted #11 contract (`0664d799`) is now emitted: inventory `deadlines.work/hard` copy the exact canonical UTC declarations; `deadlines.cleanup_reserve_ms` comes from the required integer `process_cleanup_reserve_ms` (1..60000), separately bounded within those dates. It is NOT converted from `cloud_cleanup_reserve_seconds`. Services preserve each explicit node list, with unique known names/ports and required HTTP/gRPC listeners per role. This HTTP module admits `master_http`, `master_grpc`, `filer_http`, `filer_grpc`, `volume_http`, `volume_grpc`, `s3_http`; no RDMA listener/device is invented. The subnet service rule uses exactly the union of declared ports, and is absent when that union is empty; controller SSH stays separate. The requested inventory still lacks trusted host/device observations. Canonical UTC spelling and exact Join preservation are data checks, not authentic S0b admission. No provider controller/adapter is implemented here; partial-create recovery, identity-safe destroy and independent absence still need their separate review and GO.

## Exact export and consumer handoff

`inventory_json` remains a Terraform **string containing JSON**, not an object output. Freeze this contract on the Linux controller:

```sh
# Future authorized run only: never execute apply to obtain this output today.
terraform -chdir="$TF_ROOT" output -raw inventory_json > "$RUN_DIR/requested.json"
seaweed-inventory --json validate "$RUN_DIR/requested.json" > "$RUN_DIR/inventory-validation.json"
sha256sum "$RUN_DIR/requested.json" > "$RUN_DIR/requested.sha256"
terraform -chdir="$TF_ROOT" output -json run_declaration > "$RUN_DIR/provision-declaration.json"
terraform -chdir="$TF_ROOT" output -json managed_resource_ids > "$RUN_DIR/resource-ids.json"
# Independent trusted observer produces resolved.json and binds the exact raw
# requested bytes. Never fabricate host keys, boot/device facts or a receipt.
seaweed-inventory --json join "$RUN_DIR/requested.json" "$RUN_DIR/resolved.json" > "$RUN_DIR/join.json"
```

Each nonzero command stops the handoff. Preserve requested bytes verbatim; no PowerShell text transcoding, parse/re-encode or pretty printing before the digest/Join. `terraform output -json inventory_json` emits a quoted string and **must not** be passed to Decode. Missing/partial outputs refuse; they do not imply no resources. A successful Decode/Join still grants no execution.

CPU proof, with an independently built accepted inventory CLI:

```sh
python3 provision/terraform/tests/export_contract.py \
  --terraform /absolute/path/to/terraform \
  --inventory-cli /absolute/path/to/seaweed-inventory \
  --fixtures /absolute/path/to/runner/examples/inventory
```

The explicit main-guarded test uses synthetic **resource-free local state**, invokes real Terraform `output -raw` and the actual inventory CLI, and checks raw export -> Decode -> exact-digest Join. It also requires quoted `-json` strings and reserialized request bytes to refuse. It needs no provider, init, credentials, Azure, lab or running cluster. HTTP/DC fixtures only test serialization; DC provisioning is not claimed. This is not a test of module-generated values after apply, hardware facts, S0b trust, partial-create recovery or safe destroy.

## Future lifecycle and recovery procedure (disabled now)

The commands below are the payloads a **future reviewed Linux controller** must own; they are documentation, not a runnable launcher or current GO. Do not run plan/apply/destroy today. No custom guard, retry loop or replacement controller is introduced. For every future command the controller records operation ID, exact argv/config/plan hashes, UTC start/end, exit/timeout, stdout/stderr and settlement in its retained journal. It admits jobs through the accepted S0b path, not a YAML boolean. Native Terraform diagnostics remain available with `-json`; stable shell-level refusal/UNKNOWN codes and remedy hints belong to that reviewed controller.

1. **Admit and prepare.** Require separate spend/runtime GO, accepted S0b-min PR3 and runtime adapter, the reviewed local Gates 0a/0b, reviewed inventory deadline/service-port bindings, exact existing RG/account identities and pinned artifacts. Verify current prices/quota/image, estimate all billable tails, freeze topology and deadlines with cleanup time reserved. On a private Linux controller directory (`umask 077`), retain the pinned root/module, private tfvars, lockfile, state, journal and receipts; credentials remain on that controller, never tp01/guests. Independently inventory the dedicated RG before create; unknown/pre-existing resources are a refusal. There is no automatic deletion after controller/session death; record residual billing exposure and the recovery owner.

2. **Plan and bind before effects.** After admission only, controller-owned bounded payloads are:

   ```sh
   terraform -chdir="$TF_ROOT" init -input=false -lockfile=readonly
   timeout -k 10s "${PLAN_SECONDS}s" terraform -chdir="$TF_ROOT" plan -input=false -out="$RUN_DIR/create.tfplan"
   terraform -chdir="$TF_ROOT" show -json "$RUN_DIR/create.tfplan" > "$RUN_DIR/create-plan.json"
   sha256sum "$RUN_DIR/create.tfplan" "$RUN_DIR/create-plan.json"
   ```

   The future controller bounds init/show too, reviews exact additions/replacements/cascades and subscription/RG scope, and binds the saved plan plus all data to authentic authority. Any unknown result or plan/scope/hash/authority change refuses. Never substitute `-auto-approve`, target only part of the plan, or retry an uncertain create.

3. **Apply once, then hand off.** Only after those exact checks and authentic approval does the controller admit the single saved-plan payload:

   ```sh
   timeout -k 10s "${APPLY_SECONDS}s" terraform -chdir="$TF_ROOT" apply -input=false "$RUN_DIR/create.tfplan"
   ```

   Time budgets come from remaining fixed authority/deadlines, not unbounded retry. Success requires settled provider operations plus complete live identity/inventory checks. Export raw inventory as above; resolve trusted facts and bind them before the shared pack can deploy anything. This module installs prerequisites only. HTTP/DC data transport and required evidence still belong to the one shared pack.

4. **Partial/UNKNOWN apply.** Stop further creates. Preserve original UNKNOWN receipt, stdout/stderr, input/plan hashes and state; create a **separate observed settlement** record. Do not re-apply, `state rm`, import a replacement, force-unlock or invent complete outputs. Controller-owned read-only diagnostics include:

   ```sh
   terraform -chdir="$TF_ROOT" state pull > "$RUN_DIR/recovery-state.json"
   terraform -chdir="$TF_ROOT" state list > "$RUN_DIR/state-addresses.txt"
   az resource list --subscription "$SUBSCRIPTION_ID" -g "$TEST_RG" -o json > "$RUN_DIR/live-resources.json"
   az monitor activity-log list --subscription "$SUBSCRIPTION_ID" --resource-group "$TEST_RG" --start-time "$CREATE_START" --end-time "$OBSERVED_END" -o json > "$RUN_DIR/activity.json"
   ```

   Bound these reads. Reconcile state **and** activity/operation results/full live inventory against every intended create, including OS disks and resources absent from outputs/state. Wait boundedly for creating/deleting operations to settle. If outcome/identity/binding cannot be established, report UNKNOWN with unresolved billable IDs and escalate for exact cleanup authority; never certify absence from empty state alone. Activity log absence is not resource absence. No recovery behavior here is runtime-qualified.

5. **Teardown on every admitted outcome.** Stop/settle only owned pack processes and export evidence. Before deletion, independently recheck the exact live resource generations, VM children/OS-disk IDs, attachments and dependencies against approved ownership. Changed/replacement resources or new bindings stop destructive work. After those checks, a bounded destroy **plan** is a separately reviewed payload:

   ```sh
   timeout -k 10s "${DESTROY_PLAN_SECONDS}s" terraform -chdir="$TF_ROOT" plan -destroy -input=false -out="$RUN_DIR/destroy.tfplan"
   terraform -chdir="$TF_ROOT" show -json "$RUN_DIR/destroy.tfplan" > "$RUN_DIR/destroy-plan.json"
   # Recheck exact identities/bindings/cascades and admission immediately before
   # the approved saved destroy plan. This fence is NOT implemented in this PR.
   timeout -k 10s "${DESTROY_SECONDS}s" terraform -chdir="$TF_ROOT" apply -input=false "$RUN_DIR/destroy.tfplan"
   ```

   A raw Terraform destroy/apply does not implement that identity fence. In particular VM deletion can cascade to an OS disk. The future controller/executor must close that race and retain cleanup authority before any runtime activation; this document does not qualify it. A timeout/nonzero is UNKNOWN until read-only provider observation settles it; no blind destructive retry. Never delete the pre-existing RG or alter its RBAC. Recovery uses the same exact reviewed ownership, not broad RG/tag deletion.

6. **Independent closure.** A separate verifier queries every recorded/intended full resource ID plus the full RG inventory and outstanding operations, checks run resources absent and no unexpected creations, and verifies RG plus original RBAC retained. A tag filter, empty Terraform state, empty outputs or successful CLI exit is insufficient. COMPLETE requires terminal provider settlement and this independent proof; otherwise report unresolved billable IDs/cost exposure. Account for actual VM/disk/IP lifetimes and all tail charges until proven absent. This absence/closure verifier is still unimplemented and unqualified; no runtime GO follows from these instructions.


## Actual module output contract controls (no account)

After local init/validate, run the committed mock-only test and actual accepted
inventory CLI. No credentials or Azure subscription API are used. The two
mock apply states are synthetic; they do not prove real provider operation,
resource identity, disk formatting, SSH trust, or teardown. Thirteen mock runs require reserve/date/service refusals; one additional
mock plan accepts canonical fractional UTC dates. A zero-test success is refused.

```sh
python3 provision/terraform/tests/azure_inventory_contract.py \
  --terraform /absolute/path/to/terraform \
  --inventory-cli /absolute/path/to/seaweed-inventory \
  --module provision/terraform/azure \
  --evidence /private/local/test-output
```

This explicitly invoked test selects only `tests/inventory.tftest.hcl` with
`mock_provider "azurerm"`, captures the actual `inventory_json` values, exports
them via real `output -raw` in resource-free local state, and checks the real
Go Decode/Join. It checks both all-role/nondefault-port and client-only/empty
service outputs, field drift and missing-service refusals, and quoted-string
refusal. Synthetic observations test the Join seam only. The previous
`export_contract.py` remains a serialization fixture check; it is not substituted
for this module-output test. Import/discovery causes no native or remote work.

Terraform mock provider semantics and JSON test-state format are documented at
https://developer.hashicorp.com/terraform/language/tests/mocking and
https://developer.hashicorp.com/terraform/internals/machine-readable-ui .
