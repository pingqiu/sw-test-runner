# Azure provisioning module (Draft, static validation only)

This is the provision layer of the accepted deploy-platform design. It creates a run-owned network and explicit HTTP nodes in an **existing dedicated test resource group** and emits `seaweed.inventory/v1` requested JSON. The shared Seaweed pack owns deployment, preflight, workload and collection across cloud and on-prem. There is no provisioner or cloud-specific deploy script here.

No apply, subscription plan, Azure authentication, price/availability check or VM workload has been run. Local delivery is `fmt` plus `validate` only. `terraform validate` checks provider schema and configuration consistency, not Azure API behavior. Container Gate 0a and VF Gate 0b precede the future Azure runtime gate; their implementation waits for S0b-min.

## Inputs and output seam

Use the sibling `azure-validation` root as an explicit wiring example. Supply your subscription in the root provider, existing RG, region, exact Ubuntu 24.04 server image version, controller IPv4 /32, public SSH key, pinned artifact-manifest SHA256 and a node map. There is no hardcoded subscription, region, VM SKU, private key or cloud login in this module. Terraform/provider versions are pinned and the validation root commits its lock file.

Each node supplies a stable private IPv4, VM size, roles, cache-disk GiB and explicit format declaration. Adding a map entry does not renumber other IPs. Volume nodes require a managed cache disk; this initial HTTP gate does not format ephemeral NVMe. No disk is formatted by cloud-init. LUN 0 alone is not device identity: the observer must bind the actual managed disk to a stable `/dev/disk/by-id` before a reviewed pack may format it. Small disposable gates can choose small SKUs and disks; there is no automatic expensive topology.

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
