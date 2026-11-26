# One TiB known-zero acceptance

`scenarios/public/known-zero-1tib-acceptance.yaml` runs the product's
`TestKnownZeroAcceptance` on two operator-supplied hosts. The runner uploads,
launches, waits, judges, cleans and collects; there is no second SSH controller.
The host helper has only one-host verbs. It does not grant itself a lab window,
release a lease or modify product state directly.

## Admission and inputs

Obtain the shared lab window/lease and record START through the existing lab
tools before invoking the runner. Use a new run id `kz-YYYYMMDDtHHMMSSz` and
fresh absolute roots named `swb-<run-id>` under pre-existing owned NVMe parents.
After the runner, require PASS plus both cleanup receipts and collected evidence
before END/release. Failed or unknown cleanup retains the lease/quarantine.
No lab address, host key, credential or local service name belongs in this file.

Build all product binaries from the exact coordinator-selected main RC, not a
branch that can move. The archive contains exactly these seven regular files:

- `kz.test`: `go test -c -tags subprocess,chunkstore_integration ./cmd/blockvolume`
- `blockmaster`, `blockvolume`, `sw-block`: normal builds of those product commands
- `blockvolume-kzhold`: `go build -tags kzhold ./cmd/blockvolume`, test-only crash hold
- `weed`: the independently pinned stock filer binary
- `BUILD.json`: `{"source_sha":"<full RC SHA>","files":{"kz.test":"<sha256>",...}}`

The manifest files map names exactly the six binaries. Record the toolchain,
source cleanliness and weed pin with the packet. Create an uncompressed tar of
those names, with no directory prefix, and calculate its SHA256. No credentials,
symlinks, source tree or helper scripts go in the archive. The host refuses a
foreign source pin, file census, archive hash, path escape or symlink member.

Validate from this runner checkout:

```sh
go run ./cmd/swblock validate scenarios/public/known-zero-1tib-acceptance.yaml
python3 -B -m unittest discover -s scripts/kz -p 'test_*.py'
go test . -run TestKnownZeroScenarioAlwaysSettlesBothHosts -count=10
```

Run from the same directory with `swblock run`, all `-env key=value` flags
**before** the scenario path. Supply every empty env entry from the YAML:
kz_run, rc_sha, archive_path, archive_sha256, source_host, target_host, ssh_user,
ssh_key, source_host_key, target_host_key, source_root, target_root,
source_ci_unit, target_ci_unit, min_free_bytes. Host addresses are distinct IPs;
SSH keys are operator paths and host keys are approved public pins. The exact
CI service names must match `actions.runner.*.service`; explicit `none` is
allowed only where no Runner.Listener exists. The packet sets a positive
physical free-space floor in bytes; logical sparse capacity is not free space.

For example, the command shape is:

```text
swblock run -results-dir <bundle-root> -env kz_run=<new-id> ...all other env flags... scenarios/public/known-zero-1tib-acceptance.yaml
```

The helper and judge paths default to files in this checkout. The helper is
uploaded; the judge stays on the controller. `controller_python` defaults to
`python3` (override to `python` on a Windows controller when needed); it must
name a Python executable on PATH. The local Bash command runner must also be
available on the controller. Use the flat runner syntax `{{ name }}` throughout;
`kz_run` is separate from the bundle-reserved `run_id`. Runner uploads are
bounded and checked. Both hosts need Python 3, systemd, passwordless sudo,
nvme-cli, ext4/XFS tools, filefrag, findmnt, lsblk and losetup. The product test
resolves its own exact exported NQN before mkfs; no physical backing disk is
formatted. All test data and mounts stay below the declared roots.

## States, commands and interruption cells

| Durable state / observation | Next command | Failure or interruption / rerun |
| --- | --- | --- |
| Root absent | Runner creates exclusive root | Existing root refuses reuse; no service changed |
| Inputs staged, no state.json | prepare validates pins/NVMe/capacity/idle | Failure has changed no CI/unit; cleanup reports unprepared |
| state.json durable, prior CI state recorded | schedule fallback, stop only declared CI unit | Cleanup can restore only prior-active CI, including a lost stop reply |
| Prepared | start records exclusive started marker, starts bounded unit | A lost launch reply cannot trigger a second launch; cleanup observes unit |
| Source running | handoff is a run-bound JSON receipt | RUNNING, EOF, SSH error or early process exit is never handoff |
| Target running | poll exit.json then judge report | No receipt 30 s after unit ends is rc125, not an hours-long idle wait |
| Target exits 0 | signal source, wait source, collect both report archives and judge on controller | Failure stops ordinary phases; always-cleanup still runs on both hosts |
| Any prepared state | cleanup under flock | Verify unit description bound to root+RC before stop; a foreign unit refuses |
| Unit stopped | unmount run mounts, disconnect exact run NQNs, prove absence | Failed proof leaves scratch and CI stopped; rerun same cleanup |
| Absence proved | delete only root/tmp, restore prior-active CI, receipt | Rerun is idempotent; unrelated data/services are untouched |
| Cleanup settled | collect each out/ into runner bundle | Incomplete collection fails the runner; retain remote evidence |

The state record is one atomic local JSON record, synced before CI changes.
Only prepare writes the prior CI state; start writes the at-most-once marker;
only cleanup restores it. The run unit's description is the root+RC ownership
check. Timers have a separately checked description before cancellation.
The source sequence is handoff <=60 min, target <=100 min, then source-finish
<=55 min (step 5 runs AFTER the target and can scan 1 TiB after restart).
The three waits total 215 min; signalling adds at most 3 min. Source test/unit
ceilings are 230/235 min; target test/unit ceilings are 95/100 min. The source's
KZ_TARGET_WAIT is 105 min, covering target startup, wait and completion signal.
Runner work is 250 min (including staging, collection and controller judgment),
with a fresh shared 20 min always budget. A 275 min host timer invokes the same
cleanup verb if the controller disappears, five minutes past the runner total.
Its cleanup service has a five-minute ceiling: proposed window 280 min.
These are configured deadlines, not proof of completion. The product test is unchanged.
No source/target process is resumed by a timer.

If either automatic cleanup fails, the named run operator in the launch packet
reruns the same cleanup verb on that host after diagnosis; it restores CI only
after absence is proved. The coordinator assigns a replacement operator if
necessary. Do not manually restart CI or release the lab lease while dirty or
unknown state remains. The packet names the people owning this obligation.

Environment assumptions: no host reboot, privilege/cgroup escape, external
replacement of a run directory/unit, or physical disk remapping during the
window. A reboot invalidates automatic-cleanup claims and requires quarantine
and operator inspection. The test's intentional crash kills a volume process,
not a host. The operator does not reuse run ids or clear retained failure state.
Every test child stays in its role unit; KillMode=control-group provides the
process boundary. Polling and timeouts decide failure, never product authority.

## Verdict and evidence

The product test owns byte/counter assertions. The runner judge independently
requires exact run/role, integer one TiB, real device mode, rc0, and exactly five
source plus eight target PASS rows (no duplicate, missing, failed or extra row).
A smaller rehearsal or user-space mode cannot count. Both test exit receipts
must match the run/role. Successful cleanup is not a product PASS.

The last ordinary phase judges both collected tgz files on the controller,
without extracting their contents to disk. Always phases then clean both hosts
and collect separate closeout archives even on failure; a cleanup/collection
failure keeps the overall runner result FAIL, regardless of report judgment.

The bundle contains both role reports, complete logs, launch/source/binary pins,
exit and cleanup receipts. Source rows cover empty S0 and mkfs/S1 for ext4+XFS,
plus the fresh-start crash; target rows cover before-hydration and mounted
readback for each restore/fork. `out/`, state, helpers and pinned binaries remain
on the hosts as evidence; the cleanup removes temporary data, mounts, NQNs and
role processes. Remove retained artifacts only under a separate explicit
host+absolute-path cleanup instruction after bundle verification.

Off-host tests inject failure in the real scenario engine and assert both hosts'
cleanup and collection always run. Python controls reject report mutants,
foreign units, unsafe archive entries, false handoffs and duplicate launches;
prepare/cleanup command interruption controls retry cleanup. Their systemctl,
NVMe and sync calls are mocked: they are not a real-device or root qualification.
A Linux off-lab run and a packet-approved lab run remain required before this
scenario can supply a release acceptance result.
