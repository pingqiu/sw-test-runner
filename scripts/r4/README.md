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

The controller supplies `SEAWEED_CLOUD_PACK_CONTEXT` with the inventory,
SSH client identity, individual server-key pins, output directory and deadlines.
All three hosts must have the same valid inventory SSH user. The entry never
chooses a username itself. The three SSH readiness calls and both scenario
rounds consume that inventory user and the exact per-node pins. An absent pin,
invalid username or mixed usernames refuses before SSH.
Python optimization (`-O` or `PYTHONOPTIMIZE`) is explicitly refused before
reading any input because it would disable the adapter's assertion checks.

Put the pinned Go toolchain required by the product on the controller's PATH.
The existing scenario builds product binaries from its archived product commit;
the entry does not compile or modify an alternative workload. The cloud
controller owns deadlines and destroy/absence verification, including on failure.

No-network regression: `python3 -m unittest discover -s scripts/tests -p test_r4_cloud_pack.py`.
