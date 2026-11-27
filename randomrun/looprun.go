package randomrun

// The loop run: a declared loop (loop.go) executed through a product's
// typed action handlers, each operation through Runner.Step (the same
// gen_step record and handler path as the random run). The history gets:
//   - first, an observe record "loop_header": driver loop/1, the loop's name,
//     the seed, the declaration's hash and the declaration itself, the pool,
//     blocks, iterations and duration, and with faults the schedule's
//     parameters and fault_ops (the cycle operations the schedule spans);
//   - with a fault schedule, right after the header an observe record
//     "fault_schedule" (NewFaultSchedule over fault_ops, the same derivation
//     as the random run). Its after_op counts CYCLE operations: faults run
//     only between cycle operations, with the random run's kills, restarts,
//     healthy points and resolutions (faultRun); at the end of the cycle a
//     process still down is restarted and the cycle ends on a healthy point;
//   - an observe record "loop_phase" {phase} before each phase: setup, cycle,
//     sweep, teardown, always all four in that order;
//   - before each operation its "gen_step" record (Runner.Step);
//   - last, an observe record "loop_coverage" (also loop-coverage.json in the
//     bundle): operations per action and per phase, the iterations done, why
//     the cycle ended, and the declared actions never run.
// The sweep reads back every written lba of each subject and then observes
// every subject; it runs after the cycle's last healthy point and before the
// teardown. It stops on a runner error (a handler error), never on a product
// answer. CheckLoop judges a loop history's shape against its declaration.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

// LoopConfig sets a loop run.
type LoopConfig struct {
	Name       string // the loop's name in its file
	Loop       Loop
	Seed       uint64
	Pool       []string
	Blocks     int           // the lba range 0..Blocks-1 of {lbas}
	Iterations int           // the cycle's iterations (the cap when Duration is set); 0: none
	Duration   time.Duration // the cycle stops at the first iteration boundary past it; 0: none

	Faults         float64
	FaultProcesses []string
	RestartWithin  int
	HealthyWait    string // the healthy operation's wait ("" for its default)
}

// ParseLoopConfig reads a loop run's parameters through get: seed, pool,
// blocks, iterations, duration, faults, fault_processes,
// fault_restart_within, fault_healthy_wait. The loop itself is the caller's.
func ParseLoopConfig(name string, get func(string) string) (LoopConfig, error) {
	var c LoopConfig
	var err error
	if c.Seed, err = strconv.ParseUint(get("seed"), 10, 64); err != nil {
		return c, fmt.Errorf("%s: seed: %w", name, err)
	}
	c.Pool = strings.Fields(get("pool"))
	if s := get("blocks"); s != "" {
		if c.Blocks, err = strconv.Atoi(s); err != nil || c.Blocks < 1 {
			return c, fmt.Errorf("%s: blocks %q", name, s)
		}
	}
	if s := get("iterations"); s != "" {
		if c.Iterations, err = strconv.Atoi(s); err != nil || c.Iterations < 1 {
			return c, fmt.Errorf("%s: iterations %q", name, s)
		}
	}
	if s := get("duration"); s != "" {
		if c.Duration, err = time.ParseDuration(s); err != nil || c.Duration <= 0 {
			return c, fmt.Errorf("%s: duration %q", name, s)
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
		if w := get("fault_healthy_wait"); w != "" {
			if n, err := strconv.Atoi(w); err != nil || n < 0 {
				return c, fmt.Errorf("%s: fault_healthy_wait %q", name, w)
			}
			c.HealthyWait = w
		}
	}
	return c, nil
}

// LoopRunner runs declared loops through a Runner's handlers.
type LoopRunner struct {
	Runner
	Now func() time.Time // the clock of a duration (nil: time.Now)

	// mutant seeds a defect for the red controls (tests only): "skip-sweep"
	// runs no sweep operation, "swap-cycle" swaps each iteration's first two
	// operations.
	mutant string
}

// faultOps is the number of cycle operations the fault schedule spans.
func (c LoopConfig) faultOps() int { return c.Loop.cycleOps(len(c.Pool), c.Iterations) }

// header is the loop_header record's result.
func (c LoopConfig) header() map[string]any {
	h := map[string]any{"driver": LoopDriverVersion, "loop": c.Name, "seed": strconv.FormatUint(c.Seed, 10),
		"declaration_sha256": c.Loop.Hash(), "declaration": c.Loop.record(), "pool": slices.Clone(c.Pool),
		"blocks": c.Blocks, "iterations": c.Iterations}
	if c.Duration > 0 {
		h["duration"] = c.Duration.String()
	}
	if c.Faults != 0 {
		h["faults"] = c.Faults
		h["fault_processes"] = slices.Clone(c.FaultProcesses)
		h["fault_restart_within"] = c.RestartWithin
		h["fault_ops"] = c.faultOps()
		if c.HealthyWait != "" {
			h["fault_healthy_wait"] = c.HealthyWait
		}
	}
	return h
}

// validate completes the defaults and refuses a config that cannot run.
func (c *LoopConfig) validate(valid func(string) bool) (FaultSchedule, error) {
	if c.Iterations == 0 && c.Duration == 0 {
		return FaultSchedule{}, fmt.Errorf("iterations or a duration is required")
	}
	if c.Faults != 0 {
		if c.Iterations == 0 {
			return FaultSchedule{}, fmt.Errorf("a fault schedule spans the cycle's operations: iterations (the cap of a duration) is required")
		}
		if c.RestartWithin == 0 {
			c.RestartWithin = DefaultRestartWithin
		}
		c.FaultProcesses = slices.Clone(c.FaultProcesses)
		slices.Sort(c.FaultProcesses)
		return NewFaultSchedule(c.Seed, c.faultOps(), c.FaultProcesses, c.Faults, c.RestartWithin, valid)
	}
	return FaultSchedule{Seed: c.Seed, Events: []FaultEvent{}}, nil
}

func (r LoopRunner) observe(actx *tr.ActionContext, verb string, res map[string]any) error {
	return actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "loop", Verb: verb, Result: res})
}

// RunLoop runs one declared loop to its end.
func (r LoopRunner) RunLoop(ctx context.Context, actx *tr.ActionContext, c LoopConfig) (map[string]string, error) {
	if actx.History == nil {
		return nil, fmt.Errorf("%s: no history (typed operations run only with a run bundle)", r.Name)
	}
	known := func(a string) bool { _, ok := r.Handlers[a]; return ok }
	if err := c.Loop.Check(known); err != nil {
		return nil, fmt.Errorf("%s: loop %s: %w", r.Name, c.Name, err)
	}
	sched, err := c.validate(r.Driver.ValidProcess)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", r.Name, err)
	}
	plan, err := newLoopPlan(c.Loop, c.Pool, c.Blocks, c.Seed)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", r.Name, err)
	}
	now := r.Now
	if now == nil {
		now = time.Now
	}
	start := now()
	header := c.header()
	// The product's own header entries (a loop's settings are its config:
	// the product's header function reads no parameter here).
	if r.Header != nil {
		extra, err := r.Header(func(string) string { return "" })
		if err != nil {
			return nil, fmt.Errorf("%s: %w", r.Name, err)
		}
		for k, v := range extra {
			if _, taken := header[k]; taken {
				return nil, fmt.Errorf("%s: the product's header field %q is the loop's", r.Name, k)
			}
			header[k] = v
		}
	}
	if err := r.observe(actx, "loop_header", header); err != nil {
		return nil, err
	}
	var fr *faultRun
	if c.Faults != 0 {
		if err := r.observe(actx, "fault_schedule", sched.Record()); err != nil {
			return nil, err
		}
		fr = newFaultRun(r.Runner, sched.Events, nil, header, nil) // a loop schedules no master outages
	}
	no := 0
	actions, phases := map[string]int{}, map[string]int{}
	step := func(phase string, o Op) (string, error) {
		if ctx.Err() != nil {
			return "", fmt.Errorf("%s: %w after %d operations", r.Name, ctx.Err(), no)
		}
		no++
		o.No = no
		out, err := r.Step(ctx, actx, o)
		if err != nil {
			return "", fmt.Errorf("%s: %s operation %d (%s %s): %w", r.Name, phase, o.No, o.Action, o.Volume, err)
		}
		actions[o.Action]++
		phases[phase]++
		return out, nil
	}

	if err := r.observe(actx, "loop_phase", map[string]any{"phase": PhaseSetup}); err != nil {
		return nil, err
	}
	for _, o := range plan.once(c.Loop.Setup) {
		if _, err := step(PhaseSetup, o); err != nil {
			return nil, err
		}
	}

	if err := r.observe(actx, "loop_phase", map[string]any{"phase": PhaseCycle}); err != nil {
		return nil, err
	}
	written := map[string]map[int]bool{}
	cycleNo, iter, ended := 0, 0, ""
	for {
		if c.Iterations > 0 && iter >= c.Iterations {
			ended = "iterations"
			break
		}
		if c.Duration > 0 && now().Sub(start) >= c.Duration {
			ended = "duration"
			break
		}
		iter++
		ops := plan.iteration(iter)
		if r.mutant == "swap-cycle" && len(ops) > 1 {
			ops[0], ops[1] = ops[1], ops[0]
		}
		for _, pl := range ops {
			out, err := step(PhaseCycle, pl.Op)
			if err != nil {
				return nil, err
			}
			cycleNo++
			if pl.Writes != nil && out != tr.OutcomeRefused {
				addWritten(written, pl.Op.Volume, pl.Writes)
			}
			if fr != nil {
				fr.note(pl.Op, out)
				if err := fr.afterOp(ctx, actx, cycleNo); err != nil {
					return nil, fmt.Errorf("%s: %w", r.Name, err)
				}
			}
		}
	}
	if fr != nil {
		if err := fr.finish(ctx, actx); err != nil {
			return nil, fmt.Errorf("%s: end of cycle: %w", r.Name, err)
		}
	}

	if err := r.observe(actx, "loop_phase", map[string]any{"phase": PhaseSweep}); err != nil {
		return nil, err
	}
	if r.mutant != "skip-sweep" {
		for _, o := range plan.sweep(written, r.Driver.Observe) {
			if _, err := step(PhaseSweep, o); err != nil {
				return nil, err
			}
		}
	}

	if err := r.observe(actx, "loop_phase", map[string]any{"phase": PhaseTeardown}); err != nil {
		return nil, err
	}
	for _, o := range plan.once(c.Loop.Teardown) {
		if _, err := step(PhaseTeardown, o); err != nil {
			return nil, err
		}
	}

	cov := loopCoverage(c.Loop, actions, phases, iter, ended, no)
	if err := r.observe(actx, "loop_coverage", cov); err != nil {
		return nil, err
	}
	if actx.Bundle != nil {
		b, _ := json.MarshalIndent(cov, "", "  ")
		if err := os.WriteFile(filepath.Join(actx.Bundle.Dir, "loop-coverage.json"), b, 0o644); err != nil {
			return nil, err
		}
	}
	actx.Log("  %s: loop %s, %d iterations, %d operations, ended %s", r.Name, c.Name, iter, no, ended)
	return map[string]string{"value": ended, "iterations": strconv.Itoa(iter), "operations": strconv.Itoa(no)}, nil
}

// loopCoverage is the loop_coverage record.
func loopCoverage(l Loop, actions, phases map[string]int, iter int, ended string, ops int) map[string]any {
	never := []string{}
	declared := map[string]bool{}
	for _, list := range [][]LoopStep{l.Setup, l.Cycle, l.Teardown} {
		for _, s := range list {
			declared[s.Action] = true
		}
	}
	if l.Sweep != nil {
		declared[l.Sweep.Action] = true
	}
	for a := range declared {
		if actions[a] == 0 {
			never = append(never, a)
		}
	}
	slices.Sort(never)
	a, p := map[string]any{}, map[string]any{}
	for k, v := range actions {
		a[k] = v
	}
	for k, v := range phases {
		p[k] = v
	}
	return map[string]any{"iterations": iter, "ended": ended, "operations": ops, "actions": a, "phases": p, "declared_never_run": never}
}

// loopSegment is one phase of a recorded loop history.
type loopSegment struct {
	Phase string
	Steps []ReplayStep
}

// loopScript is a recorded loop history.
type loopScript struct {
	header   map[string]any
	loop     Loop
	segments []loopSegment
	events   []FaultEvent // nil without a fault schedule
	coverage map[string]any
}

// readLoopScript reads a recorded loop history: its header (driver loop/1,
// the declaration matching its hash), the fault schedule (checked by
// CheckFaultSchedule over fault_ops), and its phases with their steps and
// recorded outcomes (the random run's rule, replay.go: the worst complete
// record after a gen_step up to the next step, loop_phase, healthy, kill or
// restart, loop_coverage, loop_replay or end record).
func readLoopScript(name, path string, valid func(string) bool) (*loopScript, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	defer f.Close()
	sc := &loopScript{}
	needSchedule := false
	var cur *ReplayStep
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<20), 64<<20)
	line := 0
	for s.Scan() {
		line++
		var r tr.HistoryRecord
		if err := json.Unmarshal(s.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("%s: %s line %d: %w", name, path, line, err)
		}
		bad := func(format string, a ...any) error {
			return fmt.Errorf("%s: %s line %d: %s", name, path, line, fmt.Sprintf(format, a...))
		}
		switch r.Kind {
		case tr.HistoryObserve:
			if sc.header == nil {
				if r.Verb != "loop_header" {
					return nil, bad("the first observation is %q, not loop_header", r.Verb)
				}
				if r.Result["driver"] != LoopDriverVersion {
					return nil, bad("driver %v, this runner reads %s", r.Result["driver"], LoopDriverVersion)
				}
				b, _ := json.Marshal(r.Result["declaration"])
				d := json.NewDecoder(strings.NewReader(string(b)))
				d.DisallowUnknownFields()
				if err := d.Decode(&sc.loop); err != nil {
					return nil, bad("declaration: %v", err)
				}
				if sc.loop.Hash() != r.Result["declaration_sha256"] {
					return nil, bad("the declaration does not match its hash %v", r.Result["declaration_sha256"])
				}
				sc.header = r.Result
				_, needSchedule = r.Result["faults"]
				continue
			}
			if needSchedule {
				if r.Verb != "fault_schedule" {
					return nil, bad("a fault history without fault_schedule after its header")
				}
				needSchedule = false
				if sc.events, err = parseFaultEvents(r.Result); err != nil {
					return nil, bad("%v", err)
				}
				ops, _ := sc.header["fault_ops"].(float64)
				within, _ := sc.header["fault_restart_within"].(float64)
				if r.Result["seed"] != sc.header["seed"] {
					return nil, bad("fault_schedule seed %v, header seed %v", r.Result["seed"], sc.header["seed"])
				}
				if err := CheckFaultSchedule(sc.events, int(ops), int(within), valid); err != nil {
					return nil, bad("fault_schedule: %v", err)
				}
				continue
			}
			cur = nil
			switch r.Verb {
			case "loop_header", "fault_schedule":
				return nil, bad("a second %s", r.Verb)
			case "loop_phase":
				p, _ := r.Result["phase"].(string)
				sc.segments = append(sc.segments, loopSegment{Phase: p})
			case "gen_step":
				if len(sc.segments) == 0 {
					return nil, bad("a gen_step before any loop_phase")
				}
				o, err := replayOp(r)
				if err != nil {
					return nil, bad("%v", err)
				}
				seg := &sc.segments[len(sc.segments)-1]
				seg.Steps = append(seg.Steps, ReplayStep{Op: o})
				cur = &seg.Steps[len(seg.Steps)-1]
			case "loop_coverage":
				sc.coverage = r.Result
			}
		case tr.HistoryInvoke:
			if sc.events != nil && (r.Verb == FaultKill || r.Verb == FaultRestart) {
				cur = nil
			}
		case tr.HistoryComplete:
			if cur != nil {
				cur.Recorded = worseOutcome(cur.Recorded, r.Outcome)
			}
		case tr.HistoryEnd:
			cur = nil
		}
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("%s: %s: %w", name, path, err)
	}
	if sc.header == nil {
		return nil, fmt.Errorf("%s: %s: no loop_header", name, path)
	}
	if needSchedule {
		return nil, fmt.Errorf("%s: %s: a fault history without fault_schedule", name, path)
	}
	return sc, nil
}

// ReplayLoop re-executes a recorded loop history: the same header (with
// replay_of), fault schedule, phases and steps, each step through Step and the
// faults through the same faultRun, and compares every outcome with the
// recorded one. Last it writes "loop_replay" {steps, differences}.
func (r LoopRunner) ReplayLoop(ctx context.Context, actx *tr.ActionContext, src string) (map[string]string, error) {
	if actx.History == nil {
		return nil, fmt.Errorf("%s: no history (typed operations run only with a run bundle)", r.Name)
	}
	if src == "" {
		return nil, fmt.Errorf("%s: param history (the recorded history.jsonl) is required", r.Name)
	}
	sc, err := readLoopScript(r.Name, src, r.Driver.ValidProcess)
	if err != nil {
		return nil, err
	}
	if err := checkPhases(sc); err != nil {
		return nil, fmt.Errorf("%s: %s: %w", r.Name, src, err)
	}
	hdr := map[string]any{}
	for k, v := range sc.header {
		hdr[k] = v
	}
	hdr["replay_of"] = src
	if err := r.observe(actx, "loop_header", hdr); err != nil {
		return nil, err
	}
	var fr *faultRun
	if sc.events != nil {
		sched := FaultSchedule{Events: sc.events}
		sched.Seed, _ = strconv.ParseUint(fmt.Sprint(sc.header["seed"]), 10, 64)
		if err := r.observe(actx, "fault_schedule", sched.Record()); err != nil {
			return nil, err
		}
		fr = newFaultRun(r.Runner, sc.events, nil, sc.header, nil)
	}
	diffs := []any{}
	steps, cycleNo := 0, 0
	var first string
	for _, seg := range sc.segments {
		if seg.Phase == PhaseSweep && fr != nil { // the cycle ended (checkPhases: once, in order)
			if err := fr.finish(ctx, actx); err != nil {
				return nil, fmt.Errorf("%s: end of cycle: %w", r.Name, err)
			}
		}
		if err := r.observe(actx, "loop_phase", map[string]any{"phase": seg.Phase}); err != nil {
			return nil, err
		}
		for _, st := range seg.Steps {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("%s: %w at operation %d", r.Name, ctx.Err(), st.Op.No)
			}
			o := st.Op
			got, err := r.Step(ctx, actx, o)
			if err != nil {
				return nil, fmt.Errorf("%s: operation %d (%s %s): %w", r.Name, o.No, o.Action, o.Volume, err)
			}
			steps++
			if got != st.Recorded {
				rec := st.Recorded
				if rec == "" {
					rec = "none"
				}
				diffs = append(diffs, map[string]any{"opno": o.No, "action": o.Action, "volume": o.Volume, "recorded": rec, "replayed": got})
				if first == "" {
					first = fmt.Sprintf("operation %d (%s %s) recorded %s, replayed %s", o.No, o.Action, o.Volume, rec, got)
				}
			}
			if seg.Phase == PhaseCycle && fr != nil {
				cycleNo++
				fr.note(o, got)
				if err := fr.afterOp(ctx, actx, cycleNo); err != nil {
					return nil, fmt.Errorf("%s: %w", r.Name, err)
				}
			}
		}
	}
	if err := r.observe(actx, "loop_replay", map[string]any{"steps": steps, "differences": diffs}); err != nil {
		return nil, err
	}
	if len(diffs) > 0 {
		return nil, fmt.Errorf("%s: %d of %d steps differ; first: %s", r.Name, len(diffs), steps, first)
	}
	actx.Log("  %s: %d steps match %s", r.Name, steps, src)
	return map[string]string{"value": "match", "steps": strconv.Itoa(steps)}, nil
}

// checkPhases: the four phases, each once, in order.
func checkPhases(sc *loopScript) error {
	var got []string
	for _, s := range sc.segments {
		got = append(got, s.Phase)
	}
	if !slices.Equal(got, loopPhases) {
		return fmt.Errorf("phases %v, want %v", got, loopPhases)
	}
	return nil
}

// CheckLoop judges a recorded loop history against the declaration in its own
// header, from the history alone: the four phases in order; setup, cycle and
// teardown exactly the operations the declaration expands to over the pool
// and seed, in the declared order and counts, the cycle ending on an
// iteration boundary (exactly the iterations declared when no duration was
// set, at most them otherwise); and the sweep reading back every lba a
// writes_lbas step wrote (outcome not refused) and observing every subject.
// observe is the product's observation of a subject (Driver.Observe), valid
// its fault process names.
func CheckLoop(path string, observe func(string) Op, valid func(string) bool) error {
	sc, err := readLoopScript("check", path, valid)
	if err != nil {
		return err
	}
	if err := checkPhases(sc); err != nil {
		return err
	}
	pool := []string{}
	if ps, ok := sc.header["pool"].([]any); ok {
		for _, p := range ps {
			s, _ := p.(string)
			pool = append(pool, s)
		}
	}
	blocks, _ := sc.header["blocks"].(float64)
	iterations, _ := sc.header["iterations"].(float64)
	_, hasDuration := sc.header["duration"]
	seed, err := strconv.ParseUint(fmt.Sprint(sc.header["seed"]), 10, 64)
	if err != nil {
		return fmt.Errorf("header seed %v", sc.header["seed"])
	}
	plan, err := newLoopPlan(sc.loop, pool, int(blocks), seed)
	if err != nil {
		return fmt.Errorf("header: %w", err)
	}
	same := func(where string, want Op, got ReplayStep) error {
		g := got.Op
		if want.Action != g.Action || want.Volume != g.Volume || !mapsEqual(want.Params, g.Params) {
			return fmt.Errorf("%s operation %d: recorded %s %s %v, the declaration gives %s %s %v",
				where, g.No, g.Action, g.Volume, g.Params, want.Action, want.Volume, want.Params)
		}
		return nil
	}
	seq := func(where string, want []Op, got []ReplayStep) error {
		for i := 0; i < min(len(want), len(got)); i++ {
			if err := same(where, want[i], got[i]); err != nil {
				return err
			}
		}
		if len(want) != len(got) {
			return fmt.Errorf("%s: %d operations recorded, the declaration gives %d", where, len(got), len(want))
		}
		return nil
	}
	seg := sc.segments
	if err := seq(PhaseSetup, plan.once(sc.loop.Setup), seg[0].Steps); err != nil {
		return err
	}
	written := map[string]map[int]bool{}
	got := seg[1].Steps
	k, iter := 0, 0
	for k < len(got) {
		if iterations > 0 && iter >= int(iterations) {
			return fmt.Errorf("cycle: %d operations recorded beyond the %d iterations declared", len(got)-k, int(iterations))
		}
		iter++
		ops := plan.iteration(iter)
		if len(ops) == 0 {
			continue
		}
		if k+len(ops) > len(got) {
			return fmt.Errorf("cycle: iteration %d is cut short (%d of %d operations)", iter, len(got)-k, len(ops))
		}
		for _, pl := range ops {
			if err := same(fmt.Sprintf("cycle iteration %d", iter), pl.Op, got[k]); err != nil {
				return err
			}
			if pl.Writes != nil && got[k].Recorded != tr.OutcomeRefused {
				addWritten(written, pl.Op.Volume, pl.Writes)
			}
			k++
		}
	}
	if want := sc.loop.cycleOps(len(pool), int(iterations)); !hasDuration && want != len(got) {
		return fmt.Errorf("cycle: %d operations recorded (%d iterations), the %d iterations declared give %d", len(got), iter, int(iterations), want)
	}
	if err := seq(PhaseSweep, plan.sweep(written, observe), seg[2].Steps); err != nil {
		return err
	}
	return seq(PhaseTeardown, plan.once(sc.loop.Teardown), seg[3].Steps)
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}
