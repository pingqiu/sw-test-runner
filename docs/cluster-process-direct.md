# Cluster process chaos on direct

Base checked: runner main 790df348. This first slice selects one of
`volume-restart`, `filer-restart`, `volume-freeze`, or `C1-single-kill` with
`cluster_chaos_cases: ["volume-restart"]` and `replication: "001"` in the
normal `seaweed-cloud direct` config. Other phases are exclusive. C2/C4/C5
follow separately; C3/C6 are NOT_RUN on direct and retain their cloud path.

Use a requested onprem HTTP inventory with one existing systemd-capable
container host first, or two real hosts after an operator lab window. Keep
physical hosts and their SSH/data addresses truthful. Each physical host
that holds volumes declares volume_http and volume_grpc; the first also
provides the metadata listener bases. Six volume process ports are the
base plus 0..5, validated for range/collision. The physical inventory is
saved unchanged inside process-placement.json alongside the role/host map.
Each role has a distinct run directory and transient-unit generation.
Co-location is process tolerance, never host tolerance.

The baseline reuses the cluster topology/writable-owner probe. Before any
fault, each volume role is joined to its unit MainPID/starttime, deployed
executable, working directory, exact data/endpoint argv, both owned TCP
listeners, and status disk directory. The unit generation is rechecked after
the status read. Unknown or foreign non-victim owners refuse before dispatch.

Stage 1 joins all seed ACK VIDs to two distinct observed role owners and the
master lookup. C1 creates new KV/S3 VIDs during the mixed load: after recovery
it reads the actual ACK journals and repeats that join for every FID using
current role generations, including the replacement. Unknown/null FIDs and
single/foreign copies refuse overall qualification even if byte readback
passed. C1 no longer runs an unrelated seed workload. ACKs must exercise all
six owners. Configuration 001 alone cannot satisfy copies.json. Existing
worker payloads and assessors remain unchanged.

Stage 1 keeps the three clients' 120 s HTTP load, prospective client-local P,
per-failed-op start >= P and first recovered exact completion <= P+60,
byte-exact named recovery GETs and the generation witnesses. Freeze also
keeps the all-thread stopped/live chain, 20 s hold, victim-key overlap and
post-resume GET; missing overlap is NOT_COVERED. C1 keeps the existing 210 s
mixed KV/S3 load, 60 s kill hold, per-operation attribution and final exact
readback of every ACK. Unknown completion and wrong bytes fail.

Run one case per command with a fresh run ID and output directory. The CLI
uses the ordinary direct arguments; no GO packets or approved-run entry.
Stage result JSON is component evidence: overall CLI PASS additionally
requires all role logs collected, all run units stopped, and every declared
service port absent. Collection failure remains FAIL and still tears down.
Raw per-step stdout/stderr, copies, markers and generation receipts stay in
--out; failed mixed-load journals are captured before teardown. No new Python,
network namespace, host network fault or platform authority machinery.
