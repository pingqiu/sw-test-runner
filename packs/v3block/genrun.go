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
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/randomrun"
)

var genHandlers = map[string]tr.ActionHandlerFunc{
	"v3_op_create": blockActions.Handler("v3_op_create"), "v3_op_attach": blockActions.Handler("v3_op_attach"), "v3_op_write": blockActions.Handler("v3_op_write"), "v3_op_read": blockActions.Handler("v3_op_read"),
	"v3_op_snapshot": blockActions.Handler("v3_op_snapshot"), "v3_op_snapshot_delete": blockActions.Handler("v3_op_snapshot_delete"), "v3_op_restore": blockActions.Handler("v3_op_restore"),
	"v3_op_stop": blockActions.Handler("v3_op_stop"), "v3_op_move": blockActions.Handler("v3_op_move"), "v3_op_delete": blockActions.Handler("v3_op_delete"), "v3_op_observe": blockActions.Handler("v3_op_observe"),
	"v3_op_kill": blockActions.Handler("v3_op_kill"), "v3_op_restart": v3OpRestart, "v3_op_healthy": v3OpHealthy,
	"v3_op_fault_apply_store": blockActions.Handler("v3_op_fault_apply_store"), "v3_op_fault_restore_store": blockActions.Handler("v3_op_fault_restore_store"),
	"v3_op_flush":   blockActions.Handler("v3_op_flush"),
	"v3_op_release": blockActions.Handler("v3_op_release"),
}

// The block Driver's rerun is the declaration's (product/actions.json,
// "retry"): the driver re-issues the action, the action never loops.
func (m *blockModel) RequestArg(action string) string { return blockActions.RequestArg(action) }

func (m *blockModel) Rerun(action string, params map[string]string, n int, since time.Duration, out map[string]string) (bool, time.Duration, string) {
	return blockActions.Rerun(action, params, n, since, out)
}

func (m *blockModel) Placeable(action string) bool { return blockActions.Placeable(action) }

func (m *blockModel) Place(action string, params map[string]string, before, after map[string]any) (string, string, bool) {
	return blockActions.Place(action, params, before, after)
}

var _ randomrun.Rerunner = (*blockModel)(nil)

// driven is the handler a SCRIPTED step runs (register.go): an action with a
// declared rerun is re-issued by the same driver function as a random run
// (randomrun.Attempts), its subject observed after each refused attempt
// that is re-issued; any other action is its declared handler. The set is
// the declarations', not a list. The random run's handlers (genHandlers) are
// the bare ones: its Runner re-issues.
func driven(name string) tr.ActionHandlerFunc {
	h := blockActions.Handler(name)
	if blockActions.RequestArg(name) == "" {
		return h
	}
	m := &blockModel{}
	observe := blockActions.Handler("v3_op_observe")
	return func(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
		act.Action = name
		o := GenOp{Action: name, Volume: opParam(act, actx, "volume"), Params: act.Params}
		out, _, err := randomrun.Attempts(ctx, actx, h, act, m, o.Volume, func() (map[string]any, error) {
			for _, s := range m.ObserveSubjects(o) {
				obs := m.Observe(s)
				if _, err := observe(ctx, actx, tr.Action{Action: obs.Action, Node: act.Node, Params: obs.Params}); err != nil {
					return nil, err
				}
			}
			return randomrun.LastObservation(actx.History, o.Volume)
		})
		return out, err
	}
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
		Faults: c.Faults, FaultProcesses: c.FaultProcesses, RestartWithin: c.RestartWithin, MasterOutage: c.MasterOutage,
		InOp: c.InOp, StoreBound: c.StoreBound}
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
		pol, err := unitForRun(ctx, actx, act)
		if err != nil {
			return nil, fmt.Errorf("v3_gen_run: %w", err)
		}
		r.Header = withUnit(r.Header, pol)
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
	if err != nil || c.Evidence {
		return nil, err
	}
	return map[string]any{"guest_connect": c.Label}, nil
}

// In-operation faults (randomrun inop.go).

// HolderOf: the holder serving o's volume now ("blockvolume:<i>", i the
// volume's place in the pool, as KillTarget names holders), none when it is
// not serving, fenced or killed.
func (m *blockModel) HolderOf(o GenOp) (string, bool) {
	for k, n := range m.order {
		if n != o.Volume {
			continue
		}
		v := m.vols[n]
		if v.state != stServing || v.fenced || v.killed {
			return "", false
		}
		return "blockvolume:" + strconv.Itoa(k), true
	}
	return "", false
}

// StorePause: the declared fault actions v3_op_fault_apply_store (a
// fault_apply: the store process paused, with its bounded detached continue)
// and v3_op_fault_restore_store (its fault_restore).
func (m *blockModel) StorePause(e randomrun.FaultEvent, boundSeconds int) (GenOp, GenOp) {
	return GenOp{Action: "v3_op_fault_apply_store", Params: map[string]string{"store": e.Process, "bound_seconds": strconv.Itoa(boundSeconds)}},
		GenOp{Action: "v3_op_fault_restore_store", Params: map[string]string{"store": e.Process}}
}

// AfterStorePause: the requests that follow a store pause around a snapshot
// of a serving volume, which must succeed: a later snapshot (a new name,
// while the volume has room for one) and a detach (the dirty lbas read back
// first, as before any stop). No detach of a side of a pending restore
// pair: it would end the pair's C3 directions (pairs.go); the snapshot is
// its follow-up then.
func (m *blockModel) AfterStorePause(o GenOp) []GenOp {
	v := m.vols[o.Volume]
	if v == nil {
		return nil
	}
	var out []GenOp
	if len(v.snaps) < MaxSnapshots-1 {
		out = append(out, op("v3_op_snapshot", o.Volume, "name", fmt.Sprintf("%s-after%d", o.Volume, o.No)))
	}
	if m.pending(v) && !oldPairs {
		return out
	}
	stop := op("v3_op_stop", o.Volume)
	out = append(out, m.readsOf(v, sortedLBAs(v.dirty), "readback")...)
	return append(out, stop)
}

var (
	_ randomrun.HolderNamer   = (*blockModel)(nil)
	_ randomrun.StoreFaulter  = (*blockModel)(nil)
	_ randomrun.StoreFollower = (*blockModel)(nil)
)

// TargetOp: an operation of action on the first serving volume that allows
// it now (a write of lba 0, a snapshot while there is room), for an armed
// in-operation fault.
func (m *blockModel) TargetOp(action string) (GenOp, bool) {
	for _, n := range m.order {
		v := m.vols[n]
		if v.state != stServing || v.fenced || v.killed || m.pending(v) {
			continue
		}
		switch action {
		case "v3_op_write":
			return op("v3_op_write", n, "lbas", "0", "round", fmt.Sprintf("f%d", m.drawn)), true
		case "v3_op_snapshot":
			if len(v.snaps) < MaxSnapshots-1 {
				return op("v3_op_snapshot", n, "name", fmt.Sprintf("%s-s%d", n, v.snapSeq+1)), true
			}
		}
	}
	return GenOp{}, false
}

var _ randomrun.Targeter = (*blockModel)(nil)
