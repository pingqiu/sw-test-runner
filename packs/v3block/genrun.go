package v3block

// v3_gen_run: the M1 random run (M2 with param faults). The generic run loop
// (randomrun.Runner.Run) drives the generator over the block model; the SAME
// typed operation handlers as M0 execute and record, and the outcome is fed
// back. The history's shape (gen_header, fault_schedule, gen_step, the
// observations around negatives and refusals, fault events with healthy
// points and resolutions, gen_coverage) is documented in randomrun/run.go.
// Params: seed, ops, blocks, pool, negative, weights, nightly_seed, and for
// faults: faults (a rate above 0), fault_processes, fault_restart_within,
// fault_healthy_wait (v3_op_healthy's wait_seconds), fault_master_outage (the
// rate of short master outages).

import (
	"context"
	"fmt"
	"strconv"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/actions"
	"github.com/pingqiu/sw-test-runner/randomrun"
)

var genHandlers = map[string]tr.ActionHandlerFunc{
	"v3_op_create": blockActions.Handler("v3_op_create"), "v3_op_attach": v3OpAttach, "v3_op_write": v3OpWrite, "v3_op_read": v3OpRead,
	"v3_op_snapshot": blockActions.Handler("v3_op_snapshot"), "v3_op_snapshot_delete": blockActions.Handler("v3_op_snapshot_delete"), "v3_op_restore": blockActions.Handler("v3_op_restore"),
	"v3_op_stop": v3OpStop, "v3_op_move": v3OpMove, "v3_op_delete": blockActions.Handler("v3_op_delete"), "v3_op_observe": v3OpObserve,
	"v3_op_kill": v3OpKill, "v3_op_restart": v3OpRestart, "v3_op_healthy": v3OpHealthy,
	"v3_op_flush":   v3OpFlush,
	"v3_op_release": blockActions.Handler("v3_op_release"),
}

// genRunner executes through the block handlers (read when called, so a
// test's installed handlers count).
func genRunner(name, node string, m *blockModel) randomrun.Runner {
	return randomrun.Runner{Name: name, Node: node, Driver: m, Handlers: genHandlers, Header: guestHeader}
}

// genExec runs one proposal through its handler (a write one lba per call);
// the worst outcome is returned.
func genExec(ctx context.Context, actx *tr.ActionContext, node string, o GenOp) (string, error) {
	return genRunner("v3_gen_run", node, &blockModel{}).Exec(ctx, actx, o)
}

// genConfig reads the run's parameters: the generic ones and blocks.
func genConfig(act tr.Action, actx *tr.ActionContext) (GenConfig, error) {
	c, err := randomrun.ParseConfig("v3_gen_run", func(k string) string { return opParam(act, actx, k) })
	cfg := GenConfig{Seed: c.Seed, Ops: c.Ops, Pool: c.Pool, Negative: c.Negative, Weights: c.Weights,
		Faults: c.Faults, FaultProcesses: c.FaultProcesses, RestartWithin: c.RestartWithin, MasterOutage: c.MasterOutage}
	if err != nil {
		return cfg, err
	}
	if cfg.Blocks, err = strconv.Atoi(opParam(act, actx, "blocks")); err != nil {
		return cfg, fmt.Errorf("v3_gen_run: blocks: %w", err)
	}
	return cfg, nil
}

func v3GenRun(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	if actx.History == nil {
		return nil, fmt.Errorf("v3_gen_run: no history (typed operations run only with a run bundle)")
	}
	cfg, err := genConfig(act, actx)
	if err != nil {
		return nil, err
	}
	g, err := NewGenerator(cfg)
	if err != nil {
		return nil, err
	}
	r := genRunner("v3_gen_run", act.Node, g.m)
	if g.Faulty() {
		// A fault run plays the holder's unit (faults.go supervise) with the
		// policy read from the product's unit file; the header says so.
		var node runner // only to read the policy, once
		if actx.Vars[unitPolicyVar] == "" {
			n, err := actions.GetNode(actx, act.Node)
			if err != nil {
				return nil, fmt.Errorf("v3_gen_run: %w", err)
			}
			node = n
		}
		pol, err := loadUnitPolicy(ctx, actx, node, opEnv{work: opParam(act, actx, "work")})
		if err != nil {
			return nil, fmt.Errorf("v3_gen_run: %w", err)
		}
		base := r.Header
		r.Header = func(get func(string) string) (map[string]any, error) {
			h, err := base(get)
			if h == nil {
				h = map[string]any{}
			}
			h["unit"] = unitHeader(pol)
			return h, err
		}
	}
	return r.Run(ctx, actx, g.Generator, func(k string) string { return opParam(act, actx, k) })
}

// guestHeader names a declared guest connect variant in the run's header,
// "guest connect variant: <values>": its results never stand in for the
// plain connect's. The plain connect (the product's, the only release
// evidence) adds nothing, so a plain run's header is unchanged; every
// attach records the connect in effect either way.
func guestHeader(get func(string) string) (map[string]any, error) {
	c, err := selectConnect(get(guest.Connect.SelectVar))
	if err != nil || c.Options == "" {
		return nil, err
	}
	return map[string]any{"guest_connect": c.Label}, nil
}
