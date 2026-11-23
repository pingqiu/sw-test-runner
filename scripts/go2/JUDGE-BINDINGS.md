# One Python judge, called through exec

`checker_commands.py` renders ordinary runner `exec` actions calling the
sealed pack's `lsncheck.py` and `summarize-go2.py` directly. It does not load,
register or call Go judge actions. There is no second verdict implementation,
Go/Python parity check, controller-derived LSN, or verdict-to-PASS conversion.

The host phases retain their existing `ledgerscan.py` calls and closeout
table. A bare ledgerscan exit 0 reports metrics; it is not a row PASS. Those
metrics stay under the existing phase's checks. The runner must preserve
the phase receipts and artifacts consumed by `summarize-go2.py`, including
the exact state, readiness and M01/M02 closeout strings. A command receipt
records dispatch/result evidence only and is not a judge receipt.

Image arguments are the boundary and raced-row decision already produced
by the pack, or the snapshot's own cutLSN. The binding never substitutes a
controller timestamp, infers an LSN or reinterprets the final row exception.
All paths refer to collected files on the named POSIX controller node.
Arguments are shell-quoted individually. Receipt directories must exist and
each receipt path must be fresh before dispatch.

Nonzero checker exits fail the exec action, including NOT_COUNTED. No
`ignore_error` or `allow_not_counted` is emitted. A rehearsal exception must
be decided in the pack's reviewed rehearsal template, not in this binding.
The pack's summarizer remains the sole source of the rep verdict. It belongs
in bounded always/finalization after evidence collection; healing, teardown,
closeout, evidence and END-or-quarantine also require that bounded path.

This is a command binding component, not a runnable two-host scenario or a
rehearsal approval. Run `python3 -m unittest discover -s scripts/go2 -p
'test_checker_commands.py'` for local argument/receipt-path controls.
