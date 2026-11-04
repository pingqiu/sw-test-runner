# Cluster process chaos on direct

Base checked: runner main a3f86210. Select one of
`volume-restart`, `filer-restart`, `volume-freeze`, `C1-single-kill`,
`C2-rolling`, `C4-meta-restart`, or `C5-two-kill` with
`cluster_chaos_cases: ["volume-restart"]` and `replication: "001"` in the
normal `seaweed-cloud direct` config. Other phases are exclusive.
C3/C6 are NOT_RUN on direct and retain their cloud path.

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
master lookup. C1/C2/C4/C5 create new KV/S3 VIDs during the mixed load: after recovery
it reads the actual ACK journals and repeats that join for every FID using
current role generations, including the replacement. Unknown/null FIDs and
single/foreign copies refuse overall qualification even if byte readback
passed. The mixed cases do not run an unrelated seed workload. ACKs must exercise all
six owners. Configuration 001 alone cannot satisfy copies.json. Existing
worker payloads and schedules remain unchanged. Shared C5 attribution is
tightened for cloud and direct callers: every FID must have known killed-pair
placement before the extended unavailable-key window can apply.

Stage 1 keeps the three clients' 120 s HTTP load, prospective client-local P,
per-failed-op start >= P and first recovered exact completion <= P+60,
byte-exact named recovery GETs and the generation witnesses. Freeze also
keeps the all-thread stopped/live chain, 20 s hold, victim-key overlap and
post-resume GET; missing overlap is NOT_COVERED. C1 keeps the existing 210 s
mixed KV/S3 load, 60 s kill hold, per-operation attribution and final exact
readback of every ACK. C2 keeps 360 s load with sequential restarts of all
six volume roles; C4 keeps 150 s load with master then filer restart; C5
keeps 210 s load with simultaneous vol3/vol4 kills and a 60 s hold.
C5 extends outage credit only for keys whose EVERY FID has two known
pre-fault copies on that killed pair; any unknown or other copy receives
only the ordinary per-operation window. All ACKs must still read back exact
and pass the final current-owner/master-lookup join. Unknown completion and
wrong bytes fail.

Run one case per command with a fresh run ID and output directory. The CLI
uses the ordinary direct arguments; no GO packets or approved-run entry.
Stage result JSON is component evidence: overall CLI PASS additionally
requires all role logs collected, all run units stopped, and every declared
service port absent. Collection failure remains FAIL and still tears down.
Raw per-step stdout/stderr, copies, markers and generation receipts stay in
--out; failed mixed-load journals are captured before teardown. No new Python,
network namespace, host network fault or platform authority machinery.

C5 process-result.json separates process_recovery from unavailable_keys.
The latter persists eligible_keys and affected_reads; eligibility or exact
reads alone do not establish unavailable-key coverage. COVERED requires a
failed read of an eligible key inside the assessed unavailable-key window.
Otherwise its status is NOT_COVERED, including zero eligible keys, even when
ordinary process recovery and the overall command pass. No live result is
retroactively changed by this output correction.
