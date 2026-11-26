# plc-powerloss-counted: the power-loss run as a runner scenario

`scenarios/plc-powerloss-counted.yaml` runs a power-loss run through the runner. The exec pack only
executes on the VM. The platform seals, verifies, produces and judges on the controller, so the verdict never
depends on the VM the run destroys.

| file | what it is |
|---|---|
| `plc_judge.py` | the controller judge: loads the sealed packet's `powerloss.py` by path and calls its `judge_run` over `lout/<fs>/plc/<case>` with the packet's sealed filesystem policy and the run's `out/info.txt`. It writes one `judge.json`. Exit 0 only for PASS, 1 FAIL, 2 UNKNOWN or any error. It never re-implements a verdict. |
| `test_plc_judge_parity.py` | parity: on the packet's own per-filesystem fixtures, `plc_judge.py` run as the scenario runs it gives the verdict and exit code of `judge_run` called directly: pass, missing metadata, wrong FSLIST, metadata off its manifest, missing and extra filesystem trees, a proven product FAIL, and an unreadable packet (UNKNOWN, never PASS). |

## Classes

- `rehearsal`: no cloud. It judges a retained capture pinned by sha256, with any packet (sealed or not), and is never counted.
- `dry`: the cloud lifecycle with an approved sealed packet and a launch token. It is recorded and not counted.
- `counted`: as `dry`, plus the product commit must be on `main`. The four sealed cases must all pass on every sealed filesystem.

## Inputs the sealed packet pins

The packet's `SHA256SUMS` hash must be an `s <sha>` line in `approved-seals`, every file must verify, and there must be no
`UNSEALED` marker. `drill/plc-run-pins.json` pins `config_sha256`, the prepared `seaweed-cloud` run config, which pins the
module, variables, pack and output path. `drill/powerloss-fs.json` is the filesystem policy.

## Run

```sh
PLC_PACKET=/path/to/packet python3 -m unittest test_plc_judge_parity     # offline parity
swblock validate scenarios/plc-powerloss-counted.yaml
swblock run -env class=counted -env packet_dir=... -env approved_seals=... -env run_name=<RUN> \
  -env product_repo=... -env product_head=<40-hex> -env launch_token=<hub id> \
  -env cloud_bin=.../seaweed-cloud -env cloud_config=.../config.json -env cloud_resource_group=<rg> \
  -env plc_tools=$PWD/scenarios/plc -env producer_dir=... scenarios/plc-powerloss-counted.yaml
```

## Not yet shown

Every retained capture is a three-case DRY run. The judge requires the four sealed cases, including
`pl-nonprefix`, so no retained capture can pass, and parity is shown on the packet's fixtures instead. The first real
counted run is the first four-case capture, and so the first end-to-end run of the produce and judge phases on real
evidence.
