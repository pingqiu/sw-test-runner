# Shared checker leaves

`facts.py` owns `read_ledger`, `lease_bound`, `ack_window`, `classify_write`
and `image_exact`. Packet adapters
call these entries; they do not retain a second structural or timing predicate.
The library uses only Python's standard library and performs no host actions.

`read_ledger(bytes, mode)` returns records keyed by sequence, the exact judged
snapshot bytes, the last ACK and a metrics dictionary with `acked`, `exact`,
`chainbreaks`. LIVE ignores only an incomplete final line; FINAL requires
termination. Complete malformed rows, duplicate blocks and unmatched terminal
records raise `ValueError`. Intents start at zero and advance by one; each
resolves before the next intent. Blocks advance as `first_block + seq`,
rejecting gaps, reversals and duplicate blocks. Monotonic timestamps never
decrease. One final unresolved intent remains valid evidence in either mode.
Metrics are not a PASS verdict. Classification and image verification consume
these shared records; packet adapters must not keep another classifier or
image verdict predicate.

`block = first_block + seq` is a property of the current sequential load
generator, not of every well-formed ledger. A load that wraps or restarts at
another base must declare that behavior as sealed policy. This reader currently
supports only the sequential policy above; a different policy needs a reviewed
implementation. Any change to an accepted fact definition requires
fact-semantics review before merge.

`lease_bound(journal)` preserves the logged lease-bound interpretation and
returns `(bound_ns, cause, error)`. Missing evidence returns an error, never an
invented bound. The last renewal-failure bound is a fallback only for a
failed-renewal expiry fence; revocation and other causes return an error. `ack_window(record, bound_ns, drain_ns)` returns `inside` only for
an intent strictly before the bound and completion no later than bound + drain;
otherwise it returns `requested_after_U` or `completed_after_drain`. Callers must not treat missing
lease evidence or guest-kernel success as a target durability proof.

`classify_write(record, boundary)` consumes a record validated by `read_ledger`
and returns `must`, `lost`, `lsnless`, `either` or `straddle`. An ACK needs
H1 = H0 + 1 to be exact. It uses LSN ordering, never wall-clock timestamps.
`classification_counts(records, classes)` only aggregates those decisions.

`image_exact(records, image, boundary, blocks, block_size=4096, raced='none')`
consumes validated records and a binary image stream positioned at byte zero.
It returns a named `verdict` (`PASS`, `FAIL`, `NOT_COUNTED`), `counts`, `errors`
and diagnostic `info`. Required writes must be byte-exact; writes above the
boundary and unwritten blocks must be zero. Failed/in-flight writes allow
exact bytes or zero. Unresolved ACK LSNs cannot produce PASS unless the caller
supplies the explicit matching final-row raced-write decision; only the last
ACK with known H0 and unknown H1 qualifies. Short/oversized images, out-of-range
blocks and torn content fail. Read failures propagate; invalid image geometry
raises `ValueError`. No path is opened and no host action occurs in the library.
Packet adapters own argument parsing, printing and exit-code formatting only.

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

Image controls cover kept/lost bytes, uncertain writes, unwritten blocks,
geometry, read errors and the explicit last-ACK exception. Two additional
source mutants classify an inexact ACK as exact and ignore missing required
bytes; both disagree with their ordinary negative controls.
