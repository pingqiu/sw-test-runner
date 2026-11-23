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

## Mid-flow heal and bounded fallback

Normal work keeps the sealed d-heal phase after takeover and before noresume
and restore. Include `scenarios/go2/shared/heal.yaml` among the finalization
phases: it calls the existing nftguard heal and absent directly on both hosts,
never starts d-heal again. Its host.sh bounded wrapper enforces the admitted
release deadline on the executing host; action timeouts bound the controller
wait. No failed or unreadable absence check is converted into success.

The parent scenario must supply validated run-owned m01_packet/m02_packet
paths and the admitted release_deadline as flat runner variables. It must set
its shared always_timeout and keep teardown, closeout, evidence and
END-or-quarantine in later bounded always phases. This shared include is only
the healing piece, not a replacement for those finalizers or their proofs.

`TestGO2HealFallbackAfterAbortAndNormalHeal` runs the actual engine and shared
YAML with a recording host model. An ordinary abort after block skips normal
heal but the finalizer heals and checks both hosts. After normal heal the same
fallback performs no deletes and still proves modeled absence. Removing
always makes the abort case fail with both modeled tables left present.
This is controller-order evidence, not a real nft, unit or lab qualification.

The current reference is recorded in CHECKER-REFERENCE.json (packet seal
776a1fb7). It is a reference pin, not a pilot approval or an assertion that
the seal has completed review. The gate call is build/gate.py check GATE.json,
after the packet seal and SHA256SUMS check; it is not another data judge.

Each exec command receipt contains the actual exit code, stdout, stderr and
complete/incomplete transport outcome. The evidence_hashes action hashes the
explicit collected output files and command receipts through sha256sum. Run
it in bounded finalization after collection even when the checker failed;
missing or unreadable files fail the action. Preserve its receipt in the
bundle. A hash authenticates bytes only: it never makes an incomplete command
answer complete or a failed checker pass. No hash action approves cleanup or
release. The full scenario must wire these output paths and finalization.
