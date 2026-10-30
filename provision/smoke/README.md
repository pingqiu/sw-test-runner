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

The native archive must contain `native/seaweedkv_native`. The cloud HTTP smoke uses the OS filesystem under the inventory run_root, with no disk formatting. The declared cache disk stays unformatted. This is an infrastructure smoke, not cache-disk performance or rotation qualification.

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

The existing native shutdown schedule/expiry tag remains a compute backstop, not disk/IP cleanup. Provider errors may prevent arming. Hard-deadline teardown is best effort; run destroy again after session death and inspect remaining IDs.

UNKNOWN apply stays sticky: destroy + an empty list cannot prove that an unresolved create will not finish later. `CREATE_UNRESOLVED` never becomes `COMPLETE` without resolving that pending create; remaining IDs and nonzero status must be reported to the owner. `verify` is locally bounded to 30 seconds. The RG/RBAC stay.

## Verification limits

CPU tests drive the actual native command boundary with fake Terraform/az, including a late asynchronous create after recovery's empty list. Loopback SSH tests use a real handshake to prove pinned-key success/mismatch refusal and exercise transfers/phase calls. Their server simulates service/worker output: they do not execute weed/cloud-init/the native connector or prove Azure service compatibility. Existing M01/M02 strict tests remain unchanged. Cloud qualification still needs the manager token.
