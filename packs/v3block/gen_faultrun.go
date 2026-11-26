package v3block

// Executing a fault schedule inside v3_gen_run and v3_gen_replay (both run
// it through faultRun, so a replayed history has the same shape):
//   - after operation k completes (and is learnt), every event with
//     after_op k runs through the typed actions v3_op_kill / v3_op_restart;
//   - after each restart, v3_op_healthy records a `healthy` observation; at a
//     healthy point (it answers healthy=true) every operation whose outcome
//     was unknown since the last healthy point is RESOLVED by observation:
//     a write by reading its lbas (v3_op_read), anything else by observing its
//     volume (and a restore's target) with v3_op_observe (kb C6). A read that
//     does not answer ok leaves the write pending for the next healthy point;
//   - at the end of the run a process still down is restarted (its scheduled
//     restart lay after the last operation), and the run ends on a healthy
//     point with its resolution, so no unknown is left unobserved.
// No time decides anything here, and nothing is judged: the observations are
// facts for the checkers.

import (
	"context"
	"fmt"

	tr "github.com/pingqiu/sw-test-runner"
)

type faultRun struct {
	events  []FaultEvent
	next    int    // the first event not yet run
	down    string // the process down now
	pending []GenOp
	gen     *Generator // nil in a replay
	wait    string     // v3_op_healthy's wait_seconds ("" for its default: look once)
}

// newFaultRun takes the header's fault_healthy_wait (absent: "").
func newFaultRun(events []FaultEvent, g *Generator, header map[string]any) *faultRun {
	w, _ := header["fault_healthy_wait"].(string)
	return &faultRun{events: events, gen: g, wait: w}
}

// note takes an operation's outcome: an unknown is pending resolution. A read
// changes nothing, so an unknown read has nothing to resolve.
func (f *faultRun) note(o GenOp, outcome string) {
	if outcome == tr.OutcomeUnknown && o.Action != "v3_op_read" {
		f.pending = append(f.pending, o)
	}
}

// afterOp runs the events scheduled after operation opno.
func (f *faultRun) afterOp(ctx context.Context, actx *tr.ActionContext, node string, opno int) error {
	for f.next < len(f.events) && f.events[f.next].AfterOp <= opno {
		if err := f.run(ctx, actx, node, f.events[f.next]); err != nil {
			return err
		}
		f.next++
	}
	return nil
}

// finish ends the run: a process still down is restarted (with its healthy
// point); otherwise one last healthy point.
func (f *faultRun) finish(ctx context.Context, actx *tr.ActionContext, node string) error {
	f.next = len(f.events) // kills scheduled after the last operation do not run
	if f.down != "" {
		return f.run(ctx, actx, node, FaultEvent{Action: FaultRestart, Process: f.down})
	}
	return f.healthy(ctx, actx, node)
}

func (f *faultRun) run(ctx context.Context, actx *tr.ActionContext, node string, e FaultEvent) error {
	if _, err := genExec(ctx, actx, node, GenOp{Action: "v3_op_" + e.Action, Params: map[string]string{"process": e.Process}}); err != nil {
		return fmt.Errorf("%s %s after operation %d: %w", e.Action, e.Process, e.AfterOp, err)
	}
	if f.gen != nil {
		f.gen.Fault(e.Action, e.Process)
	}
	if e.Action == FaultKill {
		f.down = e.Process
		return nil
	}
	f.down = ""
	return f.healthy(ctx, actx, node)
}

// healthy asks for a healthy point and, when it is one, resolves.
func (f *faultRun) healthy(ctx context.Context, actx *tr.ActionContext, node string) error {
	h, ok := genHandlers["v3_op_healthy"]
	if !ok {
		return fmt.Errorf("no handler for v3_op_healthy")
	}
	p := map[string]string{}
	if f.wait != "" {
		p["wait_seconds"] = f.wait
	}
	out, err := h(ctx, actx, tr.Action{Action: "v3_op_healthy", Node: node, Params: p})
	if err != nil {
		return fmt.Errorf("v3_op_healthy: %w", err)
	}
	if out["healthy"] != "true" {
		return nil // not a healthy point: unknowns stay pending
	}
	return f.resolve(ctx, actx, node)
}

// resolve observes every pending unknown, in the order they happened.
func (f *faultRun) resolve(ctx context.Context, actx *tr.ActionContext, node string) error {
	var left []GenOp
	observed := map[string]bool{}
	writes := map[string]bool{} // volumes with a write still pending
	observe := func(vol string) error {
		if vol == "" || observed[vol] {
			return nil
		}
		observed[vol] = true
		_, err := genExec(ctx, actx, node, GenOp{Action: "v3_op_observe", Volume: vol, Params: map[string]string{"volume": vol}})
		return err
	}
	for _, o := range f.pending {
		if o.Action == "v3_op_write" {
			got, err := genExec(ctx, actx, node, GenOp{Action: "v3_op_read", Volume: o.Volume, Params: map[string]string{"volume": o.Volume, "lbas": o.Params["lbas"]}})
			if err != nil {
				return fmt.Errorf("resolving operation %d: %w", o.No, err)
			}
			if got != tr.OutcomeOK {
				left = append(left, o)
				writes[o.Volume] = true
			}
			continue
		}
		if err := observe(o.Volume); err != nil {
			return fmt.Errorf("resolving operation %d: %w", o.No, err)
		}
		if err := observe(o.Params["target_volume"]); err != nil {
			return fmt.Errorf("resolving operation %d: %w", o.No, err)
		}
	}
	if f.gen != nil {
		for _, o := range f.pending {
			if o.Action == "v3_op_write" && !writes[o.Volume] {
				f.gen.Resolved(o.Volume)
			}
		}
	}
	f.pending = left
	return nil
}
