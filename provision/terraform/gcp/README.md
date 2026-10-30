# GCP plan-only provision skeleton

Static, no-spend Draft. This is provision configuration only: shared `seaweed.inventory/v1`, prerequisite-only cloud-init, and the same typed deadline/budget declaration as the accepted Azure static contract. There is no Seaweed deployment executor, plan/apply launcher, lease/guard, cloud credential bootstrap or workload here. The **one shared Seaweed pack** owns deployment/workload after reviewed S0b-min admission.

Current checks are Terraform 1.17.1 fmt and module/example validate with `init -backend=false`. Provider `hashicorp/google` 8.9.0 and lock files are pinned. No authenticated provider plan, API/price/quota/image lookup, apply, cloud resource, VM, lab or spend is qualified or authorized. The provider download is public registry access only. Keep Draft and wait for S0b-min PR3 before any provider work; a future real `terraform plan` needs explicit scope/credential authorization too.

## GCP scope and transport

Inputs explicitly select the existing project, region/zone, existing network/subnetwork self-links, an exact Ubuntu 24.04 image resource (never an image family), each node's machine type/private IP/cache-disk size and the public key. The provider root uses that project/region/zone. Image owner/architecture, subnet/zone/project bindings, organization firewall policy, quota and prices remain live admission checks; static validate cannot qualify them.

Owned resources are three target-tag-scoped ingress firewall rules, per-node regional public address, separate pd-balanced boot disk, instance and optional separate cache disk. Boot disks use `auto_delete=false` and remain independently state-owned. NIC/access and disk attachments are VM-embedded bindings, not independent Compute resources. The existing project/network/subnet/IAM are not owned or destroyed. `managed_resource_ids` has full self-links; `resource_bindings` adds server-assigned VM/disk unique IDs and NIC names so a later observer can distinguish same-name replacements. Partial apply outputs/state do not constitute complete ownership or absence proof.

SSH is allowed only from the controller IPv4 /32, listed service TCP ports only from declared node /32s, then other ingress is denied for the run tag. An existing higher-priority organization/network rule can change effective policy and must be checked before any future activation. Project SSH keys are blocked and OS Login disabled for this public-key-only profile; the Google guest agent must create the declared login. No service-account block, IAM role or key is requested. A future real preflight must verify no guest cloud identity was attached; no live claim is made here. Cloud-init metadata contains prerequisites only.

Google [RDMA network profiles](https://docs.cloud.google.com/vpc/docs/rdma-network-profiles) distinguish RoCE/Falcon and limit them to supported shapes/NICs/networks; general GCE machines do not imply RoCE, and RoCE does not by itself prove the product's RC/DC compatibility. This skeleton creates ordinary HTTP networking, no RDMA network profile/NIC/driver and no accelerator. A future selected RoCE shape would need capability/device/GID/MTU/fabric and product byte-path proof. No generic RC/DC or performance promise is made.

## Inventory and declaration contract

The caller must supply a nonempty node map (1..32), roles, exact SKU/image, stable private IPv4s, explicit cache-disk GiB and `format_allowed`; there is no default paid topology, region, account/project, image or SKU. Volume roles require a cache disk. Public key only; private key/provider credentials/state/plan remain on the private Linux controller, never tp01 or guests. `credential_ref` is a logical reference, not a key path or secret. Prerequisite cloud-init uses Noble `librdmacm1t64`; it does not format disks, install product artifacts or start product services.

`inventory_json` is a **string containing JSON**, `state=requested`, `transport=http`; every host has `rdma.status=not_applicable`. Host key, actual NIC/MTU and stable cache device identity are pending trusted observation. Required RC/DC cannot be requested or silently replaced by HTTP: this module exposes no transport selector and is explicitly HTTP-only. No machine-type choice changes that contract.

`run_declaration` is required typed data with no deadline/budget defaults: UTC `work_deadline`, `hard_deadline`, positive integer `cloud_cleanup_reserve_seconds`, positive USD `budget_usd` and `abort_estimate_usd <= budget_usd`. Hard end includes the declared cloud cleanup reserve. The output is `seaweed.provision-declaration/v1` bound to run/artifact identity, not a watchdog, authentic approval, inventory deadline, price check or spend guarantee. Cloud cleanup reserve differs from the S0a process reserve. Static validate does not test live value enforcement. The future reviewed controller must bind exact config/saved-plan/declaration/inventory bytes to current authentic S0b authority and fixed work/cleanup/budget limits; data cannot enlarge those limits.

Inventory deadline/service ports retain #11's ratified deferral: typed declarations, exact-preserving Join and producers/consumers must land before runtime v1 freeze/pack activation. The fixed firewall service list is not an inventory service-port declaration. Docker Gate 0a and VF Gate 0b precede future cloud activation. No container/VF implementation or S0b adapter is included.

Freeze the same raw export as Azure, **only after a separately authorized future run**:

```sh
terraform -chdir="$TF_ROOT" output -raw inventory_json > "$RUN_DIR/requested.json"
seaweed-inventory --json validate "$RUN_DIR/requested.json" > "$RUN_DIR/validate.json"
sha256sum "$RUN_DIR/requested.json" > "$RUN_DIR/requested.sha256"
terraform -chdir="$TF_ROOT" output -json run_declaration > "$RUN_DIR/declaration.json"
terraform -chdir="$TF_ROOT" output -json managed_resource_ids > "$RUN_DIR/resource-ids.json"
terraform -chdir="$TF_ROOT" output -json resource_bindings > "$RUN_DIR/bindings.json"
# Trusted observer (not this module) binds exact request bytes and actual facts.
seaweed-inventory --json join "$RUN_DIR/requested.json" "$RUN_DIR/resolved.json" > "$RUN_DIR/join.json"
```

A nonzero command stops the handoff. Do not feed quoted `output -json inventory_json` to Decode or transcode/pretty-print raw request bytes after observation. Decode/Join grants no authority. No independent trusted observation or byte-path proof is fabricated in this Draft. The accepted [Azure export contract proof](https://github.com/pingqiu/sw-test-runner/blob/8b47e8701aa6bfc15c77092a50c58f6d88652523/provision/terraform/tests/export_contract.py) covers raw Terraform string serialization using synthetic resource-free local state; it does not establish these modules' live post-apply values or API behavior.

## Static checks now; plan gate later

```sh
terraform fmt -check -recursive provision/terraform
terraform -chdir=provision/terraform/gcp init -backend=false -input=false -lockfile=readonly
terraform -chdir=provision/terraform/gcp validate -json
terraform -chdir=provision/terraform/gcp-validation init -backend=false -input=false -lockfile=readonly
terraform -chdir=provision/terraform/gcp-validation validate -json
```

Those are the only executed checks. After S0b-min and explicit plan-only authority, a reviewed Linux controller may initialize private state/config and run a bounded saved **plan only**, e.g. the payload `terraform plan -input=false -out=reviewed.tfplan`, then `terraform show -json reviewed.tfplan` and hash both for review. All credential/account/network/image/price/quota and fixed-deadline checks remain prerequisites. Native JSON diagnostics do not grant authentic GO; stable operation/error/remedy handling belongs to the reviewed controller, not a new wrapper here. No apply/destroy entrypoint or automatic paid action is provided. Plan/API success is NOT_OBSERVED, not a fake PASS or a claim that missing credentials work.

## Recovery and absence remain launch gates

Retain private state, original UNKNOWN output, exact intended resource IDs and operation journal. On partial/UNKNOWN creation, stop creates and reconcile state, provider operations and independent full live inventory; missing outputs/empty state do not prove no billable resources. Never blind-retry an uncertain create, import a replacement to hide uncertainty, or delete a broad project/VPC by tag.

Future teardown must bind exact current generations and attachment/cascade targets immediately before effects, settle operations and run a separate full-ID plus run-scope inventory absence verifier while preserving existing account/project/network/IAM. Costs include disks/public IPs/network tails until proven absent; terminating VMs alone is not full teardown. A stale Terraform state or raw destroy command is not an executor fence. These recovery/identity-safe cleanup/provider-absence paths are unimplemented and **unqualified**. The [accepted Azure module/controller lifecycle split](https://github.com/pingqiu/sw-test-runner/blob/8b47e8701aa6bfc15c77092a50c58f6d88652523/provision/terraform/azure/README.md#future-lifecycle-and-recovery-procedure-disabled-now) documents shared responsibilities, not authorization or a cloud-independent implementation. There is no session-death deletion guarantee.
