# M01/M02 profile (implementation in progress)

As-built starting point: d5ba4535b5b05f05da6baf1560aed8a75365289b, accepted inventory 0664d799, accepted S0b 1d88dca1. Assigned by Claude01 6ae4d29b9c9fd1ba9ca54501. No installation, native operation or runtime launch is authorized here.

Responsibility: LC2-C actual host/boot/device inventory binding; LC3-E exact smoke/path evidence; LC5-A/E owned settlement and export; LC6-B observations across boot/restart. Reuse the accepted pack, custody, worker and KV oracle. No new execution driver or scheduler. The lease/worker canonical-flock handoff remains a separate blocker.

Candidates: reuse the legacy server_start lifecycle (rejected: it would start an alternate owned-process system); add a new deployment driver (rejected); use a small profile CLI invoked only as existing approved pack jobs (selected). Product argv/environment and accepted smoke/oracle are reused. The profile programs have bounded observe/preflight/ready/collect and same-PID exec of hash-qualified protected product bytes; they do not fork a background service or acquire authority.

Evidence export candidates: out-of-band SSH/SCP (rejected as a second evidence reader); a reverse network receiver (rejected as new infrastructure); return bounded stdout/stderr snapshots in the existing settled custodian receipt (selected). The latter needs a small existing-wire addition, not another command channel. Snapshot failure must not erase proven process settlement, and missing/truncated output cannot qualify the profile. Legacy jobs do not request capture.

Confirmed test seams reused from the assignment and accepted substrate: profile CLI/public evaluator with real-shaped filesystem/HTTP fixtures (refuses wrong boot/device/GID/ports, distinguishes observation from permission); existing S0a Start/Wait through the actual local custodian (exports exact bounded owned bytes only after settlement); strict RunApprovedRequest/Engine through compiled transport fakes (complete profile binding and evidence, no alternate executor); compiled plan/inventory consumers (no fabricated observations or widened scope). These CPU controls do not establish hardware/DC compatibility. After controls, actual observations may be collected read-only under a short existing lease; host mutation still needs exact-command GO.

Initial scope is two hosts, one master/filer/volume on M02, one DC+required-RDMA KV smoke on M01, no S3 service, no faults, no VF/PFC/ECN changes and no disk format/mount. Controller credential is the approved pathname /home/testdev/.ssh/id_ed25519, never its bytes. Explicit service ports, deadlines, exact artifact/profile/GO pins and independently observed boot/network/fabric facts remain required before the real scenario is runnable.

The selected programs are `lab_profile.py` plus the byte-preserved accepted
`vf_preflight.py`. The former uses the latter's actual VF/IP/GID/MTU/PCI/cap
observations. It does not derive DC capability from mlx5 names. The read-only
capture therefore says `dc_capability=NOT_PROVEN`; any resolved capability must
also cite the separate retained successful DC witness with matching host boot,
device, selected GID and firmware. The smoke itself must still demonstrate the
required direct-DC counters; past evidence is not credit for a new smoke.

Service launch uses the existing S0b custodian's process, then same-PID fd-exec
of the protected, hash-qualified product. No helper forks or owns a replacement
process lifecycle. The environment is explicit and excludes inherited RDMA
selectors; cache/tmp/data files stay under the pre-created private run root.
The master explicitly uses `-peers=none`. No forced growth/rotation is included.
`rdma_push` declares the existing RC push port separately, without port arithmetic.

Readiness checks exact PID/start/boot/product hash, owned TCP listener inodes,
the fresh owned-volume DC-writer marker, typed topology registration, leader and
filer HTTP responses. The accepted preflight producer's absolute-deadline HTTP
reader pattern is reused: no redirects/proxies, literal-IP peer binding, bounded
headers/body and repeated remaining-time checks. Its readiness window is at most
45 seconds and the original work deadline. Collection returns generation,
listener, directory/filesystem and DC-log-marker/hash facts; this small smoke
does not apply soak resource gates or claim a server-side write-byte join.

Observed storage on 2026-10-30: the approved `/opt/rdma-lab-ci/work` directory is
on each host's existing root filesystem. Inventory now permits `/` only as a
declared disk mount with `format_allowed=false`; a run root of `/` or root format
still refuses. The profile compares the exact by-id block device's device number
to both mount and private output root and requires 512 MiB free. It never formats,
mounts, removes, or changes permissions on that existing filesystem.

Named bounds: hardware snapshot must span <=45 s to qualify (accepted individual
probe bounds of 10 s command/13 s subprocess retained; the enclosing job is
bounded by its 90 s action and original work deadline),
profile readiness 45 s/original work, HTTP connect/read <=2 s/original work,
and all helper/job/cleanup lifetime bounded by the original S0b work/hard envelope.
Output capture is at most 128 KiB per stream, after positive settlement and before
the original hard deadline. Local/fake tests are not a hardware launch receipt.

Retained host survey: read-only lease `7a5f9ef7cdd44634a199620e15105e59`,
2026-10-30 18:08:21Z--18:09:08Z. Both boots matched the prior survey. The initial
M01 predicate mistakenly named `enp2s0v0`; it refused. The retained prior VF JSON
showed `enp1s0v0`, and the corrected capture passed. No hardware configuration,
services, workload directories or installed bytes were changed by the survey.

Runtime is still blocked until reviewed protected installation, independent GO,
strict UID claim proof and the canonical lease-to-worker flock handoff exist.
The current lease holder and worker both acquire the same exclusive flock; a
separate running holder cannot be silently kept while starting that worker.
Neither this profile nor a YAML field bypasses that unresolved handoff.
