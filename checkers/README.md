# Shared checker leaves

`facts.py` owns `read_ledger`, `lease_bound`, `ack_window` and `resources_absent`. Packet adapters
call these entries; they do not retain a second structural or timing predicate.
The library uses only Python's standard library. Pure fact functions perform
no host actions; the separate authenticated absence CLI observes local
inventories read-only and performs no cleanup, SSH or ownership operation.

`read_ledger(bytes, mode)` returns records keyed by sequence, the exact judged
snapshot bytes, the last ACK and a metrics dictionary with `acked`, `exact`,
`chainbreaks`. LIVE ignores only an incomplete final line; FINAL requires
termination. Complete malformed rows, duplicate blocks and unmatched terminal
records raise `ValueError`. Intents start at zero and advance by one; each resolves before the next
intent. Blocks advance from the first block as `first_block + seq`, rejecting
gaps, reversals and duplicate blocks. Monotonic timestamps never decrease. One final unresolved intent
remains valid evidence in either mode.
Metrics are not a PASS verdict. Classification and image verification stay in
the calling packet in this extraction; they consume the shared records.

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

`resources_absent(leftovers, errors)` returns a named `verdict` and `details`:
UNKNOWN if any inventory failed, otherwise FAIL for observed leftovers, else
PASS. Producer errors precede leftover details, preserving their order. Its
inputs must be the complete observations from the authenticated collector.

`resources_absent_cli(packet_file, arguments)` runs the shared read-only local
collector in `resource_inventory.py`, using the caller's run.env as data and
its packet location for the existing scope rules. It checks systemd units,
listening TCP ports, mounts, NVMe, loops, declared devices, processes and owned
roots. Each producer's own status is checked; failed reads stay UNKNOWN.
It preserves the absence CLI's exit codes and output. The loader binds the
single pure deciding function; packet adapters cannot supply another one.
The collector supports the existing synthetic inventory-root/systemctl seams;
loading the library alone performs no observation.

`loader.load_library(root, manifest_sha256)` requires the exact manifest pin,
checks every declared code file, then loads the verified facts and inventory
modules. The closure includes facts.py, loader.py, resource_inventory.py,
ops_rmguard.py, unit_properties.py, completion.py and power_loss.py. There
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

Absence controls cover complete empty inventories, leftovers, failed producers,
unreadable inventories, self-unit exclusion and usage. A targeted source mutant
ignores inventory errors and falsely returns PASS; the ordinary control returns
UNKNOWN. Loader controls reject every missing/modified closure member and a
stale published manifest pin.

Operational removal is separate from facts: `loader.load_rmguard(root, pin)`
verifies the same complete manifest and returns `run(packet_file, arguments)`
from `ops_rmguard.py`. Loading performs no deletion. `facts.py` never imports
this module. The packet location selects its existing run.env bindings;
canonical roots, directory-fd ancestry and identity, complete mount inventory,
and final identity checks retain their existing refusal and exit protocols.
The remover deletes the contents and the root itself only after all admission
checks pass. A mount at the root or beneath it refuses without deleting either.

Ownership-ledger and lock-holder helpers remain identical per packet. Their
stdin-before-staging and controller event-parser consumers must be addressed
with the launcher rework before those helpers can be shared.

Removal controls require POSIX directory-fd operations. Skipping them on another
platform is not qualification evidence; run them on a supported POSIX host.

## Fact boundaries

- Guest acknowledgement window reads a guest write record and lease/drain bounds;
  proves its timing classification; does not prove a revoke event or detached authority.
- Revoke-event completion reads the stop's revoke event and its outcome/timestamps;
  proves that event completed as recorded; does not prove the detached generation or guest acknowledgement window.
- Detached-authority proof reads the detach/revoke proof, authority and probe;
  proves the expected generation/epoch and probe evidence match; does not prove revoke-event timing or guest write durability.
- Unit inactivity reads current unit properties with checked producer status;
  proves the unit is not active now; does not prove the unit succeeded.
- Invocation outcome reads properties and manager records bound to a recorded invocation;
  proves this invocation ended with this result; does not prove the unit is gone.

Only an identical unit-property observation is shared here. Each packet retains
its inactivity/completion verdict and expected-exit policy. An expected exit in
one policy does not authorize accepting a failed unit in another packet.

`show_unit_properties(unit, timeout, systemctl)` reads one property set within
its caller's remaining producer budget. A failed process, timeout, missing
required property or empty load/active state raises `ValueError`; packet
adapters translate this to their existing UNKNOWN path. It returns properties,
never a PASS or completion decision. The existing reader represents an absent
ExecMainStatus as `-`, including for services; that marker is not a successful
exit status. Loading the pinned module does not invoke systemctl.

T1 repetition completion reads the parsed phase receipts, check outcomes,
readiness and host-clean observations of one repetition; proves that repetition's
verdict under T1's completion policy; does not prove the repetition complete
under another packet's policy. The online and maintained offline T1 callers
share this policy. The other packet's completion policy remains separate.

Known discrepancy: phase parsing keeps the last receipt for each phase, while
the prose says every phase failure fails; this verdict follows the parsed
last-write-wins mapping. This extraction preserves that behavior explicitly.

`rep_complete(phase_receipts, required_phases, check_rows, required_checks,
failed_checks, ready, state, host_states)` consumes the callers' parsed mappings
and preserves their missing/failed lists. It returns the verdict, only the
receipts that decided that verdict, and failed/missing/clean fields used by the
existing reports. Required phase obligations must be supplied; an absent
receipt never satisfies one. Readiness, failed checks, missing requirements and
host state all use the one total order in completion.py. Parsing and artifact
writes stay in the callers. Both reports include the deciding receipts as
additional evidence. This policy is not adopted by the other packet.

Corrected input-consistency weakness: the original verdict block trusted a
separate failed-name list while treating any present required row as satisfying
presence. The shared API now raises when that list disagrees with the rows'
FAIL statuses, including omitted failures and passing/absent named rows.
Consistent inputs retain their previous verdict; contradictory evidence cannot
produce PASS. This correction is separate from the explicitly preserved phase
last-write-wins behavior.

Check-row vocabulary: current producers emit PASS/FAIL verdict rows and INFO
reporting rows. Only PASS satisfies a required check; FAIL remains a failure,
and a present required row with INFO, UNKNOWN, SKIP, INCONCLUSIVE or any other
non-PASS/non-FAIL status yields INCONCLUSIVE. Optional INFO is not a verdict
input. Reporting-only rows belong outside the required PASS obligations;
merely observing a measurement is not evidence that a required check passed.

## Recovered and published image comparison

`power_loss_image_exact(case, workload, local, published)` reads complete,
bound observations of a recovered local view and its subsequent published
snapshot. It proves their whole-volume content agreement and the local
image's declared block-history obligations. It does not prove that a crash
image was built correctly, that a campaign contains a non-prefix subset, or
that a repetition is counted under a packet's policy. Those observations and
decisions stay with the packet, with authenticated artifact joins.

The result has `image_verdict`, `campaign_verdict`, an overall `verdict`,
three named `rows`, coverage `counts` and `deciding_evidence` identities.
Each verdict is PASS, FAIL or UNKNOWN. Overall FAIL takes precedence over
UNKNOWN; an independently established image failure is retained when the
tick proof is absent. Missing or contradictory image evidence gives UNKNOWN,
never inferred zeros or successful comparisons.

- `LOCAL_HISTORY`: a flushed replacement and every previously flushed baseline
  block must hold its flushed value (ACKED-LOST otherwise). An unflushed block
  independently permits OLD or NEW, never a third/torn value. Illegal values
  or bytes outside the declared history are UNEXPECTED. Multi-block write
  group atomicity is not required.
- `PUBLISHED_EQUALS_LOCAL`: compares full SHA256 values of every declared
  chunk, including quiet and zero chunks. A difference is DIVERGED even when
  both views independently contain legal OLD/NEW choices. No certification
  bitmap is derived or consumed.
- `CAMPAIGN_TICK`: on a revision without the write-ahead fix, requires a
  logged extent write of the unflushed block strictly between the write ACK
  and cut/crash marks. On a fixed revision, requires at least 250 ms of host
  time (two 100 ms periods plus 50 ms), with a successful status response
  strictly between those marks. A bound positive unsynced-extent count on a
  fixed revision is WRITE-AHEAD-VIOLATION, a campaign FAIL independent of the
  image verdict. Unknown proof is not a product-content failure.

### Producer interface

The entry takes parsed Python values, not paths. Adapters authenticate and
parse their input files, check the producer's status, bound reads, and decode
required raw evidence to `bytes`. The manifest pin authenticates the function;
the packet seal binds its revision policy and expected workload. The function
does not execute a producer or trust an ambient library import.

`case` carries `run_id`, `image_id`, `volume_id`, `revision`, distinct
`local_view_id`/`published_view_id`, and positive `volume_bytes`, `block_bytes`,
`chunk_bytes`. Chunks and volume lengths must be whole blocks. The reader's
per-chunk working-memory ceiling is 64 MiB; larger chunks return UNKNOWN.
The last chunk may be partial. `snapshot` contains an `attempt`, positive
`head_gen`, and nonnegative `cut_lsn`. `window` contains monotonically ordered
harness event ordinals `recovery_complete < local_complete < snapshot_complete`
and `guest_writes: 0`; these are order receipts, not cross-host clock readings.

`workload.baseline` is a sparse block-index to full block-bytes map of the
last flushed image; undeclared baseline blocks are zero. This is an explicit
sequential-generator policy for a fresh volume per case, not a fallback for
missing observations. Reused volumes require a complete declared baseline;
undeclared nonzero history is not supported.
`writes` holds unique `{block, new: bytes, flushed: bool}` replacements, at most
one per block in this crash window. `operations` permits write/flush/fua;
discard, write-zeroes and repeated writes are unsupported and yield UNKNOWN.
Replacements may overlap baseline blocks. The complete observed block set is
the union of baseline keys and replacement blocks, protecting earlier flushed
neighbors as well as the last unflushed writes.

Each view has a `source` (local or published), integer `producer_exit: 0`,
`binding`, `chunks`, `blocks`, and a coverage trailer. Bindings contain the
case's run/image/volume/revision and the appropriate `view_id`; each chunk
repeats that binding and source. The published view also repeats the exact
`snapshot`. One chunk record has `{chunk_index, offset, length, sha256}`:
ranges must cover the geometry exactly once; SHA256 is 64 lowercase hex
characters. The trailer is `{chunks, count, bytes, first, last}`, with both
counts equal to the number of chunks, bytes equal to volume length, and
inclusive byte bounds zero through volume length minus one. Short, extra,
duplicate, out-of-range or truncated-hash records yield UNKNOWN.

Every observed written block has `{block, sha256, data: bytes}`. For every
chunk whose local and published hashes differ, both chunk records must also
carry complete `data: bytes`. Optional raw chunk evidence is validated too.
All raw evidence must agree with its full hash and any overlapping raw block.
Equal chunk hashes with conflicting raw block observations are rejected.
Sparse expected bytes plus the observed written blocks reconstruct expected
changed-chunk hashes; zero-only chunk hashes are cached. Whole-volume raw
streams are not required. An abbreviated legacy hash/tag scan is not a
substitute for this interface.

`case.write_ahead_fixed` is the packet's sealed boolean revision policy.
`proof` carries the matching run/image/volume/revision `binding`, `revision`,
host `clock_id`, integer `write_ack_ns`, `cut_mark_ns`, and nonnegative
`extent_writes`. The unfixed proof also carries `extent_write_ns`. A fixed
proof with zero extent writes carries `status_poll_ns` and `status_exit: 0`.
The harness owns the evidence for those values and the replay log/subset join.
Changing the random seed or labeling an image random does not prove a
non-prefix selection; campaign completeness remains a separate packet fact.

The synthetic boundary controls use tiny literal images, complete full hashes
and raw written/differing evidence. Source mutants demonstrate sensitivity to
missing comparisons, quiet-chunk skipping, prefix-only survival assumptions,
lost flush obligations, missing coverage, ignored producer failures/bindings,
and fixed-revision extent writes. Existing frozen packets retain their old pin;
adoption requires the complete packet oracle and a new seal.


For reviewed legacy-corpus parity ONLY, the case must declare
`observation_purpose: legacy_parity`; pattern-tag observations may then use
`encoding: pattern-tags/v1` on both views,
`tag_blocks: [{block, tag, data?}]`, and `nonzero_blocks` in each chunk record.
The same identity, range, coverage and producer-status obligations apply.
This adapter supports 4096-byte blocks: `zero` reconstructs zeros; a tag of at
most 50 UTF-8 bytes reconstructs a 64-byte `WALFS<tag>` line padded with dots
through byte 63, followed by newline, repeated 64 times. Every block is rebuilt
and judged against the declared workload bytes by the existing content judge;
equal tag strings across views are never equality evidence on their own.
An `other:` tag requires all 4096 raw bytes and a matching 16-byte hex prefix;
without raw bytes the image is UNKNOWN. A reconstructed third value fails the
same OLD-or-NEW/history rule, including corruption in quiet chunks.

Every emitted block is unique and within the declared geometry. Reconstructed
nonzero blocks must equal each chunk's `nonzero_blocks` count; missing chunk
records, short coverage or contradictory raw bytes are UNKNOWN. Only raw bytes
of differing chunks are retained, plus the complete workload block set.
Optional `scan_sha256` is a 16-hex legacy prefix or 64-hex full independent hash.
A mismatch against reconstructed bytes makes the `TAG_SCAN` row FAIL with
`HARNESS-HASH-MISMATCH`; it does not relabel the separate image/campaign facts.
The adapter computes full hashes itself even with legacy prefix observations.
New qualification runs never use this adapter: they require the full-SHA/raw
producer interface above. Missing or other purpose declarations yield UNKNOWN.
Producer provenance and captured-artifact bindings remain pack obligations;
this interface does not authenticate a free-standing tag scan.
