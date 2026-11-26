# Cloud VM power fault

Actions (mutating, chaos tier; the run needs `--allow-mutating`). The key is `vm`: in a scenario `node:` is the runner node an action runs on, and params are inline keys.

| Action | Params | Effect |
|---|---|---|
| `cloud_vm_power` | `vm`: an inventory host id | powers that host's VM off; output `fault_key` |
| `cloud_vm_power_restore` | `vm` | powers it on again and proves a new boot |

## Which power action

`cloud_vm_power` is an **abrupt stop of the machine**: `az vm stop --skip-shutdown`, a power cut with no guest shutdown. The guest gets no chance to flush, unmount or stop services. The checkers judge an abrupt stop and a clean shutdown the same way, but only the abrupt stop tests the durability promise (an acknowledged write survives a power loss). Do not replace it with a guest shutdown or a deallocate.

Applied means the VM reports stopped and the node no longer answers on its management link. Restored means the VM is running and the node answers with a boot id different from the one before.

## Restore before the judge

The fault's custody is the run's block cloud scope. The scope's `Close` restores any fault still applied, in reverse order, even when the run fails. But `Close` runs **after** the history is closed, so a restore made there is not recorded. A scenario restores each fault explicitly with `cloud_vm_power_restore` before its judge phase. `Close` is the safety net, not the record: without the explicit restore, C0 reports `fault left applied`.

```yaml
- name: fault
  actions:
    - action: cloud_vm_power
      vm: node-2
    # ... operations while the node is off ...
    - action: cloud_vm_power_restore
      vm: node-2
- name: judge
  always: true
  actions:
    - action: v3_history_end
    # ... checkers ...
```

## What it guarantees and what it does not

- The restore is **fault custody**: the run does not leave its own fault applied.
- **No VM left billing** is guaranteed only by the cloud run's terminal destroy and verify. A run that dies with a VM powered off is settled there.

## Custody and inputs

The CLI opens the block cloud scope only when `SEAWEED_CLOUD_PACK_CONTEXT` is set and the scenario uses one of these actions. The scope is bound to:

- the pack context's digest;
- the inventory's run id and its work and hard deadlines;
- a 6 min restore reserve, which must fit between work and hard.

The context and its inventory must agree: the same run id, and host keys only for inventory hosts.

## In the history

A fault is two operations of module Runner, each an invoke before the act and a complete after its observation:

- `fault_apply` / `Runner.VmPowerOff`
- `fault_restore` / `Runner.VmPowerOn`

Args are `{fault_key, resource, vm_id, node}`. The fault is active from the apply's invoke to the restore's ok complete.

An unknown or missing fault outcome is a runner failure. After it, C1 to C3 answer UNKNOWN for every volume. The history carries no volume-to-VM map yet; one can be derived from the attach records when it is needed. Receipts are in the bundle at `faults/receipts.json`.
