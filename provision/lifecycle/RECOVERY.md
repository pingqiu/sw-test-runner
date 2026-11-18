# Destroy / recovery procedure (seaweed-cloud)

One page. Commands run on the Linux controller, as the controller user, with the
private config and run directories. `destroy`, `verify` and `cleanup` never need a
launch token: recovery is never blocked. Only `run` needs one.

## 1. Read the result code

| code | resources may exist? | action |
|---|---|---|
| `LAUNCH_REFUSED`, `COST_REFUSED`, `RECOVERY_REQUIRED`, `INPUT_REFUSED`, `DEADLINE_REFUSED`, `OUTPUT_PARENT_REFUSED` | no (refused before any cloud call) | fix the input; for `RECOVERY_REQUIRED` do §3 for every listed run first |
| `PLAN_FAILED`, `RG_NOT_EMPTY` | no new ones (before apply) | `RG_NOT_EMPTY`: someone else's resources are in the RG; stop and ask the RG owner |
| `COMPLETE` | no (destroy + independent empty list) | none |
| `PACK_FAILED`, `COST_ABORTED`, `INVENTORY_FAILED`, `EVIDENCE_FAILED` | check `outcome.json` | clean outcome = done; otherwise §3 |
| `APPLY_FAILED_OR_UNKNOWN`, `CLEANUP_INCOMPLETE` | yes | §3 |
| `CREATE_UNRESOLVED` | maybe, later | §4 |
| process exited 3 (`watchdog.json` written) | yes | §3 now |
| controller/session died (no result) | yes | §3 before any new run |

## 2. The run directory (the config's `output`)

- `launch.json` — the LAUNCH token the run was admitted under.
- `cost.json` — hourly rate, abort estimate, budget, cost and work deadlines.
- `apply-started` / `apply-succeeded` — whether creates may have been issued.
- `outcome.json` — the final state after this invocation: **clean** means
  `remaining_observed: true`, `remaining_resource_ids: []`, `create_unresolved: false`.
- `watchdog.json` — the controller was still running past its deadline + grace.
- `recovery.json` (ten-node runs) — the snapshot `cleanup --run` destroys from.

Keep the whole directory until the outcome is clean: it holds Terraform state and
private host keys. Never copy it to shared storage.

## 3. Destroy until clean

```sh
# one-VM run: the same config the run used
seaweed-cloud destroy --config /private/config.json --json
# ten-node run: by run ID, from its saved snapshot
seaweed-cloud cleanup --run <run-id> --json
# then, independently of Terraform state
seaweed-cloud verify --config /private/config.json --json
```

Repeat `destroy` (same state, no re-apply) until `outcome.json` is clean and
`verify` reports an empty list. Each destroy is bounded (15 min) and has its own
watchdog. If a destroy keeps failing, report the remaining IDs from `outcome.json`
to the RG owner; do not delete them by hand outside this tool.

## 4. CREATE_UNRESOLVED

An apply whose create result was lost may still complete later; destroy plus an
empty list cannot prove otherwise. Keep the run directory, run §3, and ask the
operator to resolve the pending provider operation. Only then is the run settled.

## 5. After a controller or session death

The next `run` refuses with `RECOVERY_REQUIRED`, naming every earlier guarded run
that started an apply without a clean outcome. Run §3 for each, then retry. If the
watchdog fired, the tool's process groups were killed with it; confirm no
`terraform`/`az` process of that run remains before §3.

## Never

Delete or retag the resource group, re-apply to "fix" a run, edit Terraform
state, or remove a run directory whose outcome is not clean.
