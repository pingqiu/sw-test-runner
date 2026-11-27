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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
// a rate above 0, or off; fault_processes; fault_restart_within;
// fault_master_outage: the rate of short master outages, or off).
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
	if s := get("fault_master_outage"); s != "" && s != "off" {
		if c.MasterOutage, err = strconv.ParseFloat(s, 64); err != nil {
			return c, fmt.Errorf("%s: fault_master_outage: %w", name, err)
		}
	}
	if s := get("faults"); s != "" && s != "off" {
		if c.Faults, err = strconv.ParseFloat(s, 64); err != nil {
			return c, fmt.Errorf("%s: faults: %w", name, err)
		}
	}
	if c.Faults != 0 || c.MasterOutage != 0 {
		c.FaultProcesses = strings.Fields(get("fault_processes"))
		if s := get("fault_restart_within"); s != "" {
			if c.RestartWithin, err = strconv.Atoi(s); err != nil || c.RestartWithin < 1 {
				return c, fmt.Errorf("%s: fault_restart_within %q", name, s)
			}
		}
	}
	return c, nil
}

// checkerSet is the form of a run's required checker set: names separated by
// commas, as the checkers' own --checkers takes them.
var checkerSet = regexp.MustCompile(`^[A-Za-z0-9_]+(,[A-Za-z0-9_]+)*$`)

// Run drives generator g to its end. get reads the run's own parameters
// beyond the config: nightly_seed (the nightly seed the run's seed was drawn
// from), fault_healthy_wait (the seconds the healthy action polls) and
// checkers (the scenario's required checker set, the same list its judge
// passes to --checkers; recorded in the header for the nightly judge).
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
	if cs := get("checkers"); cs != "" {
		if !checkerSet.MatchString(cs) {
			return nil, fmt.Errorf("%s: checkers %q is not a comma-separated list of checker names", r.Name, cs)
		}
		header["checkers"] = cs
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
		fr = newFaultRun(r, s.Events, g, header, nil)
		fr.stopOnUnhealthy = true
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
			if err := fr.afterOp(ctx, actx, o.No); errors.Is(err, errUnhealthy) {
				// No more operations against dead subjects: record it, then
				// finish, sweep and end.
				if err := stopRun(actx, o.No); err != nil {
					return nil, err
				}
				g.stop("unhealthy")
				break
			} else if err != nil {
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
	if err := actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: "gen_sweep", Result: map[string]any{"ops": opList(ops)}}); err != nil {
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

// opList is a record's list of operations: action, volume, params.
func opList(ops []Op) []any {
	list := make([]any, 0, len(ops))
	for _, o := range ops {
		p := map[string]any{}
		for k, v := range o.Params {
			p[k] = v
		}
		list = append(list, map[string]any{"action": o.Action, "volume": o.Volume, "params": p})
	}
	return list
}

// parseOps reads a record's list of operations (opList's form) under key.
func parseOps(res map[string]any, key string) ([]Op, error) {
	raw, ok := res[key].([]any)
	if !ok {
		return nil, fmt.Errorf("%s %v", key, res[key])
	}
	out := make([]Op, 0, len(raw))
	for i, x := range raw {
		m, ok := x.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: op %d is %T", key, i, x)
		}
		a, _ := m["action"].(string)
		v, _ := m["volume"].(string)
		ps, ok := m["params"].(map[string]any)
		if a == "" || !ok {
			return nil, fmt.Errorf("%s: op %d %v", key, i, m)
		}
		o := Op{Action: a, Volume: v, Params: map[string]string{}}
		for k, pv := range ps {
			s, ok := pv.(string)
			if !ok {
				return nil, fmt.Errorf("%s: op %d param %s is %T", key, i, k, pv)
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
//   - before each kill, one single look (Driver.Look): "one down at a time"
//     is checked against what is observed, not only against what this run
//     killed. When the look shows anything down the kill is skipped, and so
//     is its restart. An observe record "fault_kill" {after_op, process,
//     down, skipped} records the decision; a replay follows the recorded one;
//   - after each restart, the Driver's healthy operation records a `healthy`
//     observation; at a healthy point (it answers healthy=true) every
//     operation whose outcome was unknown since the last healthy point is
//     RESOLVED as the Driver's Resolution says: by its check, or by
//     observing its subjects. A check that does not answer ok leaves the
//     operation pending for the next healthy point;
//   - a short master outage after operation k (see outage) runs as one
//     event, nothing else down meanwhile;
//   - in a run and its replay, a healthy point still false after its full
//     wait ends the run: an observe record "gen_stop" {after_op, why}, then
//     finish, the sweep and the end (no more operations against dead
//     subjects; Ended "unhealthy");
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
	gen     *Generator   // nil in a replay
	wait    string       // the healthy operation's wait ("" for its default: look once)
	outages map[int][]Op // a replay's recorded outage writes, by after_op
	// kills are a replay's recorded kill decisions, in order (nil: decide
	// from the look); skip is the process whose kill was skipped, so its
	// restart is skipped too.
	kills []KillDecision
	skip  string
	// stopOnUnhealthy: a healthy point still false after its full wait ends
	// the run (errUnhealthy) instead of carrying on against dead subjects.
	stopOnUnhealthy bool
}

// errUnhealthy is a healthy point still false after its full wait.
var errUnhealthy = errors.New("a healthy point still false after its full wait")

// stopRun records that the run stops proposing after operation opno because
// a healthy point stayed false, and why.
func stopRun(actx *tr.ActionContext, opno int) error {
	return actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: "gen_stop",
		Result: map[string]any{"after_op": opno, "why": errUnhealthy.Error()}})
}

// KillDecision is one recorded "fault_kill" record: whether the kill of
// process after operation after_op ran.
type KillDecision struct {
	AfterOp int
	Process string
	Skipped bool
}

// killDecision takes the single look before a kill and decides: a kill only
// while the look shows nothing down. It records the decision.
func (f *faultRun) killDecision(ctx context.Context, actx *tr.ActionContext, e FaultEvent) (bool, error) {
	o := f.r.Driver.Look()
	h, ok := f.r.Handlers[o.Action]
	if !ok {
		return false, fmt.Errorf("no handler for %s", o.Action)
	}
	out, err := h(ctx, actx, tr.Action{Action: o.Action, Node: f.r.Node, Params: o.Params})
	if err != nil {
		return false, fmt.Errorf("look before kill %s after operation %d: %w", e.Process, e.AfterOp, err)
	}
	down := strings.Fields(out[DownKey])
	skipped := len(down) > 0
	if f.kills != nil {
		if len(f.kills) == 0 || f.kills[0].AfterOp != e.AfterOp || f.kills[0].Process != e.Process {
			return false, fmt.Errorf("kill %s after operation %d: no recorded fault_kill for it", e.Process, e.AfterOp)
		}
		skipped, f.kills = f.kills[0].Skipped, f.kills[1:]
	}
	if down == nil {
		down = []string{}
	}
	err = actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: "fault_kill",
		Result: map[string]any{"after_op": e.AfterOp, "process": e.Process, "down": down, "skipped": skipped}})
	return skipped, err
}

// newFaultRun takes the header's fault_healthy_wait (absent: "").
func newFaultRun(r Runner, events []FaultEvent, g *Generator, header map[string]any, outages map[int][]Op) *faultRun {
	w, _ := header["fault_healthy_wait"].(string)
	return &faultRun{r: r, events: events, gen: g, wait: w, outages: outages}
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
	var err error
	if f.down != "" {
		err = f.run(ctx, actx, FaultEvent{Action: FaultRestart, Process: f.down})
	} else {
		err = f.healthy(ctx, actx)
	}
	if errors.Is(err, errUnhealthy) {
		return nil // the run ends here anyway; the record says it
	}
	return err
}

func (f *faultRun) run(ctx context.Context, actx *tr.ActionContext, e FaultEvent) error {
	if e.Action == FaultMasterOutage {
		return f.outage(ctx, actx, e)
	}
	if e.Action == FaultKill && e.AfterOp > 0 { // a scheduled kill (finish restarts with after_op 0)
		skipped, err := f.killDecision(ctx, actx, e)
		if err != nil {
			return err
		}
		if skipped {
			f.skip = e.Process
			return nil
		}
	}
	if e.Action == FaultRestart && f.skip != "" && f.skip == e.Process {
		f.skip = ""
		return nil
	}
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

// outage runs one short master outage after operation e.AfterOp, in this
// order (the shapes checker MO reads):
//   - an observe record "master_outage": after_op, process, max_seconds and
//     the outage's writes (a replay re-runs the same writes from it);
//   - a healthy point (a normal healthy record: the holders' identities
//     before the kill), resolving as any healthy point does;
//   - the kill of the master; a single look; one write on every serving
//     subject (learnt, an unknown pending like any other); a single look;
//   - the master's restart, bounded by the outage bound, then a healthy point.
//
// Nothing waits on the master in between, so the window from the kill's
// completion to the restart's is the writes and the looks. Whether it was
// short enough, and what the product answered, MO judges.
func (f *faultRun) outage(ctx context.Context, actx *tr.ActionContext, e FaultEvent) error {
	d := f.r.Driver
	master, maxSec, ok := d.MasterOutage()
	if !ok || e.Process != master {
		return fmt.Errorf("master outage of %s after operation %d: the product's master is %q", e.Process, e.AfterOp, master)
	}
	writes := f.outages[e.AfterOp]
	if f.gen != nil {
		writes = f.gen.m.OutageWrites(e.AfterOp)
	}
	if err := actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: "master_outage",
		Result: map[string]any{"after_op": e.AfterOp, "process": master, "max_seconds": maxSec, "writes": opList(writes)}}); err != nil {
		return err
	}
	if err := f.healthy(ctx, actx); err != nil {
		return err
	}
	look := func() error {
		o := d.Look()
		h, ok := f.r.Handlers[o.Action]
		if !ok {
			return fmt.Errorf("no handler for %s", o.Action)
		}
		_, err := h(ctx, actx, tr.Action{Action: o.Action, Node: f.r.Node, Params: o.Params})
		return err
	}
	if _, err := f.r.Exec(ctx, actx, d.Fault(FaultEvent{AfterOp: e.AfterOp, Action: FaultKill, Process: master})); err != nil {
		return fmt.Errorf("master outage after operation %d: kill: %w", e.AfterOp, err)
	}
	if err := look(); err != nil {
		return err
	}
	for _, w := range writes {
		got, err := f.r.Exec(ctx, actx, w)
		if err != nil {
			return fmt.Errorf("master outage after operation %d: %s %s: %w", e.AfterOp, w.Action, w.Volume, err)
		}
		if f.gen != nil {
			f.gen.Learn(w, got)
		}
		f.note(w, got)
	}
	if err := look(); err != nil {
		return err
	}
	if _, err := f.r.Exec(ctx, actx, d.OutageRestart()); err != nil {
		return fmt.Errorf("master outage after operation %d: restart: %w", e.AfterOp, err)
	}
	if f.gen != nil {
		f.gen.Fault(FaultMasterOutage, master)
	}
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
	if out[HealthyKey] != HealthyTrue {
		if f.stopOnUnhealthy {
			return errUnhealthy
		}
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
