# Azure provisioning module (Draft, local mock validation only)

This is the provision layer of the accepted deploy-platform design. It creates a run-owned network and explicit HTTP nodes in an **existing dedicated test resource group** and emits `seaweed.inventory/v1` requested JSON. The shared Seaweed pack owns deployment, preflight, workload and collection across cloud and on-prem. There is no provisioner or cloud-specific deploy script here.

No apply, subscription plan, Azure authentication, price/availability check or VM workload has been run. Local delivery is `fmt`, `validate` and schema-backed mock-provider tests only (no real provider plan/apply). `terraform validate` checks provider schema and configuration consistency, not Azure API behavior. Container Gate 0a, VF Gate 0b and cloud runtime qualification remain separate review/launch gates.

## Inputs and output seam

Use the sibling `azure-validation` root as an explicit wiring example. Supply your subscription in the root provider, existing RG, region, exact Ubuntu 24.04 server image version, controller IPv4 /32, public SSH key, pinned artifact-manifest SHA256, a typed `run_declaration` and a node map. There is no hardcoded subscription, region, VM SKU, private key or cloud login in this module. Terraform/provider versions are pinned and the validation root commits its lock file.

Each node supplies a stable private IPv4, VM size, roles, cache-disk GiB, explicit format declaration and required `services = [{name, port}]` (explicit `[]` for a client-only node). Adding a map entry does not renumber other IPs. Volume nodes require a managed cache disk; this initial HTTP gate does not format ephemeral NVMe. No disk is formatted by cloud-init. LUN 0 alone is not device identity: the observer must bind the actual managed disk to a stable `/dev/disk/by-id` before a reviewed pack may format it. Small disposable gates can choose small SKUs and disks; there is no automatic expensive topology.

`inventory_json` is an exact JSON string. After a future authorized apply, export those bytes, validate with the inventory CLI from Draft #11 and keep their SHA256. Host keys, actual data NIC/MTU, boot generations and disk device identities are deliberately pending. Trusted observation fills them in a separate resolved document; `inventory.Join` must preserve scope. This output is data, never GO, host trust, a lease or permission to deploy/format.

`managed_resource_ids` includes VMs, their OS disks, cache disks, NICs, public IPs, subnet, VNet and NSG. Terraform attachment/association pseudo-IDs are not independently billable ARM resources. Partial apply may not produce complete outputs: reconcile the private state and live run-tagged inventory before teardown; an empty/missing output cannot prove absence.

## Admission and teardown launch gates

The small [one-VM smoke controller](../../smoke/README.md) uses a pinned saved Terraform plan/apply, the existing shared pack's normal S0b admission, then destroy and an independent full RG list. It has no outer S0b, second admission or delete-identity fence: the dedicated existing RG and one existing lease/run are the reviewed assumptions. Credentials/private Terraform state stay on Linux, never tp01 or guests. Guests get only the public key. This module creates no RG, managed identity, Automation account or RBAC assignment. Runtime still requires the manager's launch token; local tests grant none.

The NSG admits controller SSH and listed private-subnet TCP services, then denies other inbound traffic (including Azure's default broad VNet allowance). No RDMA is advertised: all hosts are HTTP with RDMA `not_applicable`. Cloud-init only installs Noble prerequisite package names, including `librdmacm1t64`; it neither uploads Seaweed artifacts nor launches services.

The controller preserves failed/UNKNOWN apply evidence across recovery. A currently empty list cannot settle a pending create: without the original successful-apply receipt, recovery/verify reports CREATE_UNRESOLVED and asks for operator/provider-operation resolution. There is no automatic apply retry, blind state editing or marker-clear command. Terraform destroy retains the RG/RBAC; the full read-only RG list is independent of Terraform state. There is no native shutdown/DevTestLab requirement. Expiry tags remain; after session death the next session must destroy and verify, since resources keep billing until cleanup.

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

The one-VM controller checks pinned configuration/lock/vars, declared estimate/cap and fixed work/hard times. Fresh regional prices and the launch/lease token are operator prerequisites, not a new price/authority service. The cloud cleanup gap is best effort: normal S0b process settlement may consume part of it, and provider deletion may exceed it. The hard deadline is never extended automatically. Process and cloud reserves are distinct declarations, not guarantees of provider settlement or minimum teardown time.

The accepted #11 contract (`0664d799`) is now emitted: inventory `deadlines.work/hard` copy the exact canonical UTC declarations; `deadlines.cleanup_reserve_ms` comes from the required integer `process_cleanup_reserve_ms` (1..60000), separately bounded within those dates. It is NOT converted from `cloud_cleanup_reserve_seconds`. Services preserve each explicit node list, with unique known names/ports and required HTTP/gRPC listeners per role. This HTTP module admits `master_http`, `master_grpc`, `filer_http`, `filer_grpc`, `volume_http`, `volume_grpc`, `s3_http`; no RDMA listener/device is invented. The subnet service rule uses exactly the union of declared ports, and is absent when that union is empty; controller SSH stays separate. The requested inventory still lacks trusted host/device observations. Canonical UTC spelling and exact Join preservation are data checks, not authentic S0b admission. The separate small controller has local/fake evidence only; no account API, live deployment or cleanup qualification follows. No deletion identity fence is claimed.

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

## Small lifecycle and recovery

See the [controller recipe](../../smoke/README.md) for pins/plan/run/destroy/verify, private evidence and manual recovery. No cloud-specific deployment code or additional admission layer is added to the pack. On UNKNOWN, retain the original evidence, report remaining IDs/unknown list observations, and have the operator resolve the original provider operation. Repeating destroy plus an empty list never turns an unresolved create into COMPLETE. Standalone verify uses a finite 30-second read deadline, with timeout remaining non-clean. No resources, RBAC or credential changes are authorized by this documentation.

## Actual module output contract controls (no account)

After local init/validate, run the committed mock-only test and actual accepted
inventory CLI. No credentials or Azure subscription API are used. The two
mock apply states are synthetic; they do not prove real provider operation,
resource identity, disk formatting, SSH trust, or teardown. Fifteen mock runs require reserve/date/service refusals; one additional
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
