# plc-powerloss-counted: the power-loss run as a runner scenario

`scenarios/plc-powerloss-counted.yaml` runs a power-loss run through the runner. The exec pack only executes
on the VM. The controller seals, verifies, produces and judges, so the verdict never depends on the VM the run destroys.

## Tools (each decision lives here; the YAML only calls them)

| file | what it decides |
|---|---|
| `plc_seal.py` | seals a reviewed candidate for one run (it never approves a seal); details below |
| `plc_seal_check.sh` | before any cloud call: the approved seal, the packet files, the cloud config, the producer (exactly its listed files, `plc_select.py` among them), the tools (the manifest names exactly the executed set) and the product head. A rehearsal only records `sealed=yes\|no`. |
| `plc_launch.sh` | runs `seaweed-cloud run` under a launcher that records its own pid and start time before the child starts and exits only after the child, forwarding TERM/INT/HUP. A step shell killed on timeout or cancel leaves both running, and they stay identifiable. |
| `plc_settle.sh` | the teardown, in order. (1) Terminal barrier: wait for the launcher to exit (bounded); still running means UNKNOWN with a recovery handle, and inventory is never observed while the child can still create or delete. (2) Admission: the config's output must hold `launch.json` with this invocation's launch token; output never created means not admitted, nothing touched; output without our receipt is UNKNOWN and untouched. (3) Verify: `seaweed-cloud verify` must return its real Result contract (`seaweed.cloud-smoke/v1`, code `EMPTY`, `remaining_resource_ids == []`, `remaining_observed` true, `create_unresolved` false), plus an independent listing in the subscription and resource group the pinned config's variables name, with the group required to exist. It runs as an ordinary phase (the gate needs it) and again as an always phase. |
| `plc_judge.py` | the sealed packet's `powerloss.judge_run`. Exit 0 only for PASS; it writes no bytecode into the packet. |
| `plc_gate.sh` | `gate_pass` from the collected evidence: 1 only for `counted` with `sealed=yes` and PASS; 0 for a passing rehearsal or dry run |
| `plc_owned.sh` | every always-phase step that writes or removes under the run directory first proves it is this invocation's (prepare's token in `.plc-owned`) |
| `plc_cleanup.sh` | removes the run directory only when each existing `evidence/`, `capture/` and `lout/` is collected and its archive lists exactly its files; otherwise keeps it |
| `plc-tools.sha256` | the hashes of the seven executed tools; the sealed packet pins this manifest's sha256 |

## Sealing a run (`plc_seal.py`)

The candidate must match its reviewed input list exactly. That means:
- the list's sha256 equals the review's pin;
- every listed file verifies;
- no file is missing or extra (`__pycache__` included), and none is a symlink;
- no text file holds a CR.

Every pinned or copied file is read once: the bytes hashed are the bytes parsed or written. After the copy, the staged files are hashed again against the reviewed rows and the candidate is rescanned. So a change during sealing refuses; it never yields a new self-consistent seal.

`--out` is claimed with an exclusive `mkdir`. It must not exist and must not overlap the candidate, producer or tools. On a refusal the tool removes only a directory it created itself. `SHA256SUMS` is written last.

The sealed packet is the listed files minus `SHA256SUMS` and `UNSEALED`, plus `drill/plc-run-pins.json` and a new `SHA256SUMS`. `drill/plc-run-pins.json` records:
- `config_sha256`, the run's prepared `seaweed-cloud` config;
- `producer_sha256`, the hash of its `SHA256SUMS`;
- `tools_sha256`, the hash of `plc-tools.sha256`;
- `product_head`;
- `candidate_input_pin`.

The tool prints the seal (the sha256 of the new `SHA256SUMS`) and the approved-seals line. The approver records it; the tool never does.

## Classes

- `rehearsal`: no cloud. It judges a retained capture pinned by sha256, with any packet. Recorded with `gate_pass=0`.
- `dry`: the cloud lifecycle with an approved sealed packet and a launch token. Recorded with `gate_pass=0`.
- `counted`: as `dry`, plus the product commit must be on `main` and equal to the sealed pin. It is the only class that can publish `gate_pass=1`.

The bundle can replay the judgment: `plc-evidence` (with every tool hash and both settle passes), `plc-capture` (the raw `out.tgz`) and `plc-lout` (the produced cases) are collected before cleanup. Cleanup deletes nothing that was not collected.

## Tests

- **Packet-free, in CI.** `.github/workflows/ci.yml` runs a guarded discovery of this directory: zero tests refuses. The modules are `test_plc_gate`, `test_plc_settle`, `test_plc_seal`, `test_plc_seal_check`, `test_plc_cleanup`, `test_plc_judge_stub` and `test_plc_tools_manifest`.
  - `test_plc_settle` uses stub `seaweed-cloud` and `az`, with verify fixtures in `fixtures/` that were encoded by `provision/smoke`'s own verify path. It includes a cancel stand-in: the step shell is killed while the child stays active.
  - `test_plc_tools_manifest` keeps the executed set equal across the manifest, both tools' `REQUIRED_TOOLS` and the YAML's calls.
- **With a packet.** Run `PLC_PACKET=<packet> python3 -m unittest parity_plc_judge`; it is not named `test*`, so CI discovery leaves it out. On the packet's own fixtures, the judge as the scenario runs it gives `judge_run`'s verdict and exit code. The cases are the pass, six refusals, a product FAIL, one missing case on either filesystem, truncated case data, an invalid non-prefix container, and an unreadable packet.

## Not yet shown

Every retained capture is a three-case DRY run, while the judge requires the four sealed cases, including `pl-nonprefix`.
So the first real counted run is also the first end-to-end run of produce and judge on real evidence.
