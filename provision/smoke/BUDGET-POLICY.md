# Budget gate: per-project caps and an approved override (design note)

Status: local design, not yet reviewed. No cloud run.

## Today

`inputs()` in `smoke.go` hard-codes one budget for every run:
`prior_estimated_usd + estimated_usd <= 10`, `run_declaration.budget_usd <= 3`,
`abort_estimate_usd <= budget_usd`, `estimated_usd <= abort_estimate_usd`.
There is no way to give a project a different cap, or to let one run go
above $3 with an explicit approval.

## Change (small)

Where the numbers live: the controller config (`Config`, the private JSON
passed with `--config`). The pinned tfvars is not used, because its
`run_declaration` is a typed Terraform object and extra keys would reach
Terraform. The config is already the operator's file that carries
`estimated_usd` and `prior_estimated_usd`, and it is saved verbatim in
`recovery.json`, so destroy/verify re-check the same caps.

New optional config keys:

```json
"project": "capsule",
"budgets": {"capsule": {"per_run_cap_usd": 200, "cumulative_cap_usd": 1000}},
"budget_override": {"message_id": "chat-6aef0000", "amount_usd": 250,
                    "approver": "owner", "message": "approve one 250 USD run"}
```

Rules:

1. No `project` -> project `default` = today's caps exactly ($3 per run,
   $10 cumulative). `default` is built in and cannot be redefined in `budgets`.
2. `project` set -> it must be a key of `budgets`; both caps must be > 0 and
   `per_run_cap_usd <= cumulative_cap_usd`. Unknown project refuses.
3. `budget_override` (optional) must have a non-empty safe `message_id` and a
   finite `amount_usd` greater than the project's per-run cap. It raises this
   run's per-run cap to `amount_usd`. Anything malformed refuses. `approver`
   and `message` are optional text. The override object is copied verbatim
   (raw bytes) into the evidence: `plan.budget.override` in plan mode and
   `budget.json` in the run's output directory.
4. Cumulative: `prior_estimated_usd` keeps today's meaning (caller-supplied
   prior estimated spend), now read as the prior spend **of this project**.
   The check stays `prior + estimated_usd <= cumulative_cap_usd`. An override
   does not raise the cumulative cap: the approved run's estimate counts toward
   the project's cumulative cap like any other run, so an override that does
   not fit in the remaining cumulative budget still refuses.
5. Unchanged: `abort <= budget_usd`, `estimate <= abort`, region westus2,
   size Standard_D2s_v5, the ten-node cluster layout, deadlines, pins.
6. Every refusal is `INPUT_REFUSED` before any cloud command, with a distinct
   error text per reason.

Not in this change (deliberately): per-project regions / VM sizes / VM counts /
approver lists, a pinned policy file, a spend ledger, period windows. The
caller still states prior spend; the period it covers is the caller's
responsibility, as today.
