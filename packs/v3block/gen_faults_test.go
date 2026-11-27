package v3block

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/randomrun"
)

// The schedule itself is generic (randomrun, with its pin and red
// controls); the block names its processes. The same pinned schedule over
// the block's process names, and the block's name check in both directions.
func TestFaultScheduleOverBlockProcesses(t *testing.T) {
	a, err := NewFaultSchedule(8, 60, []string{"blockvolume:1", "blockmaster", "blockvolume:0"}, 0.08, 20)
	if err != nil {
		t.Fatal(err)
	}
	want := []FaultEvent{
		{AfterOp: 24, Action: FaultKill, Process: "blockvolume:0"}, {AfterOp: 29, Action: FaultRestart, Process: "blockvolume:0"},
		{AfterOp: 34, Action: FaultKill, Process: "blockvolume:0"}, {AfterOp: 39, Action: FaultRestart, Process: "blockvolume:0"},
		{AfterOp: 50, Action: FaultKill, Process: "blockmaster"}, {AfterOp: 57, Action: FaultRestart, Process: "blockmaster"},
	}
	if !reflect.DeepEqual(a.Events, want) || CheckFaultSchedule(a.Events, 60, 20) != nil {
		t.Fatalf("schedule for seed 8 changed:\n%#v", a.Events)
	}
	for _, p := range []string{"blockvolume", "blockvolume:x", "blockvolume:01", "blockvolume:-1", "master"} {
		if ValidFaultProcess(p) {
			t.Errorf("process %q accepted", p)
		}
		if _, err := NewFaultSchedule(1, 10, []string{p}, 0.1, 20); err == nil {
			t.Errorf("a schedule over %q accepted", p)
		}
	}
	bad := []FaultEvent{{AfterOp: 2, Action: FaultKill, Process: "blockvolume:x"}, {AfterOp: 3, Action: FaultRestart, Process: "blockvolume:x"}}
	if CheckFaultSchedule(bad, 30, 20) == nil {
		t.Error("a recorded schedule over an unknown process passed")
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
	waits       []string // wait_seconds of each healthy call
	// A short master outage: from a single look while the master is down to
	// its restart, writes answer as the product should (ok), or unknown when
	// outageUnknown seeds the defect MO must catch.
	inOutage      bool
	outageUnknown bool
	ready         []string // ready_seconds of each restart
}

func (f *faultFake) install(t *testing.T) {
	f.fakeProduct.install(t)
	for name, inner := range genHandlers {
		name, inner := name, inner
		if name == "v3_op_observe" || name == "v3_op_read" {
			continue
		}
		genHandlers[name] = func(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
			if f.down == "" || act.Params["expect"] != "" || (f.inOutage && !f.outageUnknown) {
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
				f.down, f.inOutage = "", false
				f.ready = append(f.ready, act.Params["ready_seconds"])
			}
			seq, _ := actx.History.Invoke(tr.HistoryRecord{Op: "Process." + verb, Verb: verb, Args: map[string]any{"process": p}})
			_ = actx.History.Complete(seq, tr.OutcomeOK, nil)
			return map[string]string{"value": tr.OutcomeOK}, nil
		}
	}
	genHandlers["v3_op_kill"] = fault(FaultKill)
	genHandlers["v3_op_restart"] = fault(FaultRestart)
	genHandlers["v3_op_healthy"] = func(_ context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
		if act.Params["look"] == "single" {
			f.inOutage = f.inOutage || f.down == "blockmaster"
			f.log = append(f.log, "look")
			_ = actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "runner", Verb: "healthy", Result: map[string]any{
				"healthy": false, "down": []string{f.down}, "not_serving": []string{}, "holders": map[string]any{}, "look": "single", "attempts": 1}})
			return map[string]string{"value": tr.OutcomeOK, "healthy": "false"}, nil
		}
		f.healthyCall++
		ok := f.down == "" && f.healthyCall > f.unhealthy
		f.log = append(f.log, fmt.Sprintf("healthy %v", ok))
		f.waits = append(f.waits, act.Params["wait_seconds"])
		_ = actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "runner", Verb: "healthy", Result: map[string]any{"healthy": ok, "down": []string{}, "not_serving": []string{}}})
		return map[string]string{"value": tr.OutcomeOK, "healthy": fmt.Sprint(ok)}, nil
	}
}

func faultAct(ops, rate string) tr.Action {
	a := genAct(ops)
	a.Params["pool"] = strings.Join(pool(100), " ") // not exhausted before the budget
	a.Params["faults"] = rate
	a.Params["fault_processes"] = "blockvolume:0 blockvolume:1"
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
	want, _ := NewFaultSchedule(5, 300, []string{"blockvolume:0", "blockvolume:1"}, 0.05, DefaultRestartWithin)
	if !reflect.DeepEqual(recs[1].Result, roundTrip(t, want.Record())) || len(want.Events) < 4 {
		t.Fatalf("fault_schedule %v, want %v", recs[1].Result, want.Record())
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
	for _, c := range []string{"kill|blockvolume:1", "restart|blockvolume:1", "kill|blockvolume:0", "restart|blockvolume:0", "while_down|v3_op_write"} {
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

func TestGenRunWithFaultsPassesTheHealthyWaitInRunAndReplay(t *testing.T) {
	f := &faultFake{}
	f.install(t)
	actx, a := genRunCtx(t)
	act := faultAct("120", "0.05")
	act.Params["fault_healthy_wait"] = "45"
	if _, err := v3GenRun(context.Background(), actx, act); err != nil {
		t.Fatal(err)
	}
	_ = actx.History.Close()
	if records(t, a)[0].Result["fault_healthy_wait"] != "45" || len(f.waits) < 2 {
		t.Fatalf("header %v, waits %v", records(t, a)[0].Result, f.waits)
	}
	for _, w := range f.waits {
		if w != "45" {
			t.Fatalf("waits %v", f.waits)
		}
	}
	f.waits = nil
	if out, _, err := replayInto(t, a); err != nil || out["value"] != "match" {
		t.Fatalf("%v %v", out, err)
	}
	for _, w := range f.waits {
		if w != "45" {
			t.Fatalf("replay waits %v", f.waits)
		}
	}
	// Without the param the handler's own default applies (no wait_seconds sent).
	f2 := &faultFake{}
	f2.install(t)
	actx2, _ := genRunCtx(t)
	if _, err := v3GenRun(context.Background(), actx2, faultAct("60", "0.05")); err != nil {
		t.Fatal(err)
	}
	for _, w := range f2.waits {
		if w != "" {
			t.Fatalf("waits %v", f2.waits)
		}
	}
	act.Params["fault_healthy_wait"] = "-1"
	actx3, _ := genRunCtx(t)
	if _, err := v3GenRun(context.Background(), actx3, act); err == nil {
		t.Fatal("a negative wait was accepted")
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
			obs(h, "fault_schedule", "", map[string]any{"seed": "1", "events": []any{ev(1, FaultKill, "blockvolume:1")}})
			obs(h, "gen_step", "g1", step)
			opRec(h, "create", "g1", tr.OutcomeOK)
		},
		"two down": func(h *tr.History) {
			obs(h, "gen_header", "", hdr)
			obs(h, "fault_schedule", "", map[string]any{"seed": "1", "events": []any{ev(1, FaultKill, "blockvolume:1"), ev(2, FaultKill, "blockvolume:0"), ev(3, FaultRestart, "blockvolume:0"), ev(4, FaultRestart, "blockvolume:1")}})
			obs(h, "gen_step", "g1", step)
			opRec(h, "create", "g1", tr.OutcomeOK)
		},
		"an ordinary kill of the master": func(h *tr.History) {
			obs(h, "gen_header", "", hdr)
			obs(h, "fault_schedule", "", map[string]any{"seed": "1", "events": []any{ev(1, FaultKill, "blockmaster"), ev(2, FaultRestart, "blockmaster")}})
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
		Faults: 0.1, FaultProcesses: []string{"blockvolume:0"},
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
			if o.Action == "v3_op_stop" || o.Action == "v3_op_move" {
				// Refused changes nothing: the one volume stays serving (a
				// stopped volume never serves again).
				g.Learn(o, "refused")
				continue
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
		if o.Negative != "" {
			g.Learn(o, "refused") // a negative answered ok takes the volume out
			continue
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

func outageAct(ops string) tr.Action {
	a := faultAct(ops, "0.05")
	a.Params["fault_master_outage"] = "0.05"
	// Holder kills only: a kill of the master by the schedule is a window MO
	// judges too, and this fake answers unknown while a process is down.
	a.Params["fault_processes"] = "blockvolume:0"
	return a
}

// judgeOutages states checker MO's rule on a history, independently of the
// runner: each window from an ok kill of blockmaster's completion to its next
// ok restart's completion holds at least one write and one single look; every
// write in it answers ok; every single look says which volumes serve and
// shows none not serving. It returns the windows judged and the findings.
func judgeOutages(recs []tr.HistoryRecord) (int, []string) {
	var f []string
	windows := 0
	open := map[uint64]tr.HistoryRecord{}
	kill := uint64(0)
	writes, looks := 0, 0
	for _, r := range recs {
		switch r.Kind {
		case tr.HistoryInvoke:
			open[r.Seq] = r
		case tr.HistoryComplete:
			inv := open[r.Of]
			switch {
			case inv.Verb == FaultKill && inv.Args["process"] == "blockmaster" && r.Outcome == tr.OutcomeOK:
				kill, writes, looks = r.Seq, 0, 0
			case inv.Verb == FaultRestart && inv.Args["process"] == "blockmaster" && kill != 0 && r.Outcome == tr.OutcomeOK:
				if writes == 0 || looks == 0 {
					f = append(f, fmt.Sprintf("window from seq %d: %d writes, %d looks", kill, writes, looks))
				}
				windows++
				kill = 0
			case inv.Verb == "write" && kill != 0:
				writes++
				if r.Outcome != tr.OutcomeOK {
					f = append(f, fmt.Sprintf("write seq %d %s answered %s in the window", inv.Seq, inv.Volume, r.Outcome))
				}
			}
		case tr.HistoryObserve:
			if r.Verb == "healthy" && kill != 0 && r.Result["look"] == "single" {
				looks++
				ns, ok := r.Result["not_serving"].([]any)
				if !ok || len(ns) != 0 {
					f = append(f, fmt.Sprintf("look seq %d not_serving %v", r.Seq, r.Result["not_serving"]))
				}
			}
		}
	}
	return windows, f
}

func TestGenRunWithMasterOutagesHasTheShapesMOReads(t *testing.T) {
	f := &faultFake{}
	f.install(t)
	actx, a := genRunCtx(t)
	if _, err := v3GenRun(context.Background(), actx, outageAct("300")); err != nil {
		t.Fatal(err)
	}
	_ = actx.History.Close()
	recs := records(t, a)
	if recs[0].Result["fault_master_outage"] != 0.05 {
		t.Fatalf("header %v", recs[0].Result)
	}
	base, _ := NewFaultSchedule(5, 300, []string{"blockvolume:0"}, 0.05, DefaultRestartWithin)
	want, err := randomrun.AddMasterOutages(base, 300, "blockmaster", 0.05, ValidFaultProcess)
	if err != nil || !reflect.DeepEqual(recs[1].Result, roundTrip(t, want.Record())) {
		t.Fatalf("fault_schedule %v, want %v (%v)", recs[1].Result, want.Record(), err)
	}
	outages := 0
	for _, e := range want.Events {
		if e.Action == randomrun.FaultMasterOutage {
			outages++
		}
	}
	if outages == 0 {
		t.Fatal("the pinned run has no master outage")
	}
	// Each marker is followed by exactly: a normal healthy record, the kill, a
	// single look, its writes, a single look, the restart, a healthy record.
	markers, wrote := 0, 0
	for i, r := range recs {
		if r.Verb != "master_outage" {
			continue
		}
		markers++
		if r.Result["max_seconds"] != float64(genOutage.maxSeconds) || r.Result["process"] != "blockmaster" {
			t.Fatalf("marker %v", r.Result)
		}
		var shape []string
		shape = append(shape, "observe healthy ", "invoke kill blockmaster", "complete kill blockmaster", "observe healthy single")
		for _, w := range r.Result["writes"].([]any) {
			v := w.(map[string]any)["volume"].(string)
			shape = append(shape, "invoke write "+v, "complete write "+v)
			wrote++
		}
		shape = append(shape, "observe healthy single", "invoke restart blockmaster", "complete restart blockmaster", "observe healthy ")
		for j, s := range shape {
			x := recs[i+1+j]
			got := x.Kind + " " + x.Verb + " "
			switch {
			case x.Verb == "healthy":
				look, _ := x.Result["look"].(string)
				got += look
			case x.Kind == tr.HistoryInvoke && x.Args["process"] != nil:
				got += x.Args["process"].(string)
			case x.Kind == tr.HistoryComplete && (x.Verb == FaultKill || x.Verb == FaultRestart):
				got += "blockmaster"
			default:
				got += x.Volume
			}
			if got != s {
				t.Fatalf("outage at record %d, record %d: %q, want %q", i, j, got, s)
			}
		}
	}
	if markers != outages || wrote == 0 {
		t.Fatalf("%d markers for %d outages, %d writes", markers, outages, wrote)
	}
	for _, r := range f.ready {
		if r != "" && r != strconv.Itoa(genOutage.maxSeconds) {
			t.Fatalf("restart ready_seconds %v", f.ready)
		}
	}
	if n, bad := judgeOutages(recs); n != outages || len(bad) != 0 {
		t.Fatalf("MO's rule: %d windows, %v", n, bad)
	}
	if left := unresolvedUnknowns(recs); len(left) != 0 {
		t.Fatalf("unknowns not resolved: %v", left)
	}
	fc, _ := lastVerb(t, a, "gen_coverage").Result["fault_cells"].(map[string]any)
	if fc["master_outage|blockmaster"] != float64(outages) {
		t.Fatalf("fault cells %v", fc)
	}
	// The replay matches and re-runs the same outages.
	recLog := f.log
	f.log = nil
	out, b, err := replayInto(t, a)
	if err != nil || out["value"] != "match" || !reflect.DeepEqual(f.log, recLog) {
		t.Fatalf("%v %v\n%v\n%v", out, err, f.log, recLog)
	}
	if n, bad := judgeOutages(records(t, b)); n != outages || len(bad) != 0 {
		t.Fatalf("replay, MO's rule: %d windows, %v", n, bad)
	}

	// Red control: a product whose writes fail in the outage is caught.
	f2 := &faultFake{outageUnknown: true}
	f2.install(t)
	actx2, a2 := genRunCtx(t)
	if _, err := v3GenRun(context.Background(), actx2, outageAct("300")); err != nil {
		t.Fatal(err)
	}
	_ = actx2.History.Close()
	if _, bad := judgeOutages(records(t, a2)); len(bad) == 0 {
		t.Fatal("red control: writes unknown inside the outage passed MO's rule")
	}
}

// The outage bound and the master are read from product/checks.json.
func TestOutageFactsComeFromChecksJSON(t *testing.T) {
	if !genOutageOK || genOutage.master != "blockmaster" || genOutage.maxSeconds <= 0 {
		t.Fatalf("%+v %v", genOutage, genOutageOK)
	}
	for name, b := range map[string]string{
		"no master_outage": `{"fault_targets": [{"name": "blockmaster", "role": "master"}]}`,
		"no cite":          `{"master_outage": {"max_seconds": 10}, "fault_targets": [{"name": "blockmaster", "role": "master"}]}`,
		"zero seconds":     `{"master_outage": {"max_seconds": 0, "cite": "x"}, "fault_targets": [{"name": "blockmaster", "role": "master"}]}`,
		"no master":        `{"master_outage": {"max_seconds": 10, "cite": "x"}, "fault_targets": []}`,
	} {
		if _, ok := readOutageFacts([]byte(b)); ok {
			t.Errorf("%s: accepted", name)
		}
	}
	if f, ok := readOutageFacts([]byte(`{"master_outage": {"max_seconds": 7, "cite": "x"}, "fault_targets": [{"name": "blockmaster", "role": "master"}]}`)); !ok || f.maxSeconds != 7 {
		t.Errorf("%+v %v", f, ok)
	}
}

// The master is never killed by an ordinary kill: a run configured to is
// refused before anything runs (the short outage is its own event).
func TestNoOrdinaryKillOfTheMaster(t *testing.T) {
	_, err := NewGenerator(GenConfig{Seed: 1, Ops: 50, Pool: pool(4), Blocks: 8, Faults: 0.1, FaultProcesses: []string{"blockmaster", "blockvolume:0"}})
	if err == nil {
		t.Fatal("an ordinary kill of the master was scheduled")
	}
	f := &faultFake{}
	f.install(t)
	actx, p := genRunCtx(t)
	a := faultAct("50", "0.1")
	a.Params["fault_processes"] = "blockmaster"
	if _, err := v3GenRun(context.Background(), actx, a); err == nil {
		t.Fatal("v3_gen_run accepted fault_processes blockmaster")
	}
	_ = actx.History.Close()
	if n := len(records(t, p)); n != 1 || len(f.log) != 0 {
		t.Fatalf("%d records, log %v before the refusal", n, f.log)
	}
	if _, err := NewGenerator(GenConfig{Seed: 1, Ops: 50, Pool: pool(4), Blocks: 8, Faults: 0.1, FaultProcesses: []string{"blockvolume:0"}}); err != nil {
		t.Fatalf("green: %v", err)
	}
}