# Shared checker leaves

`facts.py` owns `read_ledger`, `lease_bound` and `ack_window`. Packet adapters
call these entries; they do not retain a second structural or timing predicate.
The library uses only Python's standard library and performs no host actions.

`read_ledger(bytes, mode)` returns records keyed by sequence, the exact judged
snapshot bytes, the last ACK and a metrics dictionary with `acked`, `exact`,
`chainbreaks`. LIVE ignores only an incomplete final line; FINAL requires
termination. Complete malformed rows, duplicate blocks and unmatched terminal
records raise `ValueError`. Intents start at zero and advance by one; each resolves before the next
intent. Monotonic timestamps never decrease. One final unresolved intent
remains valid evidence in either mode.
Metrics are not a PASS verdict. Classification and image verification stay in
the calling packet in this extraction; they consume the shared records.

`lease_bound(journal)` preserves the logged lease-bound interpretation and
returns `(bound_ns, cause, error)`. Missing evidence returns an error, never an
invented bound. The last renewal-failure bound is a fallback only for a
failed-renewal expiry fence; revocation and other causes return an error. `ack_window(record, bound_ns, drain_ns)` returns `inside` only for
an intent strictly before the bound and completion no later than bound + drain;
otherwise it returns `requested_after_U` or `completed_after_drain`. Callers must not treat missing
lease evidence or guest-kernel success as a target durability proof.

`loader.load_library(root, manifest_sha256)` requires the exact manifest pin,
checks every declared code file and executes the verified facts bytes. There
is no ambient import fallback. The loader itself must be authenticated by the
calling packet before it is executed; its hash is also part of `library.json`.
A packet's own seal must cover its bootstrap, configuration and library digest.
`library.sha256` is the repository's current manifest digest. Any code update
requires new file hashes, a new manifest digest and a new packet seal. Old
packets and evidence keep their previous versions.

Run `python -m unittest discover -s checkers -p 'test_*.py'`. Synthetic controls
exercise duplicate-block and orphan-ACK rejection, FINAL/LIVE tails, ACK
boundaries, missing lease evidence and modified/missing pins. Eight isolated
source mutants demonstrate the parser ordering and lease/ACK controls
are sensitive to their deciding conditions. No packet contents, captured
artifacts, host addresses or credentials belong in this directory.
