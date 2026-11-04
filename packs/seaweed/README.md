# Seaweed plans and direct execution

Seaweed execution uses `seaweed-cloud direct` for existing hosts and the
cloud plan/run/destroy entry for cloud VMs. See the repository README examples.
The Seaweed `approved-run` CLI and mutating registry actions are retired.
Installed S0b trees and historical packets are not changed or deleted.

`RegisterPack` retains only `seaweed_inventory_validate` and `seaweed_plan`:
local data-only actions, even when the caller permits mutations. Former
preflight/deploy/workload/collect/cleanup/read-restart/soak action names are absent.

`seaweed-pack plan` and `seaweed-pack profile` remain data-only APIs. Typed plan
results use `PLAN_DATA_ONLY`, `execution_authorized=false`, and no effects.
Inventory-only planning asks for explicit plan input (`PLAN_INPUT_REQUIRED`).
Neither a plan nor a profile authorizes execution or reads live host state.

Shared profile/job producers, strict binding/verdict/clock leaves, payloads,
capture/oracle helpers and fault-window readers remain available to direct.
The generic runner approved/RunScope/custodian framework is unchanged.

[M01/M02 close-out](M01-M02-CLOSEOUT.md) and the earlier S0b documentation are
historical evidence pinned to their original heads, not current launch instructions.
