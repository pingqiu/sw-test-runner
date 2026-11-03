# EC stage 1a inputs and capture

The first case uses one Go master, filer and S3 service plus fourteen volume
processes on one target host. It seeds settled objects, encodes their manifest
volumes, rejects a planted wrong byte, reads both routes, then kills one through
four shard holders cumulatively. This is process-loss evidence on one host.
Without the product reconstruction marker, S2 remains `NOT_COVERED` and exposure
`NOT_OBSERVED`, even when all required bytes and deadlines pass.

The approved job argv must give each holder `-max=3`: after allocating its source
volume, the encoder requires two free volume slots. Holders must advertise the
target's real non-loopback address. The peer-copy guard permits internal peers
but rejects loopback; keep that guard enabled. A wildcard listener can retain
the existing localhost readiness probes while accepting peer copies to the
advertised address. On M02 use M02's inventory-bound real address, never a
workstation replica's CGNAT address. These are packet configuration, not new
runner knobs or changes to product policy.

`ec_setup.py` sends `lock`, `ec.encode` and `unlock` through one bounded shell
session. Its shell audit can create a separate ordinary volume. Capture proves
that every manifest VID has no original `.dat` (including suffixed variants).
Unknown data filenames and unsupported `.active` generations still refuse.
Unrelated ordinary volumes are not target originals: capture records their
VID, holder, filename, byte count and stable hash in `unrelated_volumes`.
The runner retains that evidence and refuses a row whose VID is in the target
manifest. It never uses unrelated volume evidence to credit reconstruction.

A release packet requires a complete local run on the pinned real product:
seed, encode, planted control, S1, all four S2 loss episodes and proven cleanup.
Readiness alone does not qualify a packet. Local success does not authorize a
lab deployment; the reviewed pins and exact-byte launch token remain required.
