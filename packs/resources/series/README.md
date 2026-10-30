# Frozen resource series (migration D4b)

`Assess` is a pure port of the five resource checks in `assess_soak.py`, with
the existing `gomemory` package for r5/r6r3. It owns no processes, files, clocks,
profiles or runner actions. It introduces no S0a dependency or Python cutover.

```text
typed periodic/final receipts + fault/generation receipts + Go profile bytes
  -> grouped process generations
  -> recorded RSS/FD and replacement-window rules
  -> exact terminal pin/lock and fixed resource bounds
  -> PASS / FAIL / NOT_OBSERVED
```

The scope is LC1-A/E and LC5-A/E resource observations, not a new transport
state machine or production-readiness claim. It reuses the existing soak gates;
no Mooncake/3FS/NIXL mechanism or new framework is needed for this port.

## Recorded policies, without regrading history

Two historical source policies must be preserved to meet the retained replay
contract. `54d2d185-startup` is used only for that recorded legacy assessment;
`9b5d0dca-replacement` is the accepted warm-window successor. There is no default,
automatic detection, or retry with another policy after a failure. New dispatch
must pin the current policy in trusted code; the later collector must bind the
policy to the reviewed source/receipt manifest rather than accept a workload
request's policy choice. Preserving both source policies was chosen over applying
the new baseline retroactively, which would change the sealed historical verdict.

The warm successor validates every replacement's complete capture chronology
before startup/fault exclusion; same host/boot/time namespace, boot-relative
clock and tick rate; three captures wholly inside age 600..690 s; FD+16 from
the third selected capture; and six later distinct captures spanning >=300 s
by conservative same-host capture endpoints. It requires one matching terminal
generation (or the existing GONE receipt). Missing replacement identity or
terminal evidence cannot fall back to the original generation or empty PASS.
Original-process and Go behavior is unchanged. RSS retains its startup baseline,
max(64 MiB,10%) tail growth and <=2 MiB/min slope. Pin and lock values must return
exactly to the selected baseline.

The other checks preserve the fixed cache/all-data/directory/auxiliary bounds,
reported pin budgets, zero retained sources, and exact cold-pool/pin baseline
return. Missing/invalid numeric evidence refuses PASS; malformed typed framing
returns FAIL. Metric diagnostics are meaningful only on rows that actually
contain an observed assessment; no default zero is evidence of measurement.

## Retained parity and boundary

The fixtures contain all 240 periodic rows per run, all final process records,
the consumed fault fields and actual Go profile bytes. Projection drops only
unused receipt fields, such as process-limit text and workload counters. Every
source member, archive, assessor and helper is hashed in the provenance files.
The frozen Python replay first reproduces its original five resource checks;
Go matches states, grouped measurements, baseline sample choices and disk maxima
(least-squares slopes compared to 1e-9 MiB/min for floating-point summation).
`132324Z` remains resource PASS; `095251Z` remains resource FAIL under its
recorded policy. This is resource parity, not a replay of unrelated functional,
fault exposure, terminal-counter or cleanup gates.

The caller still strictly decodes and bounds raw receipts; binds the source,
host/run/artifact inventory; proves observation ownership and fault enrollment;
and retains original evidence. This package validates assertions supplied at
that boundary, not the truthfulness of the collector. Historical prior-generation
closure retains its existing receipt contract. No hardware or lab run is enabled.
`assess_soak.py` and its helpers remain in service until reviewed parity, the
retirement map and Ping's explicit cutover permit replacement.
