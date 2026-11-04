# EC stage 1a inputs and capture

The first case uses one Go master, filer and S3 service plus fourteen volume
processes on one target host. It seeds settled objects, encodes their manifest
volumes, rejects a planted wrong byte, reads both routes, then kills one through
four shard holders cumulatively. This is process-loss evidence on one host.
Without the product reconstruction marker, S2 remains `NOT_COVERED` and exposure
`NOT_OBSERVED`, even when all required bytes and deadlines pass.

## Repeatable local stage 1

Legacy exact-byte GO packets/tokens and protected lab releases are historical
requirements only. Stage 1 uses the current local path; any future lab stage
uses the manager's window plus direct execution/log collection/postflight.
The normal entry is the shared direct CLI, against a local Linux container
running ssh and systemd. Such a container is explicitly an `onprem` host;
the old fourteen-host `docker` validator is unchanged.

```sh
go build -o seaweed-cloud ./cmd/seaweed-cloud
./seaweed-cloud direct --inventory INV.json --config EC.json --out NEW_DIR --timeout 8m
```

EC.json uses `ec_stage1:true`, `ssh_key`, `host_key`, and `files` containing only
`weed:{path,sha256}`. The requested HTTP inventory has one host with all five
roles, its real non-loopback data address, and run_root
`/home/<ssh-user>/sw-runs/<run-id>` (`/root/sw-runs/...` for root).
The SSH user needs passwordless sudo, python3, systemd and ss on this disposable
host. No system-wide product installation is made. Seventeen run-named units
run as that user with umask 0077. The first volume HTTP/gRPC ports anchor two
fourteen-port blocks; S3 gRPC is S3 HTTP+10000. All 34 ports must be distinct
and available. Each required step and JSON terminal is checked. The runner
joins the frozen holder identity to the unit MainPID before SIGKILL.

`ec-stage1.json` records S1 PASS only after both routes pass; S2 remains
NOT_COVERED even after all mandatory reads succeed. Raw terminals and commands
are in cloud-step JSON files; cleanup independently collects journals before
stopping units and checking all 34 ports. A failed collection or postflight
fails the CLI. Default direct runs keep their existing KV sequence.

The approved-run action, its local S0b test wrapper and scenario are retired.
Historical wrapper evidence remains attributed to its original heads; it is not
a supported execution command. Use the direct entry above.

`ec_setup.py` runs `lock`, `ec.encode`, and `unlock` in one bounded shell
session. Capture still proves every manifest VID has no original `.dat`,
records unrelated volume files, and rejects unsupported `.active` generations.
Shared payload generation, byte assessment, frozen-capture validation and all
Python worker leaves remain in use by direct. Expected rows are 64 ACKs,
640 ordinary exact GETs / zero wrong bytes, one planted-byte rejection and
four cumulative holder losses. The planted control adds 64 real original
GETs, for 704 total. S2 remains NOT_COVERED without the product marker.

## Under-quorum stage 2a (Go, local first)

Add `ec_under_quorum:true` alongside `ec_stage1:true`; use `--timeout 25m`,
work 25 minutes / hard 27 minutes / cleanup reserve 60 seconds (existing inventory maximum). This cell
requires a single first-encode uniform 10+4 volume with a 5-byte-offset index.
The frozen `.vif` and `.ecx` bind each manifest needle's complete data span.
Both missing-data0 and intact-data cohorts must be nonempty, else NOT_COVERED.
Kill captured owners of shards 10,11,12,13, then 0, checking each generation.
The first four losses retain stage 1's 640 exact GETs. With nine survivors,
every missing-data0 needle must return 5xx or a bounded read/transport error;
all intact-data needles must remain byte exact on both S3 and volume HTTP.
A target 200 or a control error fails. Any unplanted wrong-byte 200 is a P1:
stop reading immediately, retain raw evidence and classify before changing code.
Each GET has a 30-second bound; each final degraded route has a 600-second
step bound. `s2a:PASS` describes this fail-closed cell, while reconstruction
exposure remains NOT_OBSERVED and S2 remains NOT_COVERED without the marker.
`ec-stage1.json` is a component result; cleanup/postflight must also pass for
CLI success. Product builds go to the designated build host.
