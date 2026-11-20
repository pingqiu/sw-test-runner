# Standing test box (Azure spot)

One standing Linux box for smoke work (root, dm-flakey, NVMe/TCP loopback, swap smoke) that would otherwise contend on
WSL. Later, optionally, a self-hosted CI runner for trusted branches only (separate approval; the registration token is
handed over by the owner, never fetched here).

## Shape
- `Standard_D32ads_v5` (32 vCPU, 128 GiB, local NVMe temp disk = scratch, wiped on every deallocate), `westus2`.
- Spot, `eviction_policy = Deallocate` (OS disk kept), `max_bid_price = -1` (evicted for capacity only, never price).
- Own RG `rg-<seat>-standing-<yyyymmdd>` (created here unless `create_resource_group = false`), own VNet/subnet, NSG
  with exactly one inbound allow: SSH from the operator's `/32`.
- No Azure identity on the VM, no credentials, no lab or share access; the SSH key pair is made for this box only and
  its private key stays on the workstation.
- Premium SSD OS disk 256 GiB.

## Idle guard
A systemd timer (every 5 min) treats the box as active while an SSH connection is established or a job marker
`/run/standing/job-*` exists (a CI job must write one while it runs). After `idle_minutes` (default 60) of neither, it
powers the OS off.

**Billing caveat:** an OS power-off leaves the VM `stopped` (still allocated) and **compute keeps billing**; only a
deallocate stops it. Without an identity on the box, deallocate comes from the controller:
`seaweed-cloud standing stop` (and `status` reports `compute_billing: yes` for `stopped`).

## Controller commands (Linux controller)
```
seaweed-cloud standing plan    --name N --resource-group RG --module provision/standing --vars box.tfvars.json
seaweed-cloud standing apply   --name N --resource-group RG --launch-token ID   # applies exactly the reviewed plan
seaweed-cloud standing start|stop|status --name N --resource-group RG          # stop = deallocate
seaweed-cloud standing destroy --name N --resource-group RG --launch-token ID   # then RG listed empty / gone
```
State and the plan live under `~/.local/state/seaweed-cloud/standing/<name>/`.

## Tests (no account calls)
```
terraform init -backend=false && terraform test        # mock provider: shape, no identity, SSH /32 only, refusals
go test ./cmd/seaweed-cloud/ -run 'Standing|ClassifyPower|PlanAddresses'
```
