# Shared Seaweed pack over S0b

See [M01/M02 close-out](M01-M02-CLOSEOUT.md) for the scoped r8 smoke result and
seven CIFS/SSH/readiness lessons, pinned to the tested runner and product tuple.

This Draft now integrates the accepted S0b runtime at `1d88dca1`. It remains
local/fake-qualified only: no M01/M02, container, lab, cloud or provider launch
is authorized by this PR. Protected installation, service bootstrap, independent
GO export, original live lease and actual per-host readback are prerequisites,
not fabricated by this pack. See `docs/s0b-authority.md`.

```
worker claim -> RunApprovedRequest -> exact GO/lease/deployment admission
  -> seaweed_preflight -> seaweed_deploy (+ readiness)
  -> seaweed_workload -> seaweed_collect
  -> RunScope.Close -> seaweed_cleanup (always) -> retained result
```

The CLI's strict `approved-run` entry registers this pack, without the legacy
raw-shell/build/provider registry. Each job is resolved by its independently
approved label and exact executable/SHA/argv. The same RunScope owns foreground
and background processes, including POSSIBLE enrollment before a lost reply.
Every dispatch/authorization repeats S0b live authority and deployment readback.
No caller variable, YAML boolean, permissive custom scope, inventory digest or
plan result is an authorization token.

The runtime profiles below use one filer and one volume with a bounded
ACK/exact-read smoke; transport fallback always refuses. This is
not the Gate 0a Docker adapter, cloud provisioning, multi-server routing, a soak,
or privileged disk setup. It does not change VFs, PFC/ECN, mount/format disks,
install protected files or replace their reviewed bootstrap process.

The optional `m01-m02-v1` profile now supplies the missing preflight, service
argv, readiness and collector programs. It is data selection, never consent.
`seaweed-pack profile INPUT.json` emits canonical job pins and a five-phase
scenario using joined inventory; `execution_authorized` remains false. Runtime
checks the same derived argv and compiled helper hashes against independently
scanned/protected deployment bytes. See `docs/m01-m02-profile.md` for the actual
operator prerequisites and evidence limits.

## Inventory seam and independent bindings

`seaweed.inventory/v1` now requires typed work/hard deadlines, cleanup reserve
and explicit per-host service names/ports. It preserves them through Join.
`Plan` carries exact requested/resolved JSON bytes. `MarshalPlan` is the common
producer seam; `inventory.Resolve` binds an observed data document to the exact
request; `inventory.Bind` is the consumer check. At runtime the pack binds to:

- S0b's original run/workload/transport, artifact manifest digest, deadlines and
  cleanup reserve; no restarted timeout or inferred service port;
- the exact admitted host/SSH/user/key/root set and read-back boot generations;
- no formatting permission in this slice, and S0b's exact shared per-host output
  root, pre-created privately by the independent bootstrap;
- complete preflight/readiness/collector host coverage, one approved owned
  deployment job per declared service role and one workload job per client;
- the complete five-phase scenario, cleanup `always`, no skipping, parallelism,
  repeats, retries, ignored errors or caller node/target redirection.

The plan is in `scenario.metadata.seaweed_plan`, therefore inside the exact
independently approved scenario digest. Runtime actions accept no parameters;
Engine Vars cannot rewrite the plan. A pack refusal also revokes/records failure
in the original ApprovedRun, so `ignore_error` cannot turn it into a PASS.

`preflight`, service startup, readiness and collector labels refer to programs
whose exact bytes/argv must be independently reviewed and installed in S0b's
protected deployment policy. The pack does not infer the semantics of an
arbitrary program from a label, nor install/start an unapproved program. The
M01/M02 readiness packet's missing protected profile/GO/receipts are still real
runtime prerequisites; the synthetic example is not their replacement. These
programs must check actual device/GID/MTU/fabric, owned listeners, directories
and generation evidence. Approval of this code does not qualify those facts.

## KV operation adapter and proof

Two explicit runtime profiles share this same five-phase pack and S0b custody:
on-prem RC/DC requires an independently approved `rdma` binding and retains
required RDMA/no HTTP fallback; Azure HTTP requires both inventory and approved
binding to say `http`, with RDMA `not_applicable`. No profile is inferred from
a missing device or failed RDMA operation. Other target/transport pairs refuse
in this slice, including cloud RDMA and on-prem HTTP. This enables no cloud
provisioning or launch. HTTP uses the same bounded ACK/latest-byte oracle and
owned deployment/readiness/collector jobs, not another cloud deploy path.

`worker/kv_smoke.py` is a thin external-stack CLI around unchanged accepted
`soak_native_worker.work`, `payload`, `validate_ack_metadata`, and `Oracle`.
The worker copy is SHA256
`05c077d05a6af5ec4decebf18c3e0be1b4df1f7dc17265ff1b2c709cf94e577e`
from `codex02-chaos-input-head-a-20261028/input`; its old `child()` is not used.
All copied helper bytes are preserved across Windows/Linux by `.gitattributes`.

`SmokeArgs` derives filer endpoints, declared RDMA data/control/DC listeners,
client device/port/GID, transport and run/client namespace from joined inventory.
The approved job argv must match exactly, including the canonical JSON config.
The pack requires the compiled-in smoke/helper hashes in both scanned script
inputs and the client's protected read-back set, plus its native extension.
Missing clients, empty workloads or missing smoke server roles refuse before
any job starts. The helpers execute by compiling the exact sibling `.py`
sources; same-name packages/bytecode cannot override them and competing
protected helper candidates are refused.
The script uses isolated Python and pinned helper/native directories. It clears
inherited RDMA selectors and constructs the explicitly selected `loader-rdma`
or `loader-http`, both with no fallback claim. HTTP argv carries no RDMA
address/control/device/port/GID selectors; contradictory fields refuse.

Bounds are 1..16 keys of 1 byte..4 MiB, one operation at a time. Each SET is
ACKed only after successful completion and fresh metadata/FIDs; its GET must
match the reserved latest ACK SHA256/length. A miss is a failure in this smoke;
there is no rotation exception or mutation retry. The accepted worker retains
unknown DMA buffers through process teardown. Native close and all jobs remain
bounded by the original S0b work/hard envelope and owned settlement.

RC/DC success additionally requires exact direct RC/DC byte/object deltas, zero
other transport/copied deltas and no read-error delta. Correct bytes via HTTP
still fail that required-RDMA profile. Explicit HTTP success requires the loader
to report HTTP, exact copied byte/object deltas, zero direct RC/DC deltas and no
read errors. HTTP is labelled HTTP, never RDMA. Loader write-byte counts are
emitted, but are **not** called a server-side
write-transport witness: `server_write_join=NOT_OBSERVED` until an isolated
volume counter/log join is collected. No RC/DC hardware claim comes from CPU
fakes. The pinned loader-rdma product path is required mode, not fallback mode.

ACK/EXACT/summary records are JSON lines in the owned job stdout. Foreground
pack jobs request bounded stdout/stderr export in their settled custody receipt
(128 KiB per stream, exact base64 bytes and SHA256). The custodian reads only its
already-owned descriptors after child settlement. Missing, unstable or oversized
output fails evidence qualification; it never changes proven settlement into a
live issuer. Legacy jobs do not request capture. There is no additional SSH/SCP
reader. Authority bundles retain these receipts and the exact process/spec pins.

Action deadlines fence enrollment and authorization, including a delayed
enrollment reply. The custodian also receives the absolute authorization bound;
this is separate from the lifetime of an already authorized service. The
original run hard/cleanup envelope is unchanged. Background services require
their actual terminal receipt through settlement. A custodian-observed live
leader killed by explicit cleanup is distinguished from a service that exited
earlier (even exit 0); unexpected or unproven exits fail. Older custodians that
do not supply that distinction cannot qualify background cleanup. Protected
runner/helper deployment hashes therefore need updating before any runtime use.
Proven terminal failures remain sticky after retirement and repeated Stop,
Wait, ClearFault or Close. They fail the result but do not count as a possible
live issuer: existing approved obligations are restored after settlement is
proven. Unproven/UNKNOWN custody still withholds restoration under the original
authority/deadline checks; an empty live-job map alone grants no restoration.

RC direct-byte counters qualify transport, **not the declared local device or
GID**. Actual RC-device qualification remains NOT_PROVEN until the independently
reviewed preflight/profile supplies route/device evidence. This correction does
not add a device-selection product API or a hardware claim.

## Planning, JSON and recovery

```sh
go run ./cmd/seaweed-pack --json plan seaweed_deploy examples/seaweed-onprem-plan.json
# Local only: S0B_REQUIRED, execution_authorized=false, effects=[];
# planned_steps, original deadlines and owned-cleanup procedure are included.
sw-test-runner approved-run --json --request /service/running/REQUEST.env --policy /service/bootstrap.json
```

The plan example has TEST-NET IPs, fabricated boot/fabric observations, fake
artifact digests and future deadlines. It is data only, not a live lab profile.
The plan CLI has no run/apply/upload/SSH command. The native adapter also exposes
`--config JSON --dry-run`, which imports no native module and has no remote
operation. JSON results carry stable codes and remedy hints. `approved-run
--json` emits `seaweed.approved-result/v1`; phase/plan results use
`seaweed.pack-result/v1`. `execution_authorized:true` reports a completed phase
inside an admitted scope; it is never reusable authority for another action.

Cleanup settles only issuers enrolled by this exact scope. It cannot signal a
foreign process, launch new work after close, delete arbitrary roots/cloud
resources or manufacture absence. An UNKNOWN or cleanup error remains FAIL
through repeated Close and finalization, even if work itself succeeded. A
failed workload remains failed even when cleanup can prove settlement. Keep the
original S0b evidence/receipts; resolve UNKNOWN through the approved independent
procedure, not an unowned kill or retry.

## CPU verification

Run the portable inventory/CLI/pack tests on Windows or Linux. Strict ownership
admission tests are Linux-only; cross-compiling them and executing locally in
WSL is sufficient. `runtime_linux_test.go` uses actual strict record readers,
GO/lease admission, RunApprovedRequest/Engine and phase registration, replacing
only transport jobs/readback with compiled CPU fakes. It never contacts hosts.
Python controls use the accepted work/oracle completion path with an in-process
connector boundary; import/discovery performs no native or remote work.

```sh
go test ./inventory ./cmd/seaweed-inventory ./packs/seaweed ./cmd/seaweed-pack ./cli
python3 packs/seaweed/worker/test_kv_smoke.py
```


The fixed M01/M02 profile explicitly pins the filer Unix socket to
`<owned-root>/filer.sock` and refuses a pre-existing object there. Its runtime
hardware qualifier compares PCI VF/PF identity and parsed firmware with the
sealed profile facts, as well as boot/GID/MTU/rate; historical DC evidence is
never a substitute for the new required-DC smoke. The protected worker-owned
lease option and its test-only remote-tail limit are in
[the S0b authority contract](../../docs/s0b-authority.md).

## Cloud-init admission and cancelled peers

The HTTP cloud pack waits for cloud-init with JSON output before creating its
run directories or uploading artifacts. It retains the complete response in
`cloud-init-status.json` and the original exit code and decision in
`cloud-init-step.json`. Exit 0 or 2 is accepted only with status `done`, no fatal
errors (including stage errors), and no unapproved recoverable messages.
The allowed warnings are the complete Azure IMDS 410 polling message for
`169.254.169.254/metadata/instance?api-version=2021-08-01&extended=true`,
and the exact captured attempt-1 `404 Client Error: Not Found` polling message
for `169.254.169.254/metadata/reprovisiondata?api-version=2019-06-01`.
Unknown, mixed, malformed or ambiguous error data refuses preparation.
This follows [cloud-init's structured status and exit-code contract](https://github.com/canonical/cloud-init/blob/26.2/cloudinit/cmd/status.py).

A peer cancelled before deployment may have no run services or worker logs.
The collector records those as `MISSING`, using a fresh SSH connection under
the existing shared collection deadline. A failed service-list query, journal
read, applicable file read, or evidence write still fails collection. The
original preparation failure remains the run result; cleanup still runs.
The client2-shaped JSON control is reconstructed from the observed warning;
the failed run did not save its original status JSON.

SSH command cancellation aborts that Node's connection and joins
`session.Run`/`Wait` before reading stdout/stderr buffers. Callers must reconnect
before reusing a cancelled SSH Node. The cloud collector already uses a fresh
connection. Linux CPU race control (encrypted loopback only):

```sh
go test -race ./packs/seaweed -run 'Test(CloudCancelledPeerActiveSSHStreams|CancelledPeerWithoutServicesStillCollects)$' -count=50
```
