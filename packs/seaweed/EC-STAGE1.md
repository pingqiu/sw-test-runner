# EC stage 1a inputs and capture

The first case uses one Go master, filer and S3 service plus fourteen volume
processes on one target host. It seeds settled objects, encodes their manifest
volumes, rejects a planted wrong byte, reads both routes, then kills one through
four shard holders cumulatively. This is process-loss evidence on one host.
Without the product reconstruction marker, S2 remains `NOT_COVERED` and exposure
`NOT_OBSERVED`, even when all required bytes and deadlines pass.

## Repeatable local stage 1

On Linux/WSL, from the runner checkout, as root:

```sh
bash scripts/run-ec-stage1.sh /absolute/path/to/weed13230 /absolute/path/to/NEW-evidence
```

Requires Go as declared in go.mod, Python 3 with `read1`, `getcap` (libcap2-bin),
flock and tar. Supply the real Go weed built from
`13230ea1509d9a8f80dead4680ab1fb334aadb19`, SHA256
`e15a1951b6e7d2b159bbeb5740f8bce5370d1d9ee3eaaaf4d9f2e38e1c2926f4`.
No binary is downloaded. The command builds this checkout's runner and uses
the existing opt-in Linux integration fixture for encrypted loopback SSH,
protected readbacks, strict worker admission, Engine and process custody.
The worker and product run as UID/GID 1001 with umask 0077. Root is needed only
to model the protected install and drop the child UID. Ordinary CI skips this
real-product run unless `EC_LOCAL_WEED` is supplied by the command.

All services run on this one machine. No RDMA, second host, lab token or cloud
is involved. `m02` is the pack's logical node name, mapped to the disposable
loopback SSH endpoint. Synthetic local authority and the local lease never
use the shared lab ledger. The command refuses occupied profile ports and an
existing evidence directory. Its fixed service ports are 23555/33555,
23888/33888, 23833/33833, 24100..24113 and 34100..34113.

The approved job argv gives each holder `-max=3`: after allocating its source
volume, the encoder requires two free volume slots. Holders must advertise the
target's real non-loopback address. The peer-copy guard permits internal peers
but rejects loopback; keep that guard enabled. A wildcard listener can retain
the existing localhost readiness probes while accepting peer copies to the
advertised address. The command uses the first `hostname -I` address, or
`EC_LOCAL_ADVERTISE=<local-private-IPv4>`; it verifies that address is local
before starting jobs. This is needed because weed 13230's peer-copy guard
rejects loopback even for this single-machine case. It does not change that
guard, routes or interfaces. HTTP probes and object operations use loopback.

`ec_setup.py` sends `lock`, `ec.encode` and `unlock` through one bounded shell
session. Its shell audit can create a separate ordinary volume. Capture proves
that every manifest VID has no original `.dat` (including suffixed variants).
Unknown data filenames and unsupported `.active` generations still refuse.
Unrelated ordinary volumes are not target originals: capture records their
VID, holder, filename, byte count and stable hash in `unrelated_volumes`.
The runner retains that evidence and refuses a row whose VID is in the target
manifest. It never uses unrelated volume evidence to credit reconstruction.

The command runs seed, encode, planted control, S1 on S3 and direct-volume
routes, then all four cumulative S2 holder losses on both routes, followed by
worker cleanup. Expected rows: 64 seed ACKs; WRONG_BYTE_REJECTED; S1 PASS;
640 ordinary exact GETs with zero wrong bytes and each operation <=30 s;
four exact held-generation exits; mandatory_bytes_deadlines PASS;
S2 NOT_COVERED / exposure NOT_OBSERVED; worker PASS and RESOURCES_SETTLED.
The planted control flips one client-side byte only after an exact original
200 response; it does not claim product corruption. Its 64 original reads
bring the total real object GETs to 704.

`RESULT.json`, target journals/logs, worker results, authority records, local
ledger and source/binary hashes stay in the requested evidence directory.
All owned processes must settle naturally; forced disposal makes the test
fail and is recorded. Only newly created, test-owned `/opt` scratch roots are
removed. The local result IS the stage 1 result; no stage 1 lab packet is
needed. S2 reconstruction coverage awaits its product marker; stage 2 also
starts locally on its separately pinned new product main.
