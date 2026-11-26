package testrunner

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// VMPowerView is one read of a VM's instance view: its stable vmId and its
// power state ("running", "stopped", "deallocated", ...).
type VMPowerView struct {
	VMID, PowerState string
}

// WitnessedPowerFault is a VM power fault run from the controller: its apply
// and restore are cloud calls, not node jobs, and the node's boot changes by
// design, so it is a sibling of ApplyWitnessedFault, not a kind inside it.
// The pack supplies the calls; it is trusted code, as for the other faults.
type WitnessedPowerFault struct {
	Node     string                      // the inventory host
	Resource string                      // the VM's ARM id (inventory hosts[].resource_id)
	Owned    []string                    // the run's managed_resource_ids
	PowerOff func(context.Context) error // e.g. az vm stop --ids ID --skip-shutdown
	PowerOn  func(context.Context) error // e.g. az vm start --ids ID
	View     func(context.Context) (VMPowerView, error)
	// BootID reads the node's boot id over its management link; an error
	// means the node is unreachable.
	BootID func(context.Context) (string, error)
	// RestoreTimeout bounds the power-on and the wait for a running VM with
	// a readable new boot; it must cover a real start.
	RestoreTimeout time.Duration
	// History, when set, records the fault as two operations of module
	// Runner: fault_apply (Runner.VmPowerOff) and fault_restore
	// (Runner.VmPowerOn), each invoke/complete, args {fault_key, resource,
	// vm_id}, result the receipt's observation. The fault is active from the
	// apply's invoke to the restore's complete.
	History *History
	Client  string
}

// MinPowerRestoreTimeout is the least restore bound accepted: an Azure VM
// start plus guest boot and sshd typically take 1 to 3 minutes.
const MinPowerRestoreTimeout = 5 * time.Minute

var vmResourceID = regexp.MustCompile(`(?i)^/subscriptions/[^/]+/resourceGroups/[^/]+/providers/Microsoft\.Compute/virtualMachines/[^/]+$`)

// ErrFaultNotApplied: the fault was refused before any act on the VM.
var ErrFaultNotApplied = errors.New("power fault NOT applied")

func (p WitnessedPowerFault) Key() string { return p.Node + "/" + p.Resource }

// validate is every refusal before anything is observed or changed.
func (p WitnessedPowerFault) validate() error {
	switch {
	case p.Node == "" || p.Resource == "":
		return errors.New("power fault: node and resource are required")
	case !vmResourceID.MatchString(p.Resource):
		return fmt.Errorf("power fault: %s is not a virtual machine id", p.Resource)
	case !ownedResource(p.Owned, p.Resource):
		return fmt.Errorf("power fault: %s is not one of this run's managed resources", p.Resource)
	case p.PowerOff == nil || p.PowerOn == nil || p.View == nil || p.BootID == nil:
		return errors.New("power fault: incomplete plan")
	case p.RestoreTimeout < MinPowerRestoreTimeout:
		return fmt.Errorf("power fault: restore bound %s is below %s (a real VM start)", p.RestoreTimeout, MinPowerRestoreTimeout)
	}
	return nil
}

// ownedResource: Azure ids are case-insensitive.
func ownedResource(owned []string, id string) bool {
	for _, o := range owned {
		if strings.EqualFold(o, id) {
			return true
		}
	}
	return false
}

func poweredOff(state string) bool { return state == "stopped" || state == "deallocated" }

// ApplyWitnessedPowerFault powers the node's VM off and restores it under
// the run's fault obligations (same receipts and restore custody as the
// other faults). Its rules:
//   - identity is the ARM id and the instance view's vmId, in every
//     observation: a VM re-created under the same name is refused;
//   - Applied: powered off (stopped or deallocated) AND the node unreachable;
//     the BootID is empty by declaration, never copied from Before;
//   - Restored: running AND a BootID read from the node that differs from
//     Before; the same BootID, or none readable, is NOT_PROVEN;
//   - at cleanup a VM already running with a new boot needs no restore; any
//     other state runs the power-on.
//
// An approval authority is not supported yet: its cleanup grants and
// readbacks are node jobs with a fixed boot.
func (s *RunScope) ApplyWitnessedPowerFault(actionCtx context.Context, key string, p WitnessedPowerFault) error {
	if key != p.Key() {
		return errors.New("power fault: key differs from its plan")
	}
	if err := p.validate(); err != nil {
		return err
	}
	if s.authority != nil {
		return errors.New("power fault: not supported under an approval authority")
	}
	receipt := FaultReceipt{Kind: "vm_power", Node: p.Node, Resource: p.Resource}
	store := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.faultReceipts == nil {
			s.faultReceipts = make(map[string]FaultReceipt)
		}
		s.faultReceipts[key] = receipt
	}
	var vmID string // the identity bound at the baseline
	// History: invoke before the act, complete after its observation.
	// A fault that cannot be recorded is not applied: a failed apply invoke
	// refuses before the power-off. A failed restore invoke still restores
	// (custody first) and fails the action; C0 then finds no recorded restore.
	var restoreSeq uint64
	var restoreRecErr error
	invoke := func(verb, op string) (uint64, error) {
		if p.History == nil {
			return 0, nil
		}
		seq, err := p.History.Invoke(HistoryRecord{Client: p.Client, Op: op, Verb: verb,
			Args: map[string]any{"fault_key": key, "resource": p.Resource, "vm_id": vmID, "node": p.Node}})
		if err != nil {
			return 0, fmt.Errorf("power fault: history invoke of %s: %w", verb, err)
		}
		return seq, nil
	}
	complete := func(seq uint64, ok bool, x FaultObservation, extra map[string]any) error {
		if p.History == nil || seq == 0 {
			return nil
		}
		res := map[string]any{"power": x.State["power"], "node": x.State["node"], "boot_changed": x.BootID != "" && x.BootID != receipt.Before.BootID}
		for k, v := range extra {
			res[k] = v
		}
		outcome := OutcomeOK
		if !ok {
			outcome = OutcomeUnknown
		}
		if err := p.History.Complete(seq, outcome, res); err != nil {
			return fmt.Errorf("power fault: history complete: %w", err)
		}
		return nil
	}
	// observe reads the instance view (identity checked against the
	// baseline's vmId) and, when asked, the node's boot.
	observe := func(ctx context.Context, wantBoot bool) (FaultObservation, error) {
		x := FaultObservation{Node: p.Node, Resource: p.Resource, ReadStart: time.Now()}
		v, err := p.View(ctx)
		x.State = map[string]string{"power": v.PowerState, "vm_id": v.VMID}
		if err == nil && (v.VMID == "" || v.PowerState == "" || (vmID != "" && !strings.EqualFold(v.VMID, vmID))) {
			err = errors.New("power fault: VM identity/state UNKNOWN (vmId changed or missing)")
		}
		if err == nil && wantBoot {
			var bootErr error
			if x.BootID, bootErr = p.BootID(ctx); bootErr != nil {
				x.BootID = ""
				x.State["node"] = "unreachable"
			} else {
				x.State["node"] = "reachable"
			}
		}
		x.ReadEnd = time.Now()
		return x, errors.Join(err, ctx.Err())
	}
	baseline := false
	return s.ApplyFault(key, func(workCtx context.Context) error {
		ctx, cancel := context.WithCancel(actionCtx)
		stop := context.AfterFunc(workCtx, cancel)
		defer stop()
		defer cancel()
		var err error
		receipt.Before, err = observe(ctx, true)
		store()
		if err != nil || receipt.Before.State["power"] != "running" || receipt.Before.BootID == "" {
			return errors.Join(err, errors.New("power fault: baseline is not a running VM with a readable boot"))
		}
		vmID = receipt.Before.State["vm_id"]
		applySeq, err := invoke("fault_apply", "Runner.VmPowerOff")
		if err != nil {
			return errors.Join(ErrFaultNotApplied, err) // nothing was done; nothing to restore
		}
		baseline = true
		applyErr := p.PowerOff(ctx)
		var probeErr error
		receipt.Applied, probeErr = observe(ctx, true)
		receipt.Applied.BootID = "" // by declaration: the node is off
		store()
		if !poweredOff(receipt.Applied.State["power"]) || receipt.Applied.State["node"] != "unreachable" {
			probeErr = errors.Join(probeErr, errors.New("power fault effect NOT_PROVEN (not powered off, or the node still answers)"))
		}
		recErr := complete(applySeq, applyErr == nil && probeErr == nil, receipt.Applied, nil)
		return errors.Join(applyErr, probeErr, recErr)
	}, func(ctx context.Context) error {
		if !baseline {
			return nil
		}
		restoreSeq, restoreRecErr = invoke("fault_restore", "Runner.VmPowerOn")
		ctx, cancel := context.WithTimeout(ctx, p.RestoreTimeout)
		defer cancel()
		current, err := observe(ctx, true)
		if err != nil {
			return err
		}
		if current.State["power"] == "running" && current.BootID != "" && current.BootID != receipt.Before.BootID {
			receipt.RestoreNotNeeded = true
			store()
			return nil
		}
		if err := p.PowerOn(ctx); err != nil {
			return fmt.Errorf("power fault restore: %w", err)
		}
		for {
			current, err = observe(ctx, true)
			if err == nil && current.State["power"] == "running" && current.BootID != "" {
				return nil
			}
			if ctx.Err() != nil {
				return errors.Join(err, errors.New("power fault restore: VM not running with a readable boot before the bound"))
			}
			time.Sleep(2 * time.Second)
		}
	}, func(ctx context.Context) error {
		if !baseline {
			return errors.New("no qualified baseline; no restoration claim")
		}
		var err error
		receipt.Restored, err = observe(ctx, true)
		store()
		if receipt.Restored.State["power"] != "running" || receipt.Restored.BootID == "" || receipt.Restored.BootID == receipt.Before.BootID {
			err = errors.Join(err, errors.New("power fault restoration NOT_PROVEN (not running, boot unreadable, or the same boot)"))
		}
		err = errors.Join(err, restoreRecErr, complete(restoreSeq, err == nil, receipt.Restored, map[string]any{"restore_not_needed": receipt.RestoreNotNeeded}))
		restoreSeq, restoreRecErr = 0, nil
		return err
	})
}
