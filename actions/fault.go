package actions

import (
	"context"
	"fmt"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/infra"
)

// RegisterFaultActions registers fault injection actions.
func RegisterFaultActions(r *tr.Registry) {
	r.RegisterFunc("inject_netem", tr.TierChaos, injectNetemAction)
	r.RegisterFunc("inject_partition", tr.TierChaos, injectPartitionAction)
	r.RegisterFunc("fill_disk", tr.TierChaos, fillDiskAction)
	r.RegisterFunc("corrupt_wal", tr.TierChaos, corruptWALAction)
	r.RegisterFunc("clear_fault", tr.TierChaos, clearFaultAction)
}

func injectNetemAction(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	return applyWitnessedFault(ctx, actx, act, "netem")
}

func injectPartitionAction(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	return applyWitnessedFault(ctx, actx, act, "partition")
}

func fillDiskAction(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	return applyWitnessedFault(ctx, actx, act, "fill_disk")
}

func applyWitnessedFault(ctx context.Context, actx *tr.ActionContext, act tr.Action, kind string) (map[string]string, error) {
	p, ok := actx.FaultPlans[act.Params["plan"]]
	if actx.Safety == nil || !ok || p.Kind != kind || p.Node != act.Node {
		return nil, fmt.Errorf("%s requires an owned scope and matching witnessed fault plan", kind)
	}
	return map[string]string{"fault_key": p.Key()}, actx.Safety.ApplyWitnessedFault(ctx, p.Key(), p)
}

func corruptWALAction(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	targetName := act.Target
	if targetName == "" {
		return nil, fmt.Errorf("corrupt_wal: target param required")
	}

	tgt, err := getHATarget(actx, targetName)
	if err != nil {
		return nil, err
	}

	nBytes := ParseInt(act.Params["bytes"], 4096)

	return nil, infra.CorruptWALRegion(ctx, tgt.Node, tgt.VolFilePath(), nBytes)
}

func clearFaultAction(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	p, ok := actx.FaultPlans[act.Params["plan"]]
	if actx.Safety == nil || !ok || p.Node != act.Node {
		return nil, fmt.Errorf("clear_fault requires its witnessed plan")
	}
	return nil, actx.Safety.ClearFault(ctx, p.Key())
}
