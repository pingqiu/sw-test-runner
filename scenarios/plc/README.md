# plc-powerloss-counted: the power-loss run as a runner scenario

`scenarios/plc-powerloss-counted.yaml` runs a power-loss run through the runner. The exec pack only executes
on the VM. The controller seals, verifies, produces and judges, so the verdict never depends on the VM the run destroys.

## Tools (each decision lives here; the YAML only calls them)

| file | what it decides |
|---|---|
| `plc_seal.py` | seals a reviewed candidate for one run (it never approves a seal); details below |
| `plc_seal_check.sh` | before any cloud call: the approved seal, the packet files, the cloud config, the producer, these tools and the product head. A rehearsal only records `sealed=yes\|no`. |
| `plc_settle.sh` | the teardown: `seaweed-cloud verify`, plus an independent listing in the subscription and resource group that the pinned config's variables name, with the group required to exist. A verify output that lacks `remaining_resource_ids` or `create_unresolved` as lists is refused. It runs as an ordinary phase (the gate needs it) and again as an always phase (a run that timed out or was cancelled is still verified). |
| `plc_judge.py` | the sealed packet's `powerloss.judge_run`. Exit 0 only for PASS; it writes no bytecode into the packet. |
| `plc_gate.sh` | `gate_pass` from the collected evidence: 1 only for `counted` with `sealed=yes` and PASS; 0 for a passing rehearsal or dry run |
| `plc-tools.sha256` | the hashes of the four executed tools; the sealed packet pins this manifest's sha256 |

## Sealing a run (`plc_seal.py`)

The candidate must match its reviewed input list exactly. That means the list's sha256 equals the review's pin, every listed file verifies, no file is missing, extra (`__pycache__` included) or a symlink, and no text file holds a CR. The sealed packet is the listed files minus `SHA256SUMS` and `UNSEALED`, plus `drill/plc-run-pins.json` and a new `SHA256SUMS` over everything else. `drill/plc-run-pins.json` records:
- `config_sha256`, the run's prepared `seaweed-cloud` config;
- `producer_sha256`, its `SHA256SUMS` hash, with every file verified;
- `tools_sha256`, the hash of `plc-tools.sha256`;
- `product_head`;
- `candidate_input_pin`.

The tool prints the seal (the sha256 of the new `SHA256SUMS`) and the approved-seals line. The approver records it; the tool never does.

## Classes

- `rehearsal`: no cloud. It judges a retained capture pinned by sha256, with any packet. Recorded with `gate_pass=0`.
- `dry`: the cloud lifecycle with an approved sealed packet and a launch token. Recorded with `gate_pass=0`.
- `counted`: as `dry`, plus the product commit must be on `main` and equal to the sealed pin. It is the only class that can publish `gate_pass=1`.

The bundle can replay the judgment: `plc-evidence` (with every tool hash), `plc-capture` (the raw `out.tgz`) and `plc-lout` (the produced cases) are all collected before cleanup.

## Tests

- Packet-free, run in this repository's CI: `test_plc_gate`, `test_plc_settle` (stub `seaweed-cloud` and `az`), `test_plc_seal`, `test_plc_seal_check`, `test_plc_judge_stub`, `test_plc_tools_manifest`.
- With a packet: `PLC_PACKET=<packet> python3 -m unittest test_plc_judge_parity`. On the packet's own fixtures, the judge as the scenario runs it gives `judge_run`'s verdict and exit code. The cases are the pass, six refusals, a product FAIL, one missing case on either filesystem, truncated case data, an invalid non-prefix container, and an unreadable packet.

## Not yet shown

Every retained capture is a three-case DRY run, while the judge requires the four sealed cases, including `pl-nonprefix`.
So the first real counted run is also the first end-to-end run of produce and judge on real evidence.
