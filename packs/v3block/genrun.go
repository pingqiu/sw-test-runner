package v3block

// v3_gen_run: the M1 random run. The generator proposes, the SAME typed
// operation handlers as M0 execute and record, and the outcome is fed back.
// The history gets, besides the operations' own records:
//   - first, an observe record "gen_header": generator version, seed, ops,
//     pool, blocks, negative share and weights (a replay must match them),
//     and the nightly seed the seed was drawn from when param nightly_seed
//     is set;
//   - before each operation, an observe record "gen_step": its number,
//     action, params and the limit a negative probes. That is the replay
//     script, and it ties every operation record to its proposal;
//   - an observation of the volume before every negative operation and after
//     every refusal, so C5 ("a refused operation changed nothing") is judged
//     from two observations around it;
//   - last, an observe record "gen_coverage" (also written to the bundle as
//     gen-coverage.json).
// It stops on a runner error (a handler error), never on a product answer.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	tr "github.com/pingqiu/sw-test-runner"
)

var genHandlers = map[string]tr.ActionHandlerFunc{
	"v3_op_create": v3OpCreate, "v3_op_attach": v3OpAttach, "v3_op_write": v3OpWrite, "v3_op_read": v3OpRead,
	"v3_op_snapshot": v3OpSnapshot, "v3_op_snapshot_delete": v3OpSnapshotDelete, "v3_op_restore": v3OpRestore,
	"v3_op_stop": v3OpStop, "v3_op_move": v3OpMove, "v3_op_delete": v3OpDelete, "v3_op_observe": v3OpObserve,
	"v3_op_flush": v3OpFlush,
}

// outcomeRank orders outcomes for aggregation: the worst wins.
var outcomeRank = map[string]int{tr.OutcomeOK: 0, tr.OutcomeRefused: 1, tr.OutcomeUnknown: 2}

// genExec runs one proposal through its handler. A write over several lbas
// is issued one lba per call (the handler records one operation per lba
// either way) so that every lba's outcome counts: the worst is returned.
func genExec(ctx context.Context, actx *tr.ActionContext, node string, o GenOp) (string, error) {
	h, ok := genHandlers[o.Action]
	if !ok {
		return "", fmt.Errorf("v3_gen_run: no handler for %s", o.Action)
	}
	calls := []map[string]string{o.Params}
	if o.Action == "v3_op_write" {
		calls = nil
		for _, lba := range strings.Fields(o.Params["lbas"]) {
			p := map[string]string{}
			for k, v := range o.Params {
				p[k] = v
			}
			p["lbas"] = lba
			calls = append(calls, p)
		}
	}
	worst := ""
	for _, p := range calls {
		out, err := h(ctx, actx, tr.Action{Action: o.Action, Node: node, Params: p})
		if err != nil {
			return "", err
		}
		got := out["value"]
		if _, known := outcomeRank[got]; !known {
			return "", fmt.Errorf("v3_gen_run: %s returned outcome %q", o.Action, got)
		}
		if worst == "" || outcomeRank[got] > outcomeRank[worst] {
			worst = got
		}
	}
	return worst, nil
}

// genStep records one proposal and executes it: the gen_step record, an
// observation of the volume before a negative operation, the operation through
// genExec, and an observation after a refusal. v3_gen_run and v3_gen_replay
// both run every step through here, so a replayed history has the same shape
// as a recorded one.
func genStep(ctx context.Context, actx *tr.ActionContext, node string, o GenOp) (string, error) {
	observe := func() error {
		_, err := genExec(ctx, actx, node, GenOp{Action: "v3_op_observe", Volume: o.Volume, Params: map[string]string{"volume": o.Volume}})
		return err
	}
	step := map[string]any{"opno": o.No, "action": o.Action, "params": o.Params}
	if o.Negative != "" {
		step["negative"] = o.Negative
	}
	if err := actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: "gen_step", Volume: o.Volume, Result: step}); err != nil {
		return "", err
	}
	if o.Negative != "" {
		if err := observe(); err != nil {
			return "", err
		}
	}
	outcome, err := genExec(ctx, actx, node, o)
	if err != nil {
		return "", err
	}
	if outcome == tr.OutcomeRefused {
		if err := observe(); err != nil {
			return "", err
		}
	}
	return outcome, nil
}

// genConfig reads the run's parameters.
func genConfig(act tr.Action, actx *tr.ActionContext) (GenConfig, error) {
	var c GenConfig
	var err error
	if c.Seed, err = strconv.ParseUint(opParam(act, actx, "seed"), 10, 64); err != nil {
		return c, fmt.Errorf("v3_gen_run: seed: %w", err)
	}
	if c.Ops, err = strconv.Atoi(opParam(act, actx, "ops")); err != nil {
		return c, fmt.Errorf("v3_gen_run: ops: %w", err)
	}
	if c.Blocks, err = strconv.Atoi(opParam(act, actx, "blocks")); err != nil {
		return c, fmt.Errorf("v3_gen_run: blocks: %w", err)
	}
	c.Pool = strings.Fields(opParam(act, actx, "pool"))
	if s := opParam(act, actx, "negative"); s != "" {
		if c.Negative, err = strconv.ParseFloat(s, 64); err != nil {
			return c, fmt.Errorf("v3_gen_run: negative: %w", err)
		}
	}
	if s := opParam(act, actx, "weights"); s != "" {
		c.Weights = map[string]int{}
		for _, kv := range strings.Fields(s) {
			k, v, ok := strings.Cut(kv, "=")
			n, err := strconv.Atoi(v)
			if !ok || err != nil {
				return c, fmt.Errorf("v3_gen_run: weight %q is not action=integer", kv)
			}
			c.Weights[k] = n
		}
	}
	return c, nil
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
	header := g.Header()
	// The nightly seed the run's seed was drawn from, when a nightly driver set
	// one: the header then names both.
	if ns := opParam(act, actx, "nightly_seed"); ns != "" {
		if _, err := strconv.ParseUint(ns, 10, 64); err != nil {
			return nil, fmt.Errorf("v3_gen_run: nightly_seed: %w", err)
		}
		header["nightly_seed"] = ns
	}
	if err := actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: "gen_header", Result: header}); err != nil {
		return nil, err
	}
	for {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("v3_gen_run: %w after %d operations", ctx.Err(), g.no)
		}
		o, ok := g.Next()
		if !ok {
			break
		}
		outcome, err := genStep(ctx, actx, act.Node, o)
		if err != nil {
			return nil, fmt.Errorf("v3_gen_run: operation %d (%s %s): %w", o.No, o.Action, o.Volume, err)
		}
		g.Learn(o, outcome)
	}
	cov := g.Coverage()
	if err := actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: "gen_coverage", Result: cov}); err != nil {
		return nil, err
	}
	if actx.Bundle != nil {
		b, _ := json.MarshalIndent(cov, "", "  ")
		if err := os.WriteFile(filepath.Join(actx.Bundle.Dir, "gen-coverage.json"), b, 0o644); err != nil {
			return nil, err
		}
	}
	actx.Log("  v3_gen_run: %d operations, ended %s", g.no, g.Ended())
	return map[string]string{"value": g.Ended(), "proposed": strconv.Itoa(g.no)}, nil
}
