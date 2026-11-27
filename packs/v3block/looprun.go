package v3block

// v3_loop_run: the tier-1 declared workload loop (randomrun/loop.go and
// looprun.go) over the block's typed operations, a thin wrapper like
// v3_gen_run. The loop is data: product/loops.json. The SAME handlers as the
// random run execute and record, the same fault schedule runs between cycle
// operations, and the block Driver (its zero model) says how a write is
// split, how a volume is observed and how an unknown is resolved. Params:
// loop, seed, pool, blocks, iterations, duration, and for faults: faults,
// fault_processes, fault_restart_within, fault_healthy_wait.
//
// v3_loop_replay re-executes a recorded loop history (param history).

import (
	"context"
	_ "embed"
	"fmt"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/randomrun"
)

//go:embed product/loops.json
var loopsJSON []byte

// blockLoops reads the block's loops, every action checked against the
// handlers the loop runs through.
func blockLoops() (randomrun.LoopFile, error) {
	return randomrun.ParseLoops(loopsJSON, func(a string) bool { _, ok := genHandlers[a]; return ok })
}

func loopRunner(name, node string) randomrun.LoopRunner {
	return randomrun.LoopRunner{Runner: randomrun.Runner{Name: name, Node: node, Driver: &blockModel{}, Handlers: genHandlers}}
}

func v3LoopRun(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	if actx.History == nil {
		return nil, fmt.Errorf("v3_loop_run: no history (typed operations run only with a run bundle)")
	}
	get := func(k string) string { return opParam(act, actx, k) }
	f, err := blockLoops()
	if err != nil {
		return nil, fmt.Errorf("v3_loop_run: product/loops.json: %w", err)
	}
	name := get("loop")
	l, ok := f.Loops[name]
	if !ok {
		return nil, fmt.Errorf("v3_loop_run: no loop %q in product/loops.json", name)
	}
	c, err := randomrun.ParseLoopConfig("v3_loop_run", get)
	if err != nil {
		return nil, err
	}
	c.Name, c.Loop = name, l
	lr := loopRunner("v3_loop_run", act.Node)
	if c.Faults != 0 {
		// A fault loop plays the holder's unit too; its header says so.
		pol, err := unitForRun(ctx, actx, act)
		if err != nil {
			return nil, fmt.Errorf("v3_loop_run: %w", err)
		}
		lr.Header = withUnit(nil, pol)
	}
	return lr.RunLoop(ctx, actx, c)
}

func v3LoopReplay(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	return loopRunner("v3_loop_replay", act.Node).ReplayLoop(ctx, actx, opParam(act, actx, "history"))
}
