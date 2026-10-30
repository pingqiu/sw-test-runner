# S0b-min PR2: protected deployment readback

`Node.VerifyDeployment(ctx, DeploymentPolicy)` returns a `DeploymentReceipt`
and an error. Only **nil error** qualifies the receipt. The caller must retain
the receipt/transcript even on refusal. PR3 must independently approve/hash the
policy, bind the resulting receipt to admission, and repeat the check before
fresh custodian authorization. This PR alone activates no worker or RunScope.

The policy binds node ID, exact SSH endpoint/user/public key, installation root,
unprivileged workload UID, complete file paths/sizes/SHA-256/roles, required
native/interpreter sets and optional expected boot ID. On first qualification,
record the observed boot ID; rechecks must bind that ID. The receipt's
PolicySHA256 hashes Go's JSON encoding of this typed policy; the exact input
file digest remains a separate PR3 authority binding.

Each call snapshots the typed policy (including file/role slices), then opens a fresh pinned SSH connection. It does not reuse Node.client or
execute an unverified runner, adapter, product or interpreter to attest itself.
Fixed base-OS commands read boot/UID, enumerate the entire installation, read
ownership/modes/inodes/sizes/mtime/ctime before and after hashing, and enumerate
file capabilities. Exact approved files and parent directories must match:
extra/missing entries, symlinks/special files, changed metadata, wrong bytes,
wrong boot/UID, rootless ownership, group/world write, setuid/setgid or file
capabilities refuse. External files are permitted only for explicitly pinned
interpreters; their ancestors are checked too.

Initial provider prerequisites:

- Linux, root-owned protected installation and ancestors, job UID != 0.
  Workload processes must remain unprivileged, as required by S0a. Preinstall
  this protected tree using the separately approved privileged mechanism;
  this method neither uploads nor changes permissions/ownership.
- A trusted base OS/image supplies `/bin/sh`, `/bin/cat`, `/usr/bin/id`,
  `/usr/bin/find`, `/usr/bin/stat`, `/usr/bin/sha256sum`, `/usr/bin/timeout`,
  `/usr/sbin/getcap` (GNU coreutils/findutils and libcap2-bin on Ubuntu).
  This image/bootstrap trust includes a controlled SSH exec/startup environment
  and is a prerequisite, not established by the files' own reported hashes. A malicious host/root is outside this attestation.
- Readback is clipped to the caller context and a 15 s controller bound; the
  remote read-only command has a 12 s timeout plus 1 s kill grace. The SSH pipe
  closes on cancellation. Failed/timed-out readback never authorizes work and
  does not claim an owned-job exit or remote process absence witness.
- At most 256 files, 1024 paths/ancestors, 512 MiB per expected file and 1 MiB
  response. No partial or oversized transcript can qualify.

CPU controls run real encrypted loopback SSH in local Linux/WSL. Test-owned
files under an exact `/opt/codex03-readback-test-*` root are root-owned; read
commands run as UID 1000. They cover fresh successful readbacks, same-size
replacements, exact native set, boot/identity, modes/ownership, symlinks,
capabilities, and a bounded cancellation using a test-owned FIFO. No lab host,
device, cloud or service action is performed. These controls do not replace
the separately authorized M01/M02 and Azure provider gates.
