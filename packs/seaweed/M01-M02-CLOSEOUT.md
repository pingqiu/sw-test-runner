# M01/M02 smoke close-out: seven environment lessons

Checked against runner `b0e368866c2db36737a594bef8afae3a3c777fad`,
product tuple `af8c562681`, and run `s0b-m01m02-20261031-r8-cm-ready`.
These observations concern the M01/M02 mount, SSH negotiation and readiness;
they are not assumptions about every filesystem or deployment.

| # | Observed failure | Retained rule and control |
|---|---|---|
| 1 | The CIFS mount forces UID/GID and modes (`nounix`); a share-backed request cannot prove Unix ownership. | Keep strict request ownership checks. Put queue, state and results on M01 local disk under `/var/lib/seaweed-testops/rdma-s0b`; submit as `testdev`. The zero-job probe verified claim ownership `1001:1001`, mode `0600`, and missing-GO refusal. The canonical shared lab lock stays unchanged. |
| 2 | An independent nonblocking flock conflict returns `EACCES` on CIFS, rather than local `EAGAIN`. | Accept either errno only from the flock probe after the independent open succeeds. An open failure or a granted second lock still refuses. Errno controls distinguish these cases. |
| 3 | Re-flocking inherited FD9 can also return `EACCES`, even when that descriptor already holds the lock. | Never re-flock FD9. Match its `fstat` identity to the canonical path, require matching full-range `FLOCK WRITE` evidence in `/proc/self/fdinfo/9`, and require the independent probe to conflict. Ignore the exited helper's PID. Missing lock evidence or wrong inode refuses. |
| 4 | A just-written, fsynced and closed ledger record can acquire a later mtime/ctime during its first read. | Settle only the worker's own published records, then retain strict before/after capture and exact-byte checks. Do not remove timestamp comparisons or replay publication. A timestamp-change seam, continuing drift and content-change controls distinguish the outcomes. |
| 5 | Two equal stats 100 ms apart can precede a deferred close at about +1 s (`closetimeo=1`, `actimeo=1`). | Require an unchanged sampled metadata window of at least 2.5 s, polling every 250 ms, within 6 s and the original hard deadline. A +1 s change resets the window; continuous change refuses. This bounds polling, not stalled syscalls, and does not claim to detect same-byte inode replacement. |
| 6 | A fixed Ed25519 callback rejects the server's ECDSA key when default Go negotiation selects it first. OpenSSH dry runs had forced Ed25519. | Negotiate the pinned key's algorithm on both Node and independent OpenCommand connections. RSA pins use SHA-512/SHA-256 signatures. Keep exact-key verification. Multi-key server positives and wrong-key refusal controls cover both paths. |
| 7 | The volume's RDMA CM port is not a TCP socket, so requiring it in `/proc/PID/net/tcp` makes readiness impossible. | Require ownership of the four actual TCP ports; require the configured CM bind endpoint and DC-writer marker in the exact generation's bounded logs. Missing TCP ports or bind markers refuse. Logs prove startup only; the required-RDMA/no-fallback smoke supplies end-to-end evidence. |

The corresponding code is in `approved_records.go`, `worker_lease_lock_linux.go`,
`worker_lease.go`, `infra/node.go` and this pack's `worker/lab_profile.py`.
Earlier failed runs remain failed and their sealed packets are unchanged.

R8 passed four acknowledged 1 MiB writes and four independently regenerated,
hash-exact GETs: direct DC reads **4**, RC/copied reads **0**, fallback **false**,
read errors **0**. All ten jobs settled; the lease released. Independent
postflight found no owned process or active private listener, an inactive worker,
empty queue/running set and a free canonical flock. Evidence roots remain.

Terminal packet:
`V:/share/rdma/codex03-cm-ready-r8-terminal-pass-20261031/`, manifest
`f9ccc331adc4b39fc8ae6391bf51ba128966487ee928fb0dbe32a9ad65c458f2`.
Codex01 independently accepted this scoped smoke in `6ae563a3` after rehashing
126/126 files and regenerating payloads. Server committed-write equality was
**NOT_OBSERVED**; resource/soak gates were **NOT_ASSESSED**. This does not qualify
current main or authorize another run. PR #21 stays Draft; further host action
waits for the next approved tuple and run.
