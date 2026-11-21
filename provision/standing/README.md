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
A systemd timer (every 5 min; the idle clock lives in `/run`, so it restarts at each boot; any inspection failure means "do not stop") treats the box as active while an SSH connection is established or a job marker
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
State lives under `~/.local/state/seaweed-cloud/standing/<name>/` in plan generations `gNNNN/` (module copy, pinned
`box.tfvars.json`, `identity.json` with subscription/RG/name, `standing.tfplan`, Terraform state, `READY`, `ATTEMPT`,
`apply.json`). The tfvars must carry `subscription_id` and name the same RG and box as the command.
- Every plan removes `CURRENT` first, then writes a NEW generation; `CURRENT` names it only after the plan completed.
  Apply runs only the `CURRENT` generation's saved plan, so a failed re-plan leaves nothing apply-eligible.
- Apply writes `ATTEMPT` before Terraform runs; that generation is never "new" again. The live generation (an attempt
  not verified destroyed) binds start/stop/status/destroy: its pinned inputs (sha256-checked) and its subscription/RG.
- A failed, killed or receipt-less attempt stays `UNRESOLVED` across restarts: apply and plan refuse, and destroy + an
  empty RG reports `UNRESOLVED`, never clean. A plan is refused while any generation is applied or unresolved.
- Any read error on generation evidence other than "does not exist" is `UNKNOWN` (fail closed), never "absent".
- A request rejected by input validation (vars, target, token) is not a re-plan: `CURRENT` and the eligible plan stay.

**Crash scope:** the files are written atomically (temp, fsync, rename) and survive a controller **process** restart.
A **machine** crash is out of scope (no directory fsync): afterwards run `seaweed-cloud standing status`, and if
anything is unclear, tear down at the RG level (the box's own RG holds every resource) as the backstop.

## Tests (no account calls)
```
terraform init -backend=false && terraform test        # mock provider: shape, no identity, SSH /32 only, refusals
go test ./cmd/seaweed-cloud/ -run 'Standing|ClassifyPower|PlanAddresses'
```

## On-demand and the reaper
- `priority = "Regular"` runs the same box on demand (e.g. while spot quota is pending): no eviction settings, same
  hardening (no identity, SSH /32 + deny-all). Default stays `Spot`.
- `reaper.sh SEAWEED_CLOUD NAME:RG...` runs on the controller and deallocates any box the idle guard powered off
  (`stopped` = still billing); `REAP_INTERVAL` (default 300 s), `REAP_ONCE=1` for one pass. Anything not `stopped` is
  left alone.
- **Silent failure:** a box whose status stays UNKNOWN, or whose deallocate fails, for `REAP_ALERT_AFTER` passes in a
  row (default 3) gets an `ALERT` line; a single pass then exits 3. `REAP_STATE/last` stamps the last completed pass.
- **Who runs it:** on the workstation, the Windows scheduled task `c04-standing-reaper` runs one pass every 5 min
  (`wsl -d Ubuntu -u root -- env REAP_ONCE=1 bash .../reaper.sh /root/c04-standing/bin/seaweed-cloud NAME:RG...`,
  log appended under the state dir). Every 6-hourly cost line reports `reaper last pass at T` from `REAP_STATE/last`
  and any ALERT; a stamp older than 15 min means the reaper is not running.
- **Switching priority replaces the VM:** `priority` is force-new; flipping an existing box between Spot and Regular
  destroys and recreates it (the OS disk and everything on it are lost). Destroy and re-plan deliberately instead.
- **No server-side backstop for Regular (decision B, Claude01 2026-11-21):** there is no Azure-side shutdown
  schedule (Microsoft.DevTestLab is not registered and was removed on purpose). A **dead controller means a stopped
  Regular box keeps billing** at the full on-demand rate (about USD 0.8/h per D16ads_v5) until the reaper runs again.
  Therefore Regular boxes run only while the reaper runs; every launch notice states a planned end date (at most 48 h,
  renewed explicitly) and the box is destroyed (`standing destroy`) at that date; if the reaper's last pass is more
  than 15 min old, deallocate by hand (`standing stop`) at once.
