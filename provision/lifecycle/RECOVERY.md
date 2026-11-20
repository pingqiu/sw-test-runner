# Destroy / recovery procedure (seaweed-cloud)

One page. Commands run on the Linux controller, as the controller user, with the
private config and run directories. `destroy`, `verify` and `cleanup` never need a
launch token: recovery is never blocked. Only `run` needs one. The token is the
manager-issued LAUNCH message id, checked for shape and recorded; it is an
operational contract, not authentication of an approval or of its scope.

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
- `outcome.json` — the final state this invocation recorded: **clean** means
  `remaining_observed: true`, `remaining_resource_ids: []`, `create_unresolved: false`
  and a code other than `CLEANUP_INCOMPLETE` (a failed destroy is never clean, even
  if the list then came back empty). It is the tool's own record, not independent
  proof beyond that list.
- `watchdog.json` — the controller was still running past its deadline + grace and
  exited 3. The exit itself does not prove the tool processes are gone (see §5).
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
`verify` reports an empty list. This settles every case except an unresolved create
(§4), which repetition never clears. Each destroy is bounded (15 min) and has its own
watchdog. If a destroy keeps failing, report the remaining IDs from `outcome.json`
to the RG owner; do not delete them by hand outside this tool.

## 4. CREATE_UNRESOLVED

An apply whose create result was lost may still complete later; destroy plus an
empty list cannot prove otherwise, and `create_unresolved` stays true no matter how
often destroy or verify run, so this run keeps blocking new runs
(`RECOVERY_REQUIRED`). It is a manual hold: keep the run directory, run §3, and ask
the operator to resolve the pending provider operation. Only after the manager
records that settlement may the run directory be archived out of the `runs/` parent;
nothing in the tool settles it automatically.

`seaweed-cloud settle --resource-group RG --vm VM [--subscription UUID]` performs
a read-only observation of the last hour's activity, RG resources and disks.
See [settle output](../smoke/README.md#read-only-settlement-observation) for its
three classifications. It does **not** clear this hold or release an RG claim.

## 5. After a controller or session death

The next `run` refuses with `RECOVERY_REQUIRED`, naming every earlier guarded run
that started an apply without a clean outcome, and any run whose markers could not
be read (unknown, never treated as absent). The gate scans only the sibling
directories of the new run's output: keep every run of a controller under one
`runs/` parent. Run §3 for each, then retry. The watchdog's exit does not kill the
tools' process groups by itself (deadline cancellation does that separately while
the controller lives), so confirm no `terraform`/`az` process of that run remains
before §3.

## Cost

`abort_estimate_usd` at `hourly_usd` sets when the work is cut (`COST_ABORTED`);
cleanup then runs until the hard deadline. It is not an enforced maximum total
bill: billing continues if cleanup fails, until §3 succeeds. The admission check
bounds `hourly_usd` until the hard deadline by `budget_usd`.

## Never

Delete or retag the resource group, re-apply to "fix" a run, edit Terraform
state, or remove a run directory whose outcome is not clean.
