# KV receipt oracle (migration piece D1a)

This is a pure, in-memory port of the correctness state in the frozen
`soak_oracle.py`, with an explicit fence against a late ACK after an uncertain
write. No runner action is registered and no legacy entrypoint is replaced.

```text
independent planned content -> BeginWrite -> matching sequence/length/FIDs -> ACK
ACK -> BeginRead -> observed length/SHA256 -> hit
latest ACK's volumes independently retired -> 404 -> retired_miss
uncertain write -> retained pending intent -> no read/retry/late ACK promotion
```

The oracle retains hashes and metadata, not the written dataset. A single run
owns an instance. Per-key guards span calls; independent keys can progress
concurrently. FID slices returned to or supplied by a caller cannot mutate the
stored version. An overwrite changes the expected content and backing volumes.
Retirement of the old version cannot excuse a miss or wrong bytes on the new one.

The caller must bind operation identity, derive expectations before dispatch,
obtain and validate independent filer ACK metadata, durably record ACKs, collect
retirement evidence and stop on errors. `Content` is a trusted worker observation,
not a server-supplied expected hash. `RetirementEvidence` carries assertions from
the future collector; the oracle does not discover or prove them itself.
The filer metadata timestamp/version check from `validate_ack_metadata`, live
receipt decoding, worker lifecycle, fault-error attribution, rate windows,
resource gates and terminal counters are later pieces. This package alone must
not be used to claim those gates or enable lab execution.

## Controls

`go test ./packs/kv/...` checks same-key exclusion, exact latest content,
sequence/length/FID binding, all-volume retirement, defensive copies, independent
keys and unknown-outcome fencing. The uncertain-write test models a possibly
committed write with a lost response at the oracle boundary; an actual worker
transport/commit-loss integration control remains required in D2.

`testdata/*-receipts.jsonl` are ten-row subsets from each of accepted v2
`132324Z` and failed v3A `195756Z`. They retain two planned writes, independently
resolved ACK metadata/journal rows and two guarded native read completions.
Expected bytes are regenerated from the dispatched seeds, not learned from ACKs
or GETs. The provenance files name source lines, exact original-line hashes,
archive digests and original review/verdict limits. Only the selected rows are
replayed. Their success is not full retained-bundle parity, native-transfer
qualification, or permission to change the failed v3A verdict. A modified read
digest must fail the same replay.

No Python file, native binding, shared runner infrastructure, queue admission or
hardware setting changes in this piece. No Python retirement precedes retained
parity, review and the explicit owner cutover.
