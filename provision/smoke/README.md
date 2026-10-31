# Azure one-VM smoke controller

One Linux command: local dry-run -> Terraform init/plan/apply -> shared pack direct HTTP phases -> finally destroy -> independent resource-group list. No S0b admission, GO, materializer or authorization receipts on this cloud path. M01/M02 strict entry is unchanged.

Only run with the manager launch token, current price/cap check, an empty dedicated RG and one run at a time. This Draft has local/loopback evidence only; no account calls or launch qualification.

## Private controller configuration

Terraform 1.17.1, Azure CLI and credentials stay on the Linux controller. Use the existing `provision/terraform/azure-validation` tfvars: explicit subscription/RG, westus2 D2s_v5, pinned image, login **public** key and controller /32. Work/hard UTC deadlines include cloud cleanup reserve; hard end must be minute-aligned and <=60 minutes away; cap <=$3. All paths below are absolute.

```json
{
  "source": "/private/repo",
  "module_sha256": "<pins result>",
  "variables": "/private/run.tfvars.json",
  "variables_sha256": "<pins result>",
  "output": "/private/new-run",
  "estimated_usd": 0.12,
  "cloud": {
    "ssh_key": "/private/client-key",
    "files": {
      "weed": {"path": "/private/artifacts/weed", "sha256": "<approved hash>"},
      "weed-volume": {"path": "/private/artifacts/weed-volume", "sha256": "<approved hash>"},
      "native.tar": {"path": "/private/artifacts/native.tar", "sha256": "<approved hash>"}
    }
  }
}
```

The released af8c native archive contains top-level `seaweedkv_native/`. Extract it unchanged into run_root and pass run_root as native_dir; do not repack it or add a native/ prefix. The cloud HTTP smoke uses the OS filesystem under the inventory run_root, with no disk formatting. The declared cache disk stays unformatted. This is an infrastructure smoke, not cache-disk performance or rotation qualification.

The controller generates a fresh **server** ed25519 key before Terraform plan/apply, writes it in mode-0600 `host.auto.tfvars.json`, and uses its public half through infra.Node FixedHostKey for the first SSH. Cloud-init installs the private half only on that VM; it is also present in private Terraform plan/state. The client authentication private key never goes to the VM. Keep config/output/state/plan private, never V:/shared evidence. Do not print them. Terraform command output is withheld from command logs.

## Commands

```sh
go build -o /private/bin/seaweed-cloud ./cmd/seaweed-cloud
seaweed-cloud pins --source /private/repo --variables /private/run.tfvars.json --json
seaweed-cloud plan --config /private/config.json --json # local checks, effects=[]
# Manager token required:
seaweed-cloud run --config /private/config.json --json
# Recovery, including after controller/session death:
seaweed-cloud destroy --config /private/config.json --json
seaweed-cloud verify --config /private/config.json --json
```

Run requires a fresh private output directory. Init uses the committed provider version/lock, no repair. The locks retain every previous hash and add the Linux unpacked h1 obtained from the existing verified Linux zip (same azurerm 5.12.0). No credentials are transferred; no environment TF_VAR/TF_CLI_ARGS override is accepted.

The pack waits for cloud-init, uploads and checks the three artifact hashes, installs embedded #19 worker files, starts master/volume/filer/S3, waits for HTTP readiness, runs 16 x 1 MiB SET+immediate exact GET, and saves worker output and service logs. Every command has a deadline; nonzero/missing summary/wrong bytes fails. No workload retries or HTTP fallback. The VM is destroyed even if setup/workload fails. Normal teardown does not turn workload failure into PASS.

Expiry tags remain, but there is no native shutdown schedule or DevTestLab registration requirement. The run itself destroys resources and lists the full RG. If the controller/session dies, the next session must run the same destroy + verify before any new create; compute, disks and IPs can keep billing until cleanup. Hard-deadline teardown is best effort.

UNKNOWN apply stays sticky: destroy + an empty list cannot prove that an unresolved create will not finish later. `CREATE_UNRESOLVED` never becomes `COMPLETE` without resolving that pending create; remaining IDs and nonzero status must be reported to the owner. `verify` is locally bounded to 30 seconds. The RG/RBAC stay.

## Verification limits

CPU tests drive the actual native command boundary with fake Terraform/az, including a late asynchronous create after recovery's empty list. Loopback SSH tests use a real handshake to prove pinned-key success/mismatch refusal and exercise transfers/phase calls. Their server simulates service/worker output: they do not execute weed/cloud-init/the native connector or prove Azure service compatibility. Existing M01/M02 strict tests remain unchanged. Cloud qualification still needs the manager token.

Offline resource regression control (mock provider only, no Azure account):

```sh
terraform -chdir=provision/terraform/azure-validation test -filter=tests/shutdown.tftest.hcl -json -verbose > /private/mock-plan.jsonl
python3 provision/terraform/azure-validation/tests/check_no_devtestlab.py < /private/mock-plan.jsonl
```

This checks the actual one-VM mock plan for absence of DevTestLab resources and preservation of expiry tags. Controller destroy/verify/UNKNOWN code is unchanged.

## Failed cloud-step evidence

Every remote command records its step name, exact command, stdout, stderr, exit code and transport error in mode-0600 `cloud-step-NNN.json` in the private local output directory. Upload attempts record their operation and error too. A failing step's name and command also appear in the controller's JSON error. Cloud-init status output is retained.

Before returning a pack failure to the controller's finally destroy, the pack makes one best-effort read-only capture over a fresh connection with the same pinned SSH key. It saves journal output for this run's services, cloud-init output, pinned worker source and worker log/JSONL files in `cloud-failure-fetch.json`, including stderr/exit/error if fetching fails. Missing remote files are marked MISSING. It never collects user-data, private keys or Terraform files. This JSON is private evidence; do not publish raw logs without inspection.

The capture is bounded to the lesser of 60 seconds and remaining work time, including connection/session I/O. It never extends the work/hard envelope or borrows cleanup reserve. A canceled/expired work context records the failed fetch without retrying. Fetch failure never blocks destroy; evidence write failure remains a pack error. Existing UNKNOWN, full-RG verify and verdict semantics are unchanged. Evidence capture is not a cloud workload retry.

## Mandatory local artifact preflight before another VM

Use the exact af8c weed, weed-volume and native.tar.gz together (the test pins all three SHA256s), on a Linux controller or with a cross-built Linux test binary:

```sh
# Requires Go1.25+ for compiling; never requires cloud credentials.
go test -c -o /private/cloud-pack-preflight.test ./packs/seaweed
SEAWEED_CLOUD_ARTIFACT_DIR=/private/af8c-artifacts /private/cloud-pack-preflight.test -test.run '^TestCloudPinnedArtifactsOffline$' -test.v
```

The opt-in preflight drives RunCloudHTTP over loopback pinned SSH, using the production command strings and upload path. It executes real local uploads, all artifact hash checks, unchanged tar extraction/configuration, and the production worker argv with --dry-run. Every remote command gets a real shell syntax check. Service startup argv is parsed by the pinned binaries with help, via a local systemd substitute that never starts a service. The worker native_dir must bind the actual extracted package parent. Discovery without the env flag skips the preflight and performs no artifact work or host contact.

Cloud-init status/ownership, actual systemd startup, HTTP readiness, service journals, native Python3.12 import and live KV are explicitly NOT_RUN offline. An older local glibc may also prevent the pinned volume CLI from loading (current WSL lacks GLIBC_2.38); that is NOT_RUN, not an argv/runtime PASS. The test deliberately returns from the production pack at the live-workload boundary with exit77; it never fabricates an ACK/exact PASS summary. These limitations still need the reviewed cloud smoke; the local setup preflight does not qualify service/native runtime compatibility.
# Ten-node preparation (not executable yet)

Set `cluster:true` in the pinned tfvars for one meta D4s_v5, six volume D2s_v5
and three client D2s_v5 hosts. `plan` describes ten VMs without cloud calls.
The wiring uses the existing module's `for_each`, inventory and per-role ports.
Per-node server keys are generated before apply and stay in private cloud-init
and state; the controller client key is never transferred. The default remains
the qualified one-VM layout. This slice refuses real cluster execution with
`CLUSTER_PACK_UNAVAILABLE` until the reviewed fan-out pack successor lands.
