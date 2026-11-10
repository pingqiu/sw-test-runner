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

## Stage 3 fault rows (R0, F1-F7)

A soak fault row may carry `kind` (absent is `restart`, unchanged) and a
frozen `fault_row` block. Rows run only under `seaweed-cloud direct`; the
injectors and the resource sampler are `worker/soak_fault.py` on M02.

**Layout scope.** A row admits exactly one volume process, on M02, and every
row verdict says so (`layout_scope`). F1 attributes an UNKNOWN PUT to the
volume it killed, and a failed PUT does not record which volume it targeted, so
the attribution holds only with one volume. No row result covers multi-volume
layouts, volumes on other hosts, or a kill that leaves other volumes serving.

Outcomes are PASS, FAIL or NOT_COVERED; a missing witness is NOT_COVERED,
never PASS. F1's resources are NOT_COVERED by design (its replacement has no
pre-load sample to join), and an UNKNOWN PUT outside F1 and F2 stops the run,
so F6 can pass only through a named REFUSED class.

[M01/M02 close-out](M01-M02-CLOSEOUT.md) and the earlier S0b documentation are
historical evidence pinned to their original heads, not current launch instructions.
