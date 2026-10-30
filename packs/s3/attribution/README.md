# S3 A1 attribution (pure migration slice)

This package ports amendment A1's application-error rule from #887's frozen
`s3_collect.py` and role table in `s3_workload.py`, source
`2dbe555245f9f9aa53e1a3af1893e9fa24f33218`. It has no S0a, worker, network,
filesystem, retry, native binding or runner registration.

```text
failed attempt + runtime excusal + same recorded fault
  -> role/kind/class allowed
  -> sent before window closes, completes after fault begins
  -> completes within own timeout + 5 seconds
  -> read or designated fresh mutation -> attributed error
```

Ordinary attempts have 35 seconds; designated attempts have 185 seconds. A1
deliberately permits a failure after the readiness window has closed, when the
request was sent before close and its own timeout still permits completion.
It does not extend the send window or excuse semantic failures. UNKNOWN mutation
outcomes must still be quarantined by the separate oracle; attribution never
turns an error into an ACK or an ordinary mutation into a designated attempt.

`Eligible` checks the independent rule. `Assess` additionally requires the runtime
mark to identify that same fault and its exact bounds. Client-kill credit is
restricted to the victim's key/PID, as required by the table's victim-only rule.
The two checked-refusal forms are HTTP GET/HEAD 404 and the designated HTTP 416
test. A mutation or DC failure cannot be relabelled as such a refusal. Late
classifications supersede only the same immutable attempt; they cannot rewrite
key, timing or outcome. These joins make explicit checks that the Python
collector previously trusted. Canonical retained rows keep the same result;
contradictory or forged receipts intentionally refuse PASS.

Missing journals are NOT_PROVEN, malformed framing FAILs, and unknown role
labels are not silently renamed. An observed empty journal only says no error
was found; workload minimums, dispatch inventory and coverage are separate gates.
The report's first 50 unexcused rows are bounded diagnostics, not a full journal.

## Retained scope

The full `194018Z` journal has 12,400 rows / 12,391 operations after its nine late
classifications: 111 failed responses, 101 checked refusals and ten excused errors.
Go matches the frozen Python A1 result exactly. The older `174221Z` fixture
projects only its two master-episode PUTs (four rows including classifications).
Its legacy `gateway` label is not accepted as the current `s3` role; no whole-run
parity is claimed for that older journal. Archive, member, source and fixture
digests are retained. The legacy product binary was 3565db99-derived, per the
packet's provenance correction; this is algorithm replay, not current-main or
native-path qualification. The original replay's fake-DC path FAIL is unchanged.

The caller must independently prove fault windows/enrollment, bind operation and
runtime-mark receipts to the dispatch, derive error classes and checked refusals
from actual responses, and select the frozen table. This package validates typed
assertions; it cannot prove collector truth, a native transfer, bytes, cleanup
or fresh-key designation by itself. Input is read-only and outputs no ACKs.

Full S3 byte-oracle replay is a separate blocked slice: the supplied receipt
archives contain no pre-dispatch seed/version-digest history, and the frozen
workload seeds include `time.time_ns()`. GET digests are observations, not
independent expectations. No byte-oracle or Python-retirement claim is made here.
