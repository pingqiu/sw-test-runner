package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

// RestoreReserve is the block cloud scope's restore bound: a VM start and the
// wait for its new boot (at least the runner's MinPowerRestoreTimeout).
const RestoreReserve = 6 * time.Minute

// OpenScope opens the fault custody of a block cloud run from its pack
// context: the scope is bound to the context's digest and the inventory's run
// id and deadlines, and its verifier re-reads both and requires them
// unchanged. outputDir is the run bundle's absolute directory.
func OpenScope(ctx context.Context, contextPath, outputDir string) (*tr.RunScope, error) {
	run, err := LoadRun(contextPath)
	if err != nil {
		return nil, err
	}
	b := tr.BlockCloudBinding{RunID: run.RunID, ContextPath: filepath.ToSlash(contextPath), OutputDir: filepath.ToSlash(outputDir),
		WorkDeadline: run.Work, HardDeadline: run.Hard, RestoreReserve: RestoreReserve}
	return tr.NewBlockCloudRunScope(ctx, b, func(_ context.Context, b tr.BlockCloudBinding) error {
		again, err := LoadRun(contextPath)
		switch {
		case err != nil:
			return err
		case again.RunID != b.RunID || !again.Work.Equal(b.WorkDeadline) || !again.Hard.Equal(b.HardDeadline):
			return fmt.Errorf("pack context and inventory disagree with the scope (run %q, deadlines %s..%s)", again.RunID, again.Work, again.Hard)
		}
		return nil
	})
}

// RegisterPack registers the cloud fault actions. Both are mutating (they
// act on cloud resources) and chaos tier.
//
//	cloud_vm_power          vm: <inventory host id>  -> fault_key
//	cloud_vm_power_restore  vm: <inventory host id>
//
// The key is vm, not node: in a scenario node: names the runner node an
// action runs on (Action.Node), and params are inline keys.
//
// The restore action is the scenario's explicit restore, recorded before the
// history closes; the scope's Close restores anything left, in reverse order.
func RegisterPack(r *tr.Registry) {
	r.RegisterMutatingFunc("cloud_vm_power", tr.TierChaos, vmPowerOff)
	r.SetRequiredParams("cloud_vm_power", []string{"vm"})
	r.RegisterMutatingFunc("cloud_vm_power_restore", tr.TierChaos, vmPowerRestore)
	r.SetRequiredParams("cloud_vm_power_restore", []string{"vm"})
}

// plan builds the power fault of one inventory host from the run's context.
func plan(actx *tr.ActionContext, node string) (tr.WitnessedPowerFault, error) {
	if actx.Safety == nil {
		return tr.WitnessedPowerFault{}, errors.New("cloud_vm_power: no fault custody: the run has no " + ContextEnv)
	}
	if actx.History == nil || actx.Bundle == nil {
		return tr.WitnessedPowerFault{}, errors.New("cloud_vm_power: a fault is recorded in the run's history and bundle; run with a bundle")
	}
	run, err := LoadRun(os.Getenv(ContextEnv))
	if err != nil {
		return tr.WitnessedPowerFault{}, err
	}
	h, ok := run.Hosts[node]
	if !ok {
		return tr.WitnessedPowerFault{}, fmt.Errorf("cloud_vm_power: %q is not a host of the run's inventory", node)
	}
	scratch := filepath.Join(actx.Bundle.Dir, "faults")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return tr.WitnessedPowerFault{}, err
	}
	id := h.ResourceID
	return tr.WitnessedPowerFault{
		Node: node, Resource: id, Owned: run.Owned, RestoreTimeout: tr.MinPowerRestoreTimeout,
		PowerOff: func(ctx context.Context) error { return PowerOff(ctx, id) },
		PowerOn:  func(ctx context.Context) error { return PowerOn(ctx, id) },
		View:     func(ctx context.Context) (tr.VMPowerView, error) { return View(ctx, id) },
		BootID:   func(ctx context.Context) (string, error) { return BootID(ctx, h, run.SSHKey, scratch) },
		History:  actx.History, Client: "runner",
	}, nil
}

func vmPowerOff(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	p, err := plan(actx, act.Params["vm"])
	if err != nil {
		return nil, err
	}
	err = actx.Safety.ApplyWitnessedPowerFault(ctx, p.Key(), p)
	werr := writeReceipts(actx)
	if err != nil {
		return nil, errors.Join(err, werr)
	}
	return map[string]string{"fault_key": p.Key()}, werr
}

func vmPowerRestore(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	p, err := plan(actx, act.Params["vm"])
	if err != nil {
		return nil, err
	}
	err = actx.Safety.ClearFault(ctx, p.Key())
	return nil, errors.Join(err, writeReceipts(actx))
}

// writeReceipts writes every fault receipt of the scope into the bundle
// (faults/receipts.json), replacing the previous copy atomically.
func writeReceipts(actx *tr.ActionContext) error {
	raw, err := json.MarshalIndent(actx.Safety.FaultReceipts(), "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Join(actx.Bundle.Dir, "faults")
	tmp := filepath.Join(dir, "receipts.json.tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "receipts.json"))
}

// UsesFaults reports whether a scenario applies a cloud fault action.
func UsesFaults(s *tr.Scenario) bool {
	if s == nil {
		return false
	}
	for _, ph := range s.Phases {
		for _, a := range ph.Actions {
			if a.Action == "cloud_vm_power" || a.Action == "cloud_vm_power_restore" {
				return true
			}
		}
	}
	return false
}
