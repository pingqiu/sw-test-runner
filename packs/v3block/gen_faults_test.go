package v3block

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
)

var faultProcs = []string{"blockvolume:1", "blockmaster", "blockvolume:0"}

func TestFaultScheduleSameSeedSameScheduleAndPinned(t *testing.T) {
	a, err := NewFaultSchedule(8, 60, faultProcs, 0.08, 20)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewFaultSchedule(8, 60, []string{"blockmaster", "blockvolume:0", "blockvolume:1"}, 0.08, 20)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("the same seed (processes in another order) gave two schedules")
	}
	// Pinned: a change to the derivation changes every recorded run's schedule,
	// so it must come with a new GenVersionFaults.
	want := []FaultEvent{
		{24, FaultKill, "blockvolume:0"}, {29, FaultRestart, "blockvolume:0"},
		{34, FaultKill, "blockvolume:0"}, {39, FaultRestart, "blockvolume:0"},
		{50, FaultKill, "blockmaster"}, {57, FaultRestart, "blockmaster"},
	}
	if !reflect.DeepEqual(a.Events, want) {
		t.Fatalf("schedule for seed 8 changed:\n%#v", a.Events)
	}
	c, _ := NewFaultSchedule(9, 60, faultProcs, 0.08, 20)
	if reflect.DeepEqual(a.Events, c.Events) {
		t.Fatal("another seed gave the same schedule")
	}
	if off, err := NewFaultSchedule(7, 60, nil, 0, 20); err != nil || len(off.Events) != 0 {
		t.Fatalf("rate 0: %v %v", off, err)
	}
}

func TestFaultScheduleEveryKillRestartedWithinTheBoundOneDownAtATime(t *testing.T) {
	kills := 0
	for seed := uint64(1); seed <= 200; seed++ {
		for _, rate := range []float64{0.01, 0.1, 0.5, 1} {
			for _, within := range []int{1, 5, 20} {
				ops := 20 + int(seed%7)*30
				s, err := NewFaultSchedule(seed, ops, faultProcs, rate, within)
				if err != nil {
					t.Fatal(err)
				}
				if err := CheckFaultSchedule(s.Events, ops, within); err != nil {
					t.Fatalf("seed %d rate %v within %d: %v\n%v", seed, rate, within, err, s.Events)
				}
				// The same rules, stated directly here, independent of the checker.
				down := map[string]int{}
				for _, e := range s.Events {
					if e.Action == FaultKill {
						if len(down) != 0 {
							t.Fatalf("seed %d: %s killed while %v down", seed, e.Process, down)
						}
						down[e.Process] = e.AfterOp
						kills++
					} else {
						k, ok := down[e.Process]
						if !ok || e.AfterOp <= k || e.AfterOp-k > within || e.AfterOp > ops {
							t.Fatalf("seed %d: restart %+v (killed after %d, ok %v)", seed, e, k, ok)
						}
						delete(down, e.Process)
					}
				}
				if len(down) != 0 {
					t.Fatalf("seed %d: never restarted %v", seed, down)
				}
			}
		}
	}
	if kills < 1000 {
		t.Fatalf("only %d kills over the sweep", kills)
	}
	for name, f := range map[string]func() error{
		"rate above 1":    func() error { _, err := NewFaultSchedule(1, 10, faultProcs, 1.5, 20); return err },
		"bound 0":         func() error { _, err := NewFaultSchedule(1, 10, faultProcs, 0.1, 0); return err },
		"no processes":    func() error { _, err := NewFaultSchedule(1, 10, nil, 0.1, 20); return err },
		"unknown process": func() error { _, err := NewFaultSchedule(1, 10, []string{"blockvolume"}, 0.1, 20); return err },
		"repeated process": func() error {
			_, err := NewFaultSchedule(1, 10, []string{"blockmaster", "blockmaster"}, 0.1, 20)
			return err
		},
	} {
		if f() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Red controls for CheckFaultSchedule: each seeded defect must be caught.
func TestCheckFaultScheduleCatchesEachDefect(t *testing.T) {
	k := func(n int, p string) FaultEvent { return FaultEvent{n, FaultKill, p} }
	r := func(n int, p string) FaultEvent { return FaultEvent{n, FaultRestart, p} }
	good := []FaultEvent{k(2, "blockmaster"), r(5, "blockmaster"), k(5, "blockvolume:0"), r(25, "blockvolume:0")}
	if err := CheckFaultSchedule(good, 30, 20); err != nil {
		t.Fatalf("green: %v", err)
	}
	for name, ev := range map[string][]FaultEvent{
		"kill never restarted":     {k(2, "blockmaster")},
		"master and volume down":   {k(2, "blockmaster"), k(3, "blockvolume:0"), r(4, "blockvolume:0"), r(5, "blockmaster")},
		"restart beyond the bound": {k(2, "blockmaster"), r(23, "blockmaster")},
		"restart with no op down":  {k(2, "blockmaster"), r(2, "blockmaster")},
		"restart of another":       {k(2, "blockmaster"), r(4, "blockvolume:0")},
		"out of order":             {k(9, "blockmaster"), r(12, "blockmaster"), k(3, "blockvolume:0"), r(4, "blockvolume:0")},
		"after the last op":        {k(29, "blockmaster"), r(31, "blockmaster")},
		"before the first op":      {k(0, "blockmaster"), r(1, "blockmaster")},
		"unknown process":          {k(2, "blockvolume:x"), r(3, "blockvolume:x")},
		"unknown action":           {{2, "pause", "blockmaster"}},
	} {
		if err := CheckFaultSchedule(ev, 30, 20); err == nil {
			t.Errorf("red control %q not caught", name)
		}
	}
}

// faultFake extends fakeProduct with the fault actions: kill, restart and
// healthy record as the contract says; while a process is down every
// ordinary operation but a read answers unknown (so unknowns arise only
// from faults, deterministically, and replay matches).
type faultFake struct {
	fakeProduct
	down        string
	maxDown     int
	unhealthy   int // the first n healthy calls answer false
	healthyCall int
	log         []string
}

func (f *faultFake) install(t *testing.T) {
	f.fakeProduct.install(t)
	for name, inner := range genHandlers {
		name, inner := name, inner
		if name == "v3_op_observe" || name == "v3_op_read" {
			continue
		}
		genHandlers[name] = func(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
			if f.down == "" || act.Params["expect"] != "" {
				return inner(ctx, actx, act)
			}
			f.calls = append(f.calls, act)
			seq, _ := actx.History.Invoke(tr.HistoryRecord{Op: "X." + strings.TrimPrefix(name, "v3_op_"), Verb: strings.TrimPrefix(name, "v3_op_"), Volume: act.Params["volume"]})
			_ = actx.History.Complete(seq, tr.OutcomeUnknown, nil)
			return map[string]string{"value": tr.OutcomeUnknown}, nil
		}
	}
	fault := func(verb string) tr.ActionHandlerFunc {
		return func(_ context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
			p := act.Params["process"]
			f.log = append(f.log, verb+" "+p)
			if verb == FaultKill {
				if f.down != "" {
					f.maxDown = 2
				}
				f.down = p
				f.maxDown = max(f.maxDown, 1)
			} else if f.down == p {
				f.down = ""
			}
			seq, _ := actx.History.Invoke(tr.HistoryRecord{Op: "Process." + verb, Verb: verb, Args: map[string]any{"process": p}})
			_ = actx.History.Complete(seq, tr.OutcomeOK, nil)
			return map[string]string{"value": tr.OutcomeOK}, nil
		}
	}
	genHandlers["v3_op_kill"] = fault(FaultKill)
	genHandlers["v3_op_restart"] = fault(FaultRestart)
	genHandlers["v3_op_healthy"] = func(_ context.Context, actx *tr.ActionContext, _ tr.Action) (map[string]string, error) {
		f.healthyCall++
		ok := f.down == "" && f.healthyCall > f.unhealthy
		f.log = append(f.log, fmt.Sprintf("healthy %v", ok))
		_ = actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "runner", Verb: "healthy", Result: map[string]any{"healthy": ok, "down": []string{}, "not_serving": []string{}}})
		return map[string]string{"value": tr.OutcomeOK, "healthy": fmt.Sprint(ok)}, nil
	}
}

func faultAct(ops, rate string) tr.Action {
	a := genAct(ops)
	a.Params["pool"] = strings.Join(pool(100), " ") // not exhausted before the budget
	a.Params["faults"] = rate
	a.Params["fault_processes"] = "blockmaster blockvolume:0"
	return a
}

// unresolvedUnknowns judges a history the way C6 needs it resolved: every
// unknown operation of a step (not a read) must be followed, after a healthy
// point that answered healthy, by a read answered ok of its volume (a write)
// or an observation of its volume (anything else). It returns the ones that
// were not.
func unresolvedUnknowns(recs []tr.HistoryRecord) []string {
	type pend struct {
		action, vol string
		opno        int
	}
	var pending []pend
	var step map[string]any
	var stepVol string
	resolving := false
	read, observed := map[string]bool{}, map[string]bool{}
	settle := func() {
		if !resolving {
			return
		}
		var left []pend
		for _, p := range pending {
			if (p.action == "v3_op_write" && !read[p.vol]) || (p.action != "v3_op_write" && !observed[p.vol]) {
				left = append(left, p)
			}
		}
		pending, resolving = left, false
		read, observed = map[string]bool{}, map[string]bool{}
	}
	for _, r := range recs {
		switch {
		case r.Verb == "gen_step":
			settle()
			step, stepVol = r.Result, r.Volume
		case r.Kind == tr.HistoryInvoke && (r.Verb == FaultKill || r.Verb == FaultRestart):
			settle()
			step = nil
		case r.Verb == "healthy":
			settle()
			step = nil
			resolving = r.Result["healthy"] == true
		case r.Verb == "gen_coverage":
			settle()
			step = nil
		case r.Kind == tr.HistoryComplete && step != nil && r.Outcome == tr.OutcomeUnknown && step["action"] != "v3_op_read":
			pending = append(pending, pend{step["action"].(string), stepVol, int(step["opno"].(float64))})
			step = nil // one pending entry per step
		case resolving && r.Kind == tr.HistoryObserve && r.Verb == "observe":
			observed[r.Volume] = true
		case resolving && r.Kind == tr.HistoryComplete && r.Verb == "read" && r.Outcome == tr.OutcomeOK:
			read[r.Volume] = true
		}
	}
	settle()
	var out []string
	for _, p := range pending {
		out = append(out, fmt.Sprintf("op %d %s %s", p.opno, p.action, p.vol))
	}
	return out
}

func TestGenRunWithFaultsKillsRestartsAndResolvesEveryUnknown(t *testing.T) {
	f := &faultFake{}
	f.install(t)
	actx, p := genRunCtx(t)
	if _, err := v3GenRun(context.Background(), actx, faultAct("300", "0.05")); err != nil {
		t.Fatal(err)
	}
	_ = actx.History.Close()
	recs := records(t, p)
	if recs[0].Result["generator"] != GenVersionFaults || recs[1].Verb != "fault_schedule" {
		t.Fatalf("header %+v, then %+v", recs[0], recs[1])
	}
	want, _ := NewFaultSchedule(5, 300, []string{"blockmaster", "blockvolume:0"}, 0.05, DefaultRestartWithin)
	if !reflect.DeepEqual(recs[1].Result, roundTrip(t, want.record())) || len(want.Events) < 4 {
		t.Fatalf("fault_schedule %v, want %v", recs[1].Result, want.record())
	}
	// Each event ran after its operation: the last gen_step before it has its after_op.
	lastStep, ev := 0, 0
	for _, r := range recs {
		if r.Verb == "gen_step" {
			lastStep = int(r.Result["opno"].(float64))
		}
		if r.Kind == tr.HistoryInvoke && (r.Verb == FaultKill || r.Verb == FaultRestart) {
			e := want.Events[ev]
			if r.Verb != e.Action || r.Args["process"] != e.Process || lastStep != e.AfterOp {
				t.Fatalf("event %d ran as %s %v after op %d, scheduled %+v", ev, r.Verb, r.Args, lastStep, e)
			}
			ev++
		}
	}
	if ev != len(want.Events) || f.maxDown != 1 || f.down != "" {
		t.Fatalf("%d of %d events ran, max down %d, down at end %q", ev, len(want.Events), f.maxDown, f.down)
	}
	// After every restart, a healthy point.
	for i, l := range f.log {
		if strings.HasPrefix(l, FaultRestart) && !strings.HasPrefix(f.log[i+1], "healthy") {
			t.Fatalf("restart not followed by a healthy point: %v", f.log)
		}
	}
	unknowns := 0
	for _, r := range recs {
		if r.Outcome == tr.OutcomeUnknown {
			unknowns++
		}
	}
	if unknowns == 0 {
		t.Fatal("no operation was unknown: the faults exercised nothing")
	}
	if left := unresolvedUnknowns(recs); len(left) != 0 {
		t.Fatalf("unknowns not resolved: %v", left)
	}
	// Red control: the same history with the resolving observations removed
	// must be caught.
	var cut []tr.HistoryRecord
	resolving := false
	for _, r := range recs {
		switch {
		case r.Verb == "healthy":
			resolving = true
		case r.Verb == "gen_step" || r.Verb == "gen_coverage" || r.Kind == tr.HistoryInvoke && (r.Verb == FaultKill || r.Verb == FaultRestart):
			resolving = false
		case resolving:
			continue
		}
		cut = append(cut, r)
	}
	if left := unresolvedUnknowns(cut); len(left) == 0 {
		t.Fatal("red control: a history without resolutions passed")
	}
	cov := lastVerb(t, p, "gen_coverage").Result
	fc, _ := cov["fault_cells"].(map[string]any)
	for _, c := range []string{"kill|blockmaster", "restart|blockmaster", "kill|blockvolume:0", "restart|blockvolume:0", "while_down|v3_op_write"} {
		if fc[c] == nil {
			t.Errorf("fault cell %s missing: %v", c, fc)
		}
	}
}

func roundTrip(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	p := writeHistory(t, func(h *tr.History) { obs(h, "x", "", m) })
	return records(t, p)[0].Result
}

func TestGenRunWithFaultsResolvesAtTheNextHealthyPoint(t *testing.T) {
	f := &faultFake{unhealthy: 1}
	f.install(t)
	actx, p := genRunCtx(t)
	if _, err := v3GenRun(context.Background(), actx, faultAct("300", "0.05")); err != nil {
		t.Fatal(err)
	}
	_ = actx.History.Close()
	recs := records(t, p)
	if left := unresolvedUnknowns(recs); len(left) != 0 {
		t.Fatalf("unknowns not resolved: %v", left)
	}
	// Nothing is resolved at the point that was not healthy.
	for i, r := range recs {
		if r.Verb == "healthy" && r.Result["healthy"] == false {
			if n := recs[i+1]; n.Verb == "observe" || n.Kind == tr.HistoryInvoke && n.Verb == "read" {
				t.Fatalf("resolution after an unhealthy point: %+v", n)
			}
			break
		}
	}
}

func TestGenRunWithFaultsEndsHealthyWhenTheRunEndsWhileDown(t *testing.T) {
	f := &faultFake{}
	f.install(t)
	actx, p := genRunCtx(t)
	a := faultAct("1000", "1")
	a.Params["pool"] = "g1 g2 g3"
	a.Params["weights"] = "v3_op_create=1 v3_op_delete=1"
	out, err := v3GenRun(context.Background(), actx, a)
	if err != nil {
		t.Fatal(err)
	}
	_ = actx.History.Close()
	if out["value"] != "pool exhausted" || f.down != "" {
		t.Fatalf("ended %v, down %q, log %v", out, f.down, f.log)
	}
	kills, restarts := 0, 0
	for _, l := range f.log {
		kills += btoi(strings.HasPrefix(l, FaultKill))
		restarts += btoi(strings.HasPrefix(l, FaultRestart))
	}
	if kills == 0 || kills != restarts || !strings.HasPrefix(f.log[len(f.log)-1], "healthy") {
		t.Fatalf("log %v", f.log)
	}
	if left := unresolvedUnknowns(records(t, p)); len(left) != 0 {
		t.Fatalf("unknowns not resolved: %v", left)
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestGenReplayWithFaultsMatchesAndReExecutesTheSchedule(t *testing.T) {
	f := &faultFake{}
	f.install(t)
	actx, a := genRunCtx(t)
	if _, err := v3GenRun(context.Background(), actx, faultAct("300", "0.05")); err != nil {
		t.Fatal(err)
	}
	_ = actx.History.Close()
	recLog := f.log
	f.log = nil
	out, b, err := replayInto(t, a)
	if err != nil || out["value"] != "match" {
		t.Fatalf("%v %v", out, err)
	}
	if !reflect.DeepEqual(f.log, recLog) {
		t.Fatalf("the replay ran other fault events:\n%v\n%v", f.log, recLog)
	}
	ra, rb := records(t, a), records(t, b)
	if rb[1].Verb != "fault_schedule" || !reflect.DeepEqual(ra[1].Result, rb[1].Result) || !reflect.DeepEqual(stepsOf(t, a), stepsOf(t, b)) {
		t.Fatal("the replay's schedule or steps differ")
	}
	if left := unresolvedUnknowns(rb); len(left) != 0 {
		t.Fatalf("replay left unknowns unresolved: %v", left)
	}
	if out, _, err := replayInto(t, b); err != nil || out["value"] != "match" {
		t.Fatalf("replay of the replay: %v %v", out, err)
	}
	// Red control: a replay that runs no faults differs (the steps run while
	// a process was down answered unknown when recorded).
	delete(genHandlers, "v3_op_kill")
	genHandlers["v3_op_kill"] = func(_ context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
		seq, _ := actx.History.Invoke(tr.HistoryRecord{Op: "Process.kill", Verb: FaultKill})
		_ = actx.History.Complete(seq, tr.OutcomeOK, nil)
		return map[string]string{"value": tr.OutcomeOK}, nil // kills nothing
	}
	if _, _, err := replayInto(t, a); err == nil || !strings.Contains(err.Error(), "differ") {
		t.Fatalf("red control: a replay without the faults matched: %v", err)
	}
}

func TestGenReplayRefusesABadFaultScheduleBeforeAnyCall(t *testing.T) {
	hdr := map[string]any{"generator": GenVersionFaults, "seed": "1", "ops": 10, "fault_restart_within": 20}
	step := map[string]any{"opno": 1, "action": "v3_op_create", "params": map[string]any{"volume": "g1"}}
	ev := func(k int, a, p string) map[string]any {
		return map[string]any{"after_op": k, "action": a, "process": p}
	}
	for name, fill := range map[string]func(h *tr.History){
		"no schedule": func(h *tr.History) {
			obs(h, "gen_header", "", hdr)
			obs(h, "gen_step", "g1", step)
			opRec(h, "create", "g1", tr.OutcomeOK)
		},
		"kill never restarted": func(h *tr.History) {
			obs(h, "gen_header", "", hdr)
			obs(h, "fault_schedule", "", map[string]any{"seed": "1", "events": []any{ev(1, FaultKill, "blockmaster")}})
			obs(h, "gen_step", "g1", step)
			opRec(h, "create", "g1", tr.OutcomeOK)
		},
		"two down": func(h *tr.History) {
			obs(h, "gen_header", "", hdr)
			obs(h, "fault_schedule", "", map[string]any{"seed": "1", "events": []any{ev(1, FaultKill, "blockmaster"), ev(2, FaultKill, "blockvolume:0"), ev(3, FaultRestart, "blockvolume:0"), ev(4, FaultRestart, "blockmaster")}})
			obs(h, "gen_step", "g1", step)
			opRec(h, "create", "g1", tr.OutcomeOK)
		},
		"another seed": func(h *tr.History) {
			obs(h, "gen_header", "", hdr)
			obs(h, "fault_schedule", "", map[string]any{"seed": "2", "events": []any{}})
			obs(h, "gen_step", "g1", step)
			opRec(h, "create", "g1", tr.OutcomeOK)
		},
		"schedule in an M1 history": func(h *tr.History) {
			obs(h, "gen_header", "", map[string]any{"generator": GenVersion, "seed": "1"})
			obs(h, "gen_step", "g1", step)
			obs(h, "fault_schedule", "", map[string]any{"seed": "1", "events": []any{}})
			opRec(h, "create", "g1", tr.OutcomeOK)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := &faultFake{}
			f.install(t)
			p := writeHistory(t, fill)
			if _, _, err := replayInto(t, p); err == nil {
				t.Fatal("accepted")
			}
			if len(f.calls) != 0 || len(f.log) != 0 {
				t.Fatalf("calls before the refusal: %d, %v", len(f.calls), f.log)
			}
		})
	}
}

func TestGeneratorLearnsAnUnknownWriteInAFaultRun(t *testing.T) {
	cfg := GenConfig{Seed: 1, Ops: 400, Pool: pool(1), Blocks: 4, Negative: 0.01,
		Faults: 0.1, FaultProcesses: []string{"blockmaster"},
		Weights: map[string]int{"v3_op_create": 1, "v3_op_attach": 1, "v3_op_write": 1, "v3_op_stop": 50, "v3_op_move": 50}}
	g, err := NewGenerator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	drive := func(want string) GenOp {
		for {
			o, ok := g.Next()
			if !ok {
				t.Fatalf("run ended before a %s", want)
			}
			if o.Negative != "" {
				g.Learn(o, "refused")
				continue
			}
			if o.Action == want {
				return o
			}
			g.Learn(o, "ok")
		}
	}
	g.Learn(drive("v3_op_create"), "ok")
	g.Learn(drive("v3_op_attach"), "ok")
	g.Learn(drive("v3_op_write"), "unknown")
	// The volume stays serving and writable, but is not stopped or moved
	// while its unknown write is unresolved.
	for i := 0; i < 30; i++ {
		o, ok := g.Next()
		if !ok {
			t.Fatal("ended")
		}
		if o.Negative == "" && (o.Action == "v3_op_stop" || o.Action == "v3_op_move") {
			t.Fatalf("%s proposed with an unresolved write", o.Action)
		}
		if o.Negative == "" && o.Action != "v3_op_write" && o.Action != "v3_op_read" {
			t.Fatalf("unexpected %s", o.Action)
		}
		g.Learn(o, "ok")
	}
	g.Resolved("g1")
	if o := drive("v3_op_stop"); o.Volume != "g1" {
		t.Fatalf("%+v", o)
	}
	// Without faults an unknown write takes the volume out, as in M1.
	cfg.Faults = 0
	g, _ = NewGenerator(cfg)
	g.Learn(drive("v3_op_create"), "ok")
	g.Learn(drive("v3_op_attach"), "ok")
	g.Learn(drive("v3_op_write"), "unknown")
	if o, ok := g.Next(); ok && o.Negative == "" {
		t.Fatalf("an M1 run proposed %s on a volume whose write was unknown", o.Action)
	}
	h := g.Header()
	if h["generator"] != GenVersion || h["faults"] != nil {
		t.Fatalf("an M1 header %v", h)
	}
}

func TestGenExecTakesAnObservationsAnswerAsNoOutcome(t *testing.T) {
	old := genHandlers
	t.Cleanup(func() { genHandlers = old })
	genHandlers = map[string]tr.ActionHandlerFunc{"v3_op_observe": func(context.Context, *tr.ActionContext, tr.Action) (map[string]string, error) {
		return map[string]string{"value": "observed"}, nil // what v3OpObserve answers
	}}
	actx, _ := genRunCtx(t)
	if _, err := genExec(context.Background(), actx, "host", GenOp{Action: "v3_op_observe", Volume: "g1", Params: map[string]string{"volume": "g1"}}); err != nil {
		t.Fatal(err)
	}
}
