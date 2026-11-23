# Artifact-only judge bindings

`judge_bindings.py` renders the accepted `hostloss_row1`, `hostloss_final`,
`hostloss_image`, `hostloss_lostrange` and `hostloss_stopbound` action maps.
It performs no SSH, clock sampling, product judgement or rep counting.

Execute `initial_actions` after the immutable live ledger snapshot and move
outputs have been collected. Execute `followup_actions` only after the initial
judges have written their confirmed receipts and the whole stopped-writer
ledger, full image and loss receipt have been collected. All references are
artifact file paths; no stdout-cap transport. Image L comes from the confirmed
lostrange receipt, or `snapshot_image_action` reads that snapshot's `cutLSN`.
The summarizer must read the judge receipts, never infer counts from an action
status. Explicit receipt paths avoid reliance on a pending action output fix.

The loss-adapter record binds victim host, boot id, unit and invocation and
declares `clock_domain: host`. KB requires positive `kill_issued_ns` and a
separate later/equal `confirmed_dead_ns`; PO requires `fence_sec`; exactly one
loss clock is accepted. A declaration alone does not prove origin: the provider
must produce it on the host, joined to the collected raw record and admission
identity. A controller timestamp is refused. Legacy `KILLED-* t_ns` alone is
refused: the old script writes it after systemctl kill returns, so it cannot be
relabeled as a pre-dispatch kill-issued timestamp. The forthcoming host-side
unit_kill provider must supply the two distinct stamps.

`allow_not_counted: true` is emitted only for an explicitly rehearsal template,
a rehearsal run-id suffix, and literal zero release credit. Counted templates
never receive that exception. This library is not a runnable drill or an
execution entry and cannot approve a pilot. Until the judge action pack lands,
the executable controller must keep the unchanged script checker calls.

Run `python3 -m unittest discover -s scripts/go2 -p 'test_judge_bindings.py'`.
Tests exercise clock/identity mismatches, legacy/confirmed-dead stamp misuse,
exclusive clock selection, unconfirmed range, counted-template refusal and
snapshot-local L. No host or ledger resource is touched.
