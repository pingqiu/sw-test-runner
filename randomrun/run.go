package randomrun

// The random run: the generator proposes, the product's typed action
// handlers execute and record, and the outcome is fed back. The history gets,
// besides the operations' own records:
//   - first, an observe record "gen_header": generator version, seed, ops,
//     pool, negative share, weights and the model's own configuration (a
//     replay must match them), and the nightly seed the seed was drawn from
//     when param nightly_seed is set;
//   - with a fault schedule, right after the header an observe record
//     "fault_schedule", and the schedule's kills, restarts, healthy points and
//     resolving observations between the operations (faultRun below); the
//     header then names the model's fault-run version;
//   - before each operation, an observe record "gen_step": its number,
//     action, params and the limit a negative probes. That is the replay
//     script, and it ties every operation record to its proposal;
//   - two observations of the subject before every negative operation (the
//     first pair is the baseline: equal, with nothing between them) and one
//     after every negative operation and every refusal, so "a refused
//     operation changed nothing" is judged from the observations around it;
//   - after the last operation (in a fault run, after the final healthy
//     point), the end-of-run sweep: an observe record "gen_sweep" listing the
//     model's sweep operations, then each through Exec (reads of every block
//     with acknowledged content, and the last observations the checkers need);
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

// Runner executes proposals through a product's handlers.
type Runner struct {
	Name     string // the registered action, in messages
	Node     string
	Driver   Driver
	Handlers map[string]tr.ActionHandlerFunc
}

// outcomeRank orders outcomes for aggregation: the worst wins.
var outcomeRank = map[string]int{tr.OutcomeOK: 0, tr.OutcomeRefused: 1, tr.OutcomeUnknown: 2}

// worseOutcome returns the worse of two outcomes; "" (none yet) loses to any.
func worseOutcome(a, b string) string {
	if a == "" || outcomeRank[b] > outcomeRank[a] {
		return b
	}
	return a
}

// Exec runs one proposal through its handler, one call per part the Driver
// splits it into, so that every part's outcome counts: the worst is
// returned. An observation answers no outcome ("").
func (r Runner) Exec(ctx context.Context, actx *tr.ActionContext, o Op) (string, error) {
	h, ok := r.Handlers[o.Action]
	if !ok {
		return "", fmt.Errorf("%s: no handler for %s", r.Name, o.Action)
	}
	observation := o.Action == r.Driver.Observe(o.Volume).Action
	worst := ""
	for _, p := range r.Driver.Calls(o) {
		out, err := h(ctx, actx, tr.Action{Action: o.Action, Node: r.Node, Params: p})
		if err != nil {
			return "", err
		}
		if observation {
			continue // an observation has no outcome (its handler answers "observed")
		}
		got := out["value"]
		if _, known := outcomeRank[got]; !known {
			return "", fmt.Errorf("%s: %s returned outcome %q", r.Name, o.Action, got)
		}
		if worst == "" || outcomeRank[got] > outcomeRank[worst] {
			worst = got
		}
	}
	return worst, nil
}

// Step records one proposal and executes it: the gen_step record, two
// observations of the subject before a negative operation, the operation
// through Exec, and an observation after a refusal or a negative. A run and
// a replay both run every step through here, so a replayed history has the
// same shape as a recorded one.
func (r Runner) Step(ctx context.Context, actx *tr.ActionContext, o Op) (string, error) {
	observe := func() error {
		obs := r.Driver.Observe(o.Volume)
		h, ok := r.Handlers[obs.Action]
		if !ok {
			return fmt.Errorf("%s: no handler for %s", r.Name, obs.Action)
		}
		_, err := h(ctx, actx, tr.Action{Action: obs.Action, Node: r.Node, Params: obs.Params})
		return err
	}
	step := map[string]any{"opno": o.No, "action": o.Action, "params": o.Params}
	if o.Negative != "" {
		step["negative"] = o.Negative
	}
	if err := actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: "gen_step", Volume: o.Volume, Result: step}); err != nil {
		return "", err
	}
	// Two observations before a negative operation: with nothing between
	// them they must be equal (the product's background work shows there,
	// not at the refusal), and the second is the refusal's "before".
	if o.Negative != "" {
		for i := 0; i < 2; i++ {
			if err := observe(); err != nil {
				return "", err
			}
		}
	}
	outcome, err := r.Exec(ctx, actx, o)
	if err != nil {
		return "", err
	}
	if outcome == tr.OutcomeRefused || o.Negative != "" {
		if err := observe(); err != nil {
			return "", err
		}
	}
	return outcome, nil
}

// ParseConfig reads a run's generic parameters through get: seed, ops, pool,
// negative, weights ("action=integer ..."), and the fault schedule (faults:
// a rate above 0, or off; fault_processes; fault_restart_within).
func ParseConfig(name string, get func(string) string) (Config, error) {
	var c Config
	var err error
	if c.Seed, err = strconv.ParseUint(get("seed"), 10, 64); err != nil {
		return c, fmt.Errorf("%s: seed: %w", name, err)
	}
	if c.Ops, err = strconv.Atoi(get("ops")); err != nil {
		return c, fmt.Errorf("%s: ops: %w", name, err)
	}
	c.Pool = strings.Fields(get("pool"))
	if s := get("negative"); s != "" {
		if c.Negative, err = strconv.ParseFloat(s, 64); err != nil {
			return c, fmt.Errorf("%s: negative: %w", name, err)
		}
	}
	if s := get("weights"); s != "" {
		c.Weights = map[string]int{}
		for _, kv := range strings.Fields(s) {
			k, v, ok := strings.Cut(kv, "=")
			n, err := strconv.Atoi(v)
			if !ok || err != nil {
				return c, fmt.Errorf("%s: weight %q is not action=integer", name, kv)
			}
			c.Weights[k] = n
		}
	}
	if s := get("faults"); s != "" && s != "off" {
		if c.Faults, err = strconv.ParseFloat(s, 64); err != nil {
			return c, fmt.Errorf("%s: faults: %w", name, err)
		}
		c.FaultProcesses = strings.Fields(get("fault_processes"))
		if s := get("fault_restart_within"); s != "" {
			if c.RestartWithin, err = strconv.Atoi(s); err != nil || c.RestartWithin < 1 {
				return c, fmt.Errorf("%s: fault_restart_within %q", name, s)
			}
		}
	}
	return c, nil
}

// Run drives generator g to its end. get reads the run's own parameters
// beyond the config: nightly_seed (the nightly seed the run's seed was drawn
// from) and fault_healthy_wait (the seconds the healthy action polls).
func (r Runner) Run(ctx context.Context, actx *tr.ActionContext, g *Generator, get func(string) string) (map[string]string, error) {
	if actx.History == nil {
		return nil, fmt.Errorf("%s: no history (typed operations run only with a run bundle)", r.Name)
	}
	header := g.Header()
	if ns := get("nightly_seed"); ns != "" {
		if _, err := strconv.ParseUint(ns, 10, 64); err != nil {
			return nil, fmt.Errorf("%s: nightly_seed: %w", r.Name, err)
		}
		header["nightly_seed"] = ns
	}
	// In the header, so a replay waits the same.
	if w := get("fault_healthy_wait"); w != "" && g.Faulty() {
		if n, err := strconv.Atoi(w); err != nil || n < 0 {
			return nil, fmt.Errorf("%s: fault_healthy_wait %q", r.Name, w)
		}
		header["fault_healthy_wait"] = w
	}
	if err := actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: "gen_header", Result: header}); err != nil {
		return nil, err
	}
	var fr *faultRun
	if g.Faulty() {
		s := g.Schedule()
		if err := actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: "fault_schedule", Result: s.Record()}); err != nil {
			return nil, err
		}
		fr = newFaultRun(r, s.Events, g, header)
	}
	for {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%s: %w after %d operations", r.Name, ctx.Err(), g.no)
		}
		o, ok := g.Next()
		if !ok {
			break
		}
		outcome, err := r.Step(ctx, actx, o)
		if err != nil {
			return nil, fmt.Errorf("%s: operation %d (%s %s): %w", r.Name, o.No, o.Action, o.Volume, err)
		}
		g.Learn(o, outcome)
		if fr != nil {
			fr.note(o, outcome)
			if err := fr.afterOp(ctx, actx, o.No); err != nil {
				return nil, fmt.Errorf("%s: %w", r.Name, err)
			}
		}
	}
	if fr != nil {
		if err := fr.finish(ctx, actx); err != nil {
			return nil, fmt.Errorf("%s: end of run: %w", r.Name, err)
		}
	}
	if err := r.sweep(ctx, actx, g.m.Sweep()); err != nil {
		return nil, fmt.Errorf("%s: end-of-run sweep: %w", r.Name, err)
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
	actx.Log("  %s: %d operations, ended %s", r.Name, g.no, g.Ended())
	return map[string]string{"value": g.Ended(), "proposed": strconv.Itoa(g.no)}, nil
}

// sweep records the gen_sweep marker with the sweep's operations (so a
// replay re-executes the same sweep from the history alone) and runs each
// through Exec. The outcomes are facts for the checkers; nothing is judged.
func (r Runner) sweep(ctx context.Context, actx *tr.ActionContext, ops []Op) error {
	list := make([]any, 0, len(ops))
	for _, o := range ops {
		p := map[string]any{}
		for k, v := range o.Params {
			p[k] = v
		}
		list = append(list, map[string]any{"action": o.Action, "volume": o.Volume, "params": p})
	}
	if err := actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: "gen_sweep", Result: map[string]any{"ops": list}}); err != nil {
		return err
	}
	for _, o := range ops {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := r.Exec(ctx, actx, o); err != nil {
			return fmt.Errorf("%s %s: %w", o.Action, o.Volume, err)
		}
	}
	return nil
}

// parseSweep reads the operations of a recorded gen_sweep result.
func parseSweep(res map[string]any) ([]Op, error) {
	raw, ok := res["ops"].([]any)
	if !ok {
		return nil, fmt.Errorf("gen_sweep: ops %v", res["ops"])
	}
	out := make([]Op, 0, len(raw))
	for i, x := range raw {
		m, ok := x.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("gen_sweep: op %d is %T", i, x)
		}
		a, _ := m["action"].(string)
		v, _ := m["volume"].(string)
		ps, ok := m["params"].(map[string]any)
		if a == "" || !ok {
			return nil, fmt.Errorf("gen_sweep: op %d %v", i, m)
		}
		o := Op{Action: a, Volume: v, Params: map[string]string{}}
		for k, pv := range ps {
			s, ok := pv.(string)
			if !ok {
				return nil, fmt.Errorf("gen_sweep: op %d param %s is %T", i, k, pv)
			}
			o.Params[k] = s
		}
		out = append(out, o)
	}
	return out, nil
}

// faultRun executes a fault schedule inside a run and a replay (both run it
// through here, so a replayed history has the same shape):
//   - after operation k completes (and is learnt), every event with
//     after_op k runs through the Driver's fault operation;
//   - after each restart, the Driver's healthy operation records a `healthy`
//     observation; at a healthy point (it answers healthy=true) every
//     operation whose outcome was unknown since the last healthy point is
//     RESOLVED as the Driver's Resolution says: by its check, or by
//     observing its subjects. A check that does not answer ok leaves the
//     operation pending for the next healthy point;
//   - at the end of the run a process still down is restarted (its scheduled
//     restart lay after the last operation), and the run ends on a healthy
//     point with its resolution, so no unknown is left unobserved.
//
// No time decides anything here, and nothing is judged: the observations are
// facts for the checkers.
type faultRun struct {
	r       Runner
	events  []FaultEvent
	next    int    // the first event not yet run
	down    string // the process down now
	pending []Op
	gen     *Generator // nil in a replay
	wait    string     // the healthy operation's wait ("" for its default: look once)
}

// newFaultRun takes the header's fault_healthy_wait (absent: "").
func newFaultRun(r Runner, events []FaultEvent, g *Generator, header map[string]any) *faultRun {
	w, _ := header["fault_healthy_wait"].(string)
	return &faultRun{r: r, events: events, gen: g, wait: w}
}

// note takes an operation's outcome: an unknown is pending resolution,
// unless the Driver says it has nothing to resolve.
func (f *faultRun) note(o Op, outcome string) {
	if outcome != tr.OutcomeUnknown {
		return
	}
	if check, obs := f.r.Driver.Resolution(o); check != nil || len(obs) > 0 {
		f.pending = append(f.pending, o)
	}
}

// afterOp runs the events scheduled after operation opno.
func (f *faultRun) afterOp(ctx context.Context, actx *tr.ActionContext, opno int) error {
	for f.next < len(f.events) && f.events[f.next].AfterOp <= opno {
		if err := f.run(ctx, actx, f.events[f.next]); err != nil {
			return err
		}
		f.next++
	}
	return nil
}

// finish ends the run: a process still down is restarted (with its healthy
// point); otherwise one last healthy point.
func (f *faultRun) finish(ctx context.Context, actx *tr.ActionContext) error {
	f.next = len(f.events) // kills scheduled after the last operation do not run
	if f.down != "" {
		return f.run(ctx, actx, FaultEvent{Action: FaultRestart, Process: f.down})
	}
	return f.healthy(ctx, actx)
}

func (f *faultRun) run(ctx context.Context, actx *tr.ActionContext, e FaultEvent) error {
	if _, err := f.r.Exec(ctx, actx, f.r.Driver.Fault(e)); err != nil {
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
	return f.healthy(ctx, actx)
}

// healthy asks for a healthy point and, when it is one, resolves.
func (f *faultRun) healthy(ctx context.Context, actx *tr.ActionContext) error {
	o := f.r.Driver.Healthy(f.wait)
	h, ok := f.r.Handlers[o.Action]
	if !ok {
		return fmt.Errorf("no handler for %s", o.Action)
	}
	out, err := h(ctx, actx, tr.Action{Action: o.Action, Node: f.r.Node, Params: o.Params})
	if err != nil {
		return fmt.Errorf("%s: %w", o.Action, err)
	}
	if out["healthy"] != "true" {
		return nil // not a healthy point: unknowns stay pending
	}
	return f.resolve(ctx, actx)
}

// resolve resolves every pending unknown, in the order they happened.
func (f *faultRun) resolve(ctx context.Context, actx *tr.ActionContext) error {
	var left []Op
	observed := map[string]bool{}
	failed := map[string]bool{} // subjects with a check still pending
	observe := func(subject string) error {
		if subject == "" || observed[subject] {
			return nil
		}
		observed[subject] = true
		_, err := f.r.Exec(ctx, actx, f.r.Driver.Observe(subject))
		return err
	}
	for _, o := range f.pending {
		check, obs := f.r.Driver.Resolution(o)
		if check != nil {
			got, err := f.r.Exec(ctx, actx, *check)
			if err != nil {
				return fmt.Errorf("resolving operation %d: %w", o.No, err)
			}
			if got != tr.OutcomeOK {
				left = append(left, o)
				failed[o.Volume] = true
			}
			continue
		}
		for _, s := range obs {
			if err := observe(s); err != nil {
				return fmt.Errorf("resolving operation %d: %w", o.No, err)
			}
		}
	}
	if f.gen != nil {
		for _, o := range f.pending {
			if check, _ := f.r.Driver.Resolution(o); check != nil && !failed[o.Volume] {
				f.gen.Resolved(o.Volume)
			}
		}
	}
	f.pending = left
	return nil
}
