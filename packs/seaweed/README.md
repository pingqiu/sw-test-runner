# Shared Seaweed deployment pack skeleton

One pack will consume the same `seaweed.inventory/v1` seam for Azure, AWS, GCP and on-prem. Provisioning is outside this pack. This Draft deliberately stops at the S0b-min boundary: there is **no deploy, preflight, workload, collection or cleanup implementation**, and no compatibility path through ordinary `NodeRunner.Run`.

## Current behavior

`RegisterPack(registry)` explicitly registers these names; the CLI does not enable this skeleton automatically:

| Action | Current outcome |
| --- | --- |
| `seaweed_inventory_validate` | Pure in-memory Decode of `inventory_json`; returns JSON `valid_data_only`, `execution_authorized=false`, state and transport. No file, network or credential access. |
| `seaweed_plan` | Read-only plan for `for_action` plus `inventory_json`; returns `blocked_plan`, `S0B_UNAVAILABLE`, zero executable effects and no remote calls. |
| `seaweed_preflight` | `ErrS0bUnavailable`, structured refusal and no remote calls |
| `seaweed_deploy` | Same refusal |
| `seaweed_workload` | Same refusal |
| `seaweed_collect` | Same refusal |
| `seaweed_cleanup` | Same refusal |

The five runtime actions are conservatively registered as mutating. Even with all tiers and mutating enabled, nil context, supplied nodes, a valid inventory or caller-provided `go`/`s0b_admitted` variables, they refuse. Neither YAML nor a passing inventory validator can enable them. Refused cleanup cannot be relied on to remove resources from an independently launched run: no such run is launched by this skeleton.

## Interface to be bound after S0b-min lands

The accepted S0a/S0b implementation owns authoritative admission and job/process settlement. This pack does not define an alternative token, boolean enable switch or executor. After that API lands, a separately reviewed adapter must bind:

1. Requested/resolved inventory byte hashes and scope-preserving join; trusted host keys, boot generations, data NIC/MTU, exact disks and formatting authorization.
2. Authoritative GO and policy, host-scoped lease, exact scenario/config/artifact/interpreter/runner bytes, work/hard deadlines and cleanup reserve; revoked/expired/mismatched evidence refuses before any job or upload.
3. Pinned master/volume/filer/S3 deployment under the declared run root. Reuse existing workload/oracle/resource collectors; do not invent per-cloud deployments or new lifecycle machinery.
4. Required RC/DC device, port, GID, MTU and fabric evidence. HTTP is explicit. Missing required transport refuses; there is no automatic RC/DC-to-HTTP fallback. Azure HB/ND InfiniBand, EFA SRD and selected GCP RoCE are distinct capabilities, not interchangeable claims.
5. Existing bounded restart/fault helpers, process generation enrollments and closures, ACK-before-overwrite checks and evidence exports. Filer uses accepted r6r3 warm-RSS timing; master preserves r5. Missing members/generations/terminal evidence fail; no fake resource PASS.
6. Cleanup limited to admitted run ownership, identity checked before effects, settlement followed by independent absence proof. Cloud resource destroy remains the provision/controller responsibility, never an arbitrary shell action derived from inventory.

The phase names reserve the seam, not a promise that a specific S0b API shape already exists. No placeholder evidence authorizes execution.

## Delivery sequence and CPU proof

Inventory is Draft #11; this Draft is stacked on its branch and changes only this pack. Azure provisioning is independent Draft #13. Local Docker Gate 0a and later VF Gate 0b are design-r2 follow-ups, not implemented here. Gate 0a has HTTP first and DC N/A; its container identity bindings and Gate 0b actual CX5 evidence must be reviewed after S0b-min. No Docker, VF, cloud or lab activity accompanies this Draft.

`go test ./packs/seaweed ./inventory ./cmd/seaweed-inventory` and `go vet` exercise the public Registry boundary. Ten phase/context cases require the sentinel refusal even with permissive registry flags; a node implementation panics on any Run/RunRoot/Upload/Close. Data-only decode has a positive and malformed-input controls. The pre-fix no-refusal stub is RED; the refusal implementation is GREEN. Removing the sentinel refusal reproduces the RED. These are CPU boundary controls, not deployment qualification.

## Machine output and plan contract

Every handler returns `seaweed.pack-result/v1` JSON in the Registry `value` key, which the existing engine retains in `ActionResult.Output` even on failure. Stable fields are `schema`, `status`, `code`, `remedy`, `action`, `execution_authorized` and `effects`; data/plan success adds `state` and `transport`. Runtime errors also begin with their stable code for human logs. No raw inventory, secret or endpoint is echoed in this envelope.

| Code | Remedy |
| --- | --- |
| `OK` | Data is valid only; wait for reviewed S0b admission before execution. |
| `INVENTORY_INVALID` | Correct input against the schema and preserve requested scope. |
| `ACTION_UNSUPPORTED` | Choose a registered Seaweed runtime phase for planning. |
| `S0B_UNAVAILABLE` | Wait for the reviewed adapter; do not retry or bypass admission. |

Call the read-only `seaweed_plan` with `for_action` equal to any runtime phase and the exact `inventory_json`. It needs no mutating registry permission. Because no phase implementation exists, the accurate current plan is `blocked_plan`, `effects: []`, `execution_authorized: false`; it is not a deploy preview or approval. The separately reviewed runtime implementation must replace this blocked plan with exact fenced changes before any effect is enabled. CPU controls exercise every phase's plan and malformed/unsupported refusals; all use the same panic-on-remote-access node.
