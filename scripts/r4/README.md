# R4 cloud pack entry

Use `cloud-pack.py ASSET_ROOT` as the controller's pinned `pack_exec` entry.
It passes the approved cloud inventory and per-node host keys to the committed
`scenarios/v3block/r4-node-loss.yaml`, first RF3, then RF2. The scenario judges
the run; this entry only transports inputs and returns the first failing exit.

The private asset root contains:

- `pins.json`: full `product` and `runner` commit ids.
- `product/`, `runner/`: checkouts at those commits.
- `swblock`: the pinned runner binary.
- `manifest.json`: asset filename to SHA256 mappings, including `pins.json`
  and `swblock`; pin this manifest's digest in the controller declaration.

The entry refuses a manifest missing either required file before reading source
pins, running git, opening SSH or executing the runner binary. Its own executable
digest is pinned separately by the cloud controller's `pack_exec.sha256`; system
git/SSH and the Go toolchain are supplied by the controller environment.

The controller supplies `SEAWEED_CLOUD_PACK_CONTEXT` with the inventory,
SSH client identity, individual server-key pins, output directory and deadlines.
All three hosts must have the same valid inventory SSH user. The entry never
chooses a username itself. The three SSH readiness calls and both scenario
rounds consume that inventory user and the exact per-node pins. An absent pin,
invalid username or mixed usernames refuses before SSH.
Validation uses explicit errors that remain active with optimized Python.
The entry also explicitly refuses optimization (`-O` or `PYTHONOPTIMIZE`) before
reading any input, so its admitted interpreter mode stays fixed.

Put the pinned Go toolchain required by the product on the controller's PATH.
The existing scenario builds product binaries from its archived product commit;
the entry does not compile or modify an alternative workload. The cloud
controller owns deadlines and destroy/absence verification, including on failure.

No-network regression: `python3 -m unittest discover -s scripts/tests -p test_r4_cloud_pack.py`.

## Before any cloud create

Run the local startup preflight on the controller (Linux, Python with PyYAML,
Go on PATH, OpenSSL, tar and an existing IPv4 interface). It builds the pinned
RC binaries from `git archive`, extracts the actual master and blockvolume argv
from the pinned scenario, and starts both RF3 and RF2. Each master and each
volume command is observed for 10 seconds. The master stays up while volume
commands run sequentially, respecting the production one-volume-per-host lock.
Every process must remain alive and accept a TCP connection at its rendered
master or volume-status listener. A readiness log alone is insufficient; the
master's optional ready-line flag is not added to the scenario's argv.
This catches startup refusals; it is not a replication
or data-readiness qualification. No systemd, root, NVMe initiator or cloud API is
used by this check. Processes are reaped by recorded PID in every outcome.

Only paths, endpoints and the executable locations are substituted for local
temporary resources; the flag set and its non-address values come from the
scenario. TLS remains enabled. In particular, authenticated master startup
includes `--audit-log`, documented in the product operator guide, line 65.
The external-NVMe opt-in is preserved, so its temporary listener uses the
controller's existing non-loopback IPv4 interface and an ephemeral port; other
listeners use loopback. No guest writes or cloud mutations are performed.

Use this entry for an authorized R4 launch, instead of invoking cloud run directly:

```sh
python3 scripts/r4/start-preflight.py /private/assets --launch-token "$LAUNCH_TOKEN"
```

Without `--launch-token` it performs only the local smoke. With the token it
invokes `ASSET_ROOT/seaweed-cloud run` only after both rounds pass, using the
same asset root's pinned config and pack entry. The cloud controller retains
its own token, deadline, budget and empty-RG checks.
Include this script in the private asset manifest. Keep its temporary directory
private: it contains certificate keys. Publish only `receipt.json` and process
logs, never the whole directory. The receipt records binary hashes and rendered
argv; log paths in a refusal identify which command failed. If the scenario or
RC changes, rerun this preflight before create.

Controls: `python3 -m unittest discover -s scripts/tests -p test_r4_start_preflight.py`.
