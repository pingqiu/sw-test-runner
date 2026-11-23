# G-O2 controller extraction, step 0

`split_controller.py --packet REFERENCE --output FRESH_OUTPUT --launch-sha256 SHA`
materializes a review-only split of a pinned existing G-O2 packet. The reference
is never edited. Every host safety script and judge stays byte-for-byte intact.
The generated `CONTROLLER-SPLIT.json` records the input pin and every output file.
The output is explicitly UNSEALED; this command neither approves nor reseals it.

`preflight.sh` holds the original local pre-claim and local validation section.
`finalize.sh` holds the original closeout section, including the M02-before-M01
ordering, positive closeout requirement and END/release versus quarantine rules.
They are sourced Bash modules with the compatibility launcher's original state;
this deliberately preserves its guards and exit semantics in step 0. They are
not standalone arbitrary-host executors. The retained launcher calls them at
the original boundaries. Dead T1 observation readers and their call sites are
removed; G-O2 host phase logic and judges are unchanged. Unknown source anchors
or observation call shapes refuse instead of guessing a refactor.

Step 1 must expand *each* remote call into ordinary YAML actions. Calling the
compatibility launcher, or its whole remote finalizer, with one `exec` does not
make the runner the controller and earns no custody or release claim. The host
InvocationID records, dead-men and absence proof remain authoritative. Every
heal/teardown/closeout/collection/END-or-quarantine action must be in a bounded
always-phase, not an `ignore_error` ordinary phase.

Run `python3 -m unittest discover -s scripts/go2 -p 'test_*.py'`. The transcript
control executes Bash only against temporary local fake commands; no lab host
or ledger is touched. `BASH_FOR_TESTS` can select the local Bash executable.
A missing Bash skips that control and is not transcript qualification.
