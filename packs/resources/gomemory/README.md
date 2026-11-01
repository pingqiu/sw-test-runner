# Frozen Go-memory gates (migration D4a)

This pure package ports the adopted `go_memory_gate.py` and
`go_memory_r6b.py`: r5 for master, r6r3's same-generation warm RSS for filer,
canonical pprof `inuse_space` bytes, and prior-generation closure. It does not
register runner actions, collect live processes/profiles, or use S0a.

```text
enrolled identity + archived capture metadata/bytes -> Group
  closed and independently absent -> CLOSED (not live-memory PASS)
  live -> bound warm/end profiles -> Evaluate -> PASS / FAIL + measurements
```

The caller supplies independently collected listener ownership, process identity,
quiescence, GC count and closure receipts. This package checks their required
values, generation/phase/filename/hash joins and timing; it cannot prove the
collector told the truth. The caller must also bind the run, host, artifact and
full evidence inventory. There is no filesystem/archive extraction here.
Missing numeric evidence cannot become zero/PASS. The typed Go inputs intentionally
do not reproduce Python's dynamic dictionary interface or exception messages.

Unchanged thresholds: RSS growth <= max(64 MiB, 10%), heap growth strictly
<32 MiB, FDs <= baseline+16, one terminal GC after >=60 s quiescence, warm
profile and filer RSS entirely inside age 600..635 s. Original-process RSS/FD
series, replacement FD/pin windows, terminal pin/lock, storage and quiescence
gauges remain separate migration pieces. No native terminal-counter claim is made.

The small pprof decoder accepts the frozen canonical Go-profile form. Duplicate
singular ValueType fields remain outside that reference format. Inflated profile
bytes are bounded to 64 MiB and the accumulated heap total to signed 64-bit;
these are evidence framing limits, not resource-threshold tuning. No new dependency
or toolchain/module change is introduced.

## Retained parity

All five Go process groups from each of `132324Z` and `095251Z` are included:
six CLOSED generations and four live PASS generations in total. Frozen Python
re-evaluation matched each original Go-memory result, including exact heap/RSS
measurements; Go matches those states and numbers. Fixtures include the actual
gzip pprof bytes, source member hashes and original archive/assessment digests.

`132324Z`'s full verdict remains PASS. `095251Z`'s full verdict remains FAIL:
its Go-memory rows already passed; the failure arose in replacement FD/pin
assessment. This PR neither regrades that run nor claims whole-resource parity.
That distinction must survive the later aggregate port. No Python retirement or
lab cutover is authorized by this package.
