# Frozen KV load accounting (migration D1b)

This pure package ports the frozen v3A comparison windows from `grill.py` at
`b0e5cb99`: `[300,600)` and `[600,840)` seconds from the bound body epoch.
The aggregate/native ACK and byte thresholds remain 10 times the retained
baseline; successful native reads require 1/s/client and HTTP reads 0.5/s.
There is no API to replace the windows, baselines or multiplier after a run.

Each supplied timestamp counts in its own half-open window: offers, admissions,
native submissions, native completions and ACKs. A write admitted before a
boundary and ACKed after it contributes to different stage windows. ACKs are
never backcharged. HTTP writer 3 cannot supply the native writer 0..2 floor.
Submission means the native worker's callback after it accepted a future; it is
not proof that a DMA work request was posted. The fixture projects that callback's
monotonic timestamp with the same worker's recorded wall/monotonic anchor.

Mean SET and phase latencies are grouped by ACK time. `Samples=0` means no known
sample, not a proved zero wait. The phases are deliberately named:

- `QueueAndPreparation`: admission to native submission, including IPC/buffer work.
- `NativeWait`: native submission to completion, including loader/network/store.
- `ReceiptAndChecks`: native completion to parent receipt, including checks,
  metadata and IPC/polling.
- `Journal`: parent receipt to the ACK record.

These intervals do not isolate store-only time or actual buffer occupancy.
Those measurements need the later worker/collector port. Missing stage fields
remain NOT_PROVEN; they are not filled with zero. Telemetry COMPLETE means the
supplied writes have all stage timestamps; it is not proof that the input journal
is exhaustive. Overall coverage is NOT_COVERED for a measured frozen gate failure,
NOT_PROVEN if the frozen floors pass but phase fields are missing, and PASS only
for this package's complete measured gates. It is never a whole-run verdict.

The caller must bind run/host/clock domains, join exact operation identity,
validate receipt completeness, apply the byte oracle and evaluate admission,
fault/path/resource/cleanup gates. This package neither schedules writes nor
proves one offer authorizes one SET. It does not add a scheduler, broker, decoder,
new native seam or live action. The frozen ramp and boundary-drain proof are
later pieces, not inferred from these counts.

The checked gzip fixture is a declared field projection of retained failed
v3A `195756Z`: 10,517 offers, 181 writes and 16,692 GET completion records.
Its source archive, selection rules and byte digest are in `testdata/provenance.json`.
The independently saved output of the byte-identical legacy grader is compared
for window bounds, ACK/native rates, byte rates, successful per-client reads and
NOT_COVERED. Retained offers and admission counts also match the post-run diagnosis:
5,860/4,657 offered versus 84/97 admitted and ACKed. The evidence remains a failed
run; replay is not a new load or native-path qualification.

`go test ./packs/kv/...` also distinguishes high offers with low ACKs,
HTTP-only substitution, starved reads, boundary-crossing ACKs, missing timings,
duplicate identities, malformed clocks and byte overflow. No Python counterpart
is changed or retired. The successful oracle package is a separate PR.
