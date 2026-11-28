package v3block

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	// m2-gen/10: the processes in turn, the first of each forced.
	want := []FaultEvent{
		{AfterOp: 15, Action: FaultKill, Process: "blockmaster"}, {AfterOp: 20, Action: FaultRestart, Process: "blockmaster"},
		{AfterOp: 28, Action: FaultKill, Process: "blockvolume:0"}, {AfterOp: 41, Action: FaultRestart, Process: "blockvolume:0"},
		{AfterOp: 45, Action: FaultKill, Process: "blockvolume:1"}, {AfterOp: 54, Action: FaultRestart, Process: "blockvolume:1"},
		{AfterOp: 55, Action: FaultKill, Process: "blockmaster"}, {AfterOp: 60, Action: FaultRestart, Process: "blockmaster"},
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
	// alsoDown is a process a look finds down that this run never killed
	// (a holder fenced on its own): looks report it.
	alsoDown string
	// settle: at each normal healthy point, a settled record with this state
	// for every volume whose attach or stop answered unknown since ("": none,
	// they stay unsettled). fenceAll: the first n normal healthy records list
	// every pool name not serving.
	settle    string
	unsettled []string
	fenceAll  int
	pool      []string
	// refuseRestart: every restart answers refused (a holder that does not
	// come back).
	refuseRestart bool
	// opDelay: a write or snapshot takes this long, and answers unknown when
	// a kill ran meanwhile (a kill timed into it); kills counts the kills.
	// storePaused: between a store pause and its continue a snapshot answers
	// unknown (its publish cut). storeCalls logs them.
	opDelay     time.Duration
	noHold      bool // an operation a kill is timed into does not hold its answer: the kill may land after it
	kills       atomic.Int64
	storePaused atomic.Bool
	storeCalls  []string
}

// lookNotServing is what a single look shows not serving: the pool while
// the fenceAll records are in force (a look sees what a healthy record does).
func (f *faultFake) lookNotServing() []string {
	if f.healthyCall >= 1 && f.healthyCall <= f.fenceAll {
		return f.pool
	}
	return []string{}
}

func (f *faultFake) install(t *testing.T) {
	f.fakeProduct.install(t)
	for name, inner := range genHandlers {
		name, inner := name, inner
		if name == "v3_op_observe" || name == "v3_op_read" {
			continue
		}
		genHandlers[name] = func(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
			if f.opDelay > 0 && (name == "v3_op_write" || name == "v3_op_snapshot") && act.Params["expect"] == "" && f.down == "" {
				// The operation takes opDelay between its invoke and its
				// answer, and is cut (unknown) by a kill that ran meanwhile, or
				// while the store is paused.
				k0 := f.kills.Load()
				seq, _ := actx.History.Invoke(tr.HistoryRecord{Op: "X." + strings.TrimPrefix(name, "v3_op_"), Verb: strings.TrimPrefix(name, "v3_op_"), Volume: act.Params["volume"],
					Args: cutArgs(name, act)})
				time.Sleep(f.opDelay)
				if randomrun.KillTimedInto(ctx) && !f.noHold {
					// A kill is timed into this operation: like the product's
					// operation cut by its holder's death, it holds its answer
					// until the kill has landed (or the guard), whatever the
					// machine's timing. A guard expiry answers ok: the test
					// then sees the kill MISSED and fails.
					for guard := time.Now().Add(killLandGuard); f.kills.Load() == k0 && time.Now().Before(guard); {
						time.Sleep(time.Millisecond)
					}
				}
				if f.kills.Load() != k0 || (name == "v3_op_snapshot" && f.storePaused.Load()) {
					_ = actx.History.Complete(seq, tr.OutcomeUnknown, map[string]any{"detail": "fake: cut by a fault"})
					return map[string]string{"value": tr.OutcomeUnknown, "detail": "fake: cut by a fault"}, nil
				}
				_ = actx.History.Complete(seq, tr.OutcomeOK, nil)
				return map[string]string{"value": tr.OutcomeOK}, nil
			}
			if f.down == "" || act.Params["expect"] != "" || (f.inOutage && !f.outageUnknown) {
				return inner(ctx, actx, act)
			}
			f.calls = append(f.calls, act)
			if name == "v3_op_attach" || name == "v3_op_stop" {
				f.unsettled = append(f.unsettled, act.Params["volume"])
			}
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
			if verb == FaultKill {
				f.kills.Add(1) // after its invocation record: an operation it cuts completes after it
			}
			if verb == FaultRestart && f.refuseRestart {
				_ = actx.History.Complete(seq, tr.OutcomeRefused, nil)
				return map[string]string{"value": tr.OutcomeRefused}, nil
			}
			_ = actx.History.Complete(seq, tr.OutcomeOK, nil)
			return map[string]string{"value": tr.OutcomeOK}, nil
		}
	}
	store := func(verb string, paused bool) tr.ActionHandlerFunc {
		return func(_ context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
			f.storeCalls = append(f.storeCalls, verb+" "+act.Params["store"]+" "+act.Params["bound_seconds"])
			args := map[string]any{"fault_key": "store_pause:" + act.Params["store"], "process": act.Params["store"]}
			if b := act.Params["bound_seconds"]; b != "" {
				args["bound_seconds"] = b
			}
			seq, _ := actx.History.Invoke(tr.HistoryRecord{Op: "Env." + verb, Verb: verb, Args: args})
			f.storePaused.Store(paused)
			_ = actx.History.Complete(seq, tr.OutcomeOK, nil)
			return map[string]string{"value": tr.OutcomeOK}, nil
		}
	}
	genHandlers["v3_op_fault_apply_store"] = store("fault_apply", true)
	genHandlers["v3_op_fault_restore_store"] = store("fault_restore", false)
	genHandlers["v3_op_kill"] = fault(FaultKill)
	genHandlers["v3_op_restart"] = fault(FaultRestart)
	genHandlers["v3_op_healthy"] = func(_ context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
		if act.Params["look"] == "single" {
			f.inOutage = f.inOutage || f.down == "blockmaster"
			f.log = append(f.log, "look")
			_ = actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "runner", Verb: "healthy", Result: map[string]any{
				"healthy": f.down == "" && f.alsoDown == "", "down": strings.Fields(f.down + " " + f.alsoDown), "not_serving": f.lookNotServing(), "holders": map[string]any{}, "look": "single", "attempts": 1}})
			return map[string]string{"value": tr.OutcomeOK, "healthy": fmt.Sprint(f.down == "" && f.alsoDown == ""), "down": strings.TrimSpace(f.down + " " + f.alsoDown)}, nil
		}
		f.healthyCall++
		ok := f.down == "" && f.healthyCall > f.unhealthy
		f.log = append(f.log, fmt.Sprintf("healthy %v", ok))
		f.waits = append(f.waits, act.Params["wait_seconds"])
		ns := []string{}
		if f.healthyCall <= f.fenceAll {
			ns = f.pool
		}
		_ = actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "runner", Verb: "healthy", Result: map[string]any{"healthy": ok, "down": []string{}, "not_serving": ns}})
		if ok && f.settle != "" {
			for _, v := range f.unsettled {
				_ = actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "runner", Verb: "settled", Result: map[string]any{"volume": v, "state": f.settle, "evidence": "fake"}})
			}
			f.unsettled = nil
		}
		return map[string]string{"value": tr.OutcomeOK, "healthy": fmt.Sprint(ok)}, nil
	}
}

func faultAct(ops, rate string) tr.Action {
	a := genAct(ops)
	a.Params["pool"] = strings.Join(pool(200), " ") // not exhausted before the budget (this fake answers unknown while a process is down; every listed process is now killed)
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
	// Each event ran after its operation (the last gen_step before it has its
	// after_op), on the process its fault_kill record resolved; a kill the
	// record skipped ran neither, nor did its restart.
	lastStep, ev, ran := 0, 0, 0
	target, skipped := "", false
	for _, r := range recs {
		if r.Verb == "gen_step" {
			lastStep = int(r.Result["opno"].(float64))
		}
		for ev < len(want.Events) && skipped && want.Events[ev].Action == FaultRestart {
			ev, skipped = ev+1, false // the skipped kill's restart
		}
		if r.Verb == "fault_kill" {
			e := want.Events[ev]
			if e.Action != FaultKill || r.Result["scheduled"] != e.Process || lastStep != e.AfterOp {
				t.Fatalf("decision %+v for scheduled %+v after op %d", r.Result, e, lastStep)
			}
			target, skipped = r.Result["process"].(string), r.Result["skipped"] == true
			if skipped {
				ev++
			}
		}
		if r.Kind == tr.HistoryInvoke && (r.Verb == FaultKill || r.Verb == FaultRestart) {
			e := want.Events[ev]
			if r.Verb != e.Action || r.Args["process"] != target || lastStep != e.AfterOp {
				t.Fatalf("event %d ran as %s %v after op %d, scheduled %+v, resolved %s", ev, r.Verb, r.Args, lastStep, e, target)
			}
			ev++
			ran++
		}
	}
	if ran == 0 || ev != len(want.Events) || f.maxDown != 1 || f.down != "" {
		t.Fatalf("%d of %d events done (%d ran), max down %d, down at end %q", ev, len(want.Events), ran, f.maxDown, f.down)
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

// F7: a healthy point still false after its full wait ends the run: the
// record gen_stop, then finish, the sweep and the end; no operation after
// it. Nothing is resolved at the false point.
func TestGenRunEndsAtAHealthyPointStillFalse(t *testing.T) {
	f := &faultFake{unhealthy: 1}
	f.install(t)
	actx, p := genRunCtx(t)
	out, err := v3GenRun(context.Background(), actx, faultAct("300", "0.05"))
	if err != nil || out["value"] != "unhealthy" {
		t.Fatalf("%v %v", out, err)
	}
	_ = actx.History.Close()
	recs := records(t, p)
	stop := -1
	for i, r := range recs {
		if stop < 0 && r.Verb == "healthy" && r.Result["look"] == nil && r.Result["healthy"] == false {
			if n := recs[i+1]; n.Verb != "gen_stop" || n.Result["why"] == nil {
				t.Fatalf("the false healthy point is followed by %+v, not gen_stop", n)
			}
			stop = i + 1
		}
		if stop >= 0 && r.Verb == "gen_step" {
			t.Fatalf("an operation after the stop: %+v", r.Result)
		}
	}
	if stop < 0 {
		t.Fatal("no false healthy point")
	}
	if left := unresolvedUnknowns(recs); len(left) != 0 {
		t.Fatalf("unknowns not resolved at the end: %v", left)
	}
	if lastVerb(t, p, "gen_sweep").Seq < recs[stop].Seq || lastVerb(t, p, "gen_coverage").Result["ended"] != "unhealthy" {
		t.Fatal("no sweep after the stop, or coverage does not say unhealthy")
	}
	// The replay stops at the same point.
	f.healthyCall = 0
	if out, b, err := replayInto(t, p); err != nil || out["value"] != "match" || lastVerb(t, b, "gen_stop").Result["after_op"] != recs[stop].Result["after_op"] {
		t.Fatalf("replay: %v %v", out, err)
	}
	// Red control: a replay that meets a false healthy point the record did
	// not is a difference (the steps it no longer runs).
	f2 := &faultFake{}
	f2.install(t)
	actx2, a := genRunCtx(t)
	if out, err := v3GenRun(context.Background(), actx2, faultAct("300", "0.05")); err != nil || out["value"] != "ops" {
		t.Fatalf("%v %v", out, err)
	}
	_ = actx2.History.Close()
	f2.unhealthy, f2.healthyCall = 1, 0
	if _, _, err := replayInto(t, a); err == nil || !strings.Contains(err.Error(), "not run") {
		t.Fatalf("a replay stopped early matched: %v", err)
	}
}
func TestGenRunWithFaultsEndsHealthyWhenTheRunEndsWhileDown(t *testing.T) {
	f := &faultFake{}
	f.install(t)
	actx, p := genRunCtx(t)
	a := faultAct("1000", "1")
	a.Params["pool"] = "g1 g2 g3"
	a.Params["weights"] = "v3_op_create=1 v3_op_attach=1" // holders to kill, then nothing ordinary left
	a.Params["fault_restart_within"] = "1"                // kills often enough to meet a holder
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
	// planned: the writes each outage's marker listed, by its kill's
	// complete seq (no serving volume: no write to make).
	planned, listed := map[uint64]int{}, -1
	for _, r := range recs {
		if r.Verb == "master_outage" {
			w, _ := r.Result["writes"].([]any)
			listed = len(w)
		}
		if r.Kind == tr.HistoryComplete && open[r.Of].Verb == FaultKill && listed >= 0 {
			planned[r.Seq], listed = listed, -1
		}
		switch r.Kind {
		case tr.HistoryInvoke:
			open[r.Seq] = r
		case tr.HistoryComplete:
			inv := open[r.Of]
			switch {
			case inv.Verb == FaultKill && inv.Args["process"] == "blockmaster" && r.Outcome == tr.OutcomeOK:
				kill, writes, looks = r.Seq, 0, 0
			case inv.Verb == FaultRestart && inv.Args["process"] == "blockmaster" && kill != 0 && r.Outcome == tr.OutcomeOK:
				if (writes == 0 && planned[kill] > 0) || looks == 0 {
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

// F6: "one down at a time" is checked against what is observed. A single
// look before each kill; when it shows anything down, the kill and its
// restart are skipped and the decision is recorded; a replay follows the
// recorded decisions.
func TestKillSkippedWhileALookShowsSomethingDown(t *testing.T) {
	kills := func(log []string) int {
		n := 0
		for _, l := range log {
			n += btoi(strings.HasPrefix(l, FaultKill+" ") || strings.HasPrefix(l, FaultRestart+" "))
		}
		return n
	}
	// Green: nothing else down, every scheduled kill runs, each after a look.
	f := &faultFake{}
	f.install(t)
	actx, a := genRunCtx(t)
	if _, err := v3GenRun(context.Background(), actx, faultAct("300", "0.05")); err != nil {
		t.Fatal(err)
	}
	_ = actx.History.Close()
	recs := records(t, a)
	decided, held, ran := 0, 0, kills(f.log)
	for i, r := range recs {
		if r.Verb != "fault_kill" {
			continue
		}
		decided++
		if recs[i-1].Verb != "healthy" || recs[i-1].Result["look"] != "single" {
			t.Fatalf("kill decision at %d without its look: %+v", i, recs[i-1])
		}
		if r.Result["why"] == "nothing holds" {
			continue // F10: no target
		}
		held++
		if r.Result["skipped"] != false || recs[i+1].Kind != tr.HistoryInvoke || recs[i+1].Verb != FaultKill {
			t.Fatalf("kill decision at %d: %+v, then %+v", i, r.Result, recs[i+1])
		}
	}
	if held == 0 || ran != 2*held {
		t.Fatalf("%d decisions with a target, %d kills and restarts ran", held, ran)
	}
	// The replay follows the recorded decisions, whatever its own looks show.
	f.alsoDown = "blockvolume:7"
	f.log = nil
	if out, _, err := replayInto(t, a); err != nil || out["value"] != "match" || kills(f.log) != ran {
		t.Fatalf("replay of the kills: %v %v, log %v", out, err, f.log)
	} // Red: a holder the run never killed is down: no kill and no restart runs.
	f2 := &faultFake{alsoDown: "blockvolume:7"}
	f2.install(t)
	actx2, b := genRunCtx(t)
	if _, err := v3GenRun(context.Background(), actx2, faultAct("300", "0.05")); err != nil {
		t.Fatal(err)
	}
	_ = actx2.History.Close()
	if n := kills(f2.log); n != 0 {
		t.Fatalf("%d kills and restarts ran while a look showed blockvolume:7 down: %v", n, f2.log)
	}
	skips := 0
	for _, r := range records(t, b) {
		if r.Verb == "fault_kill" {
			d, _ := r.Result["down"].([]any)
			if r.Result["skipped"] != true || (r.Result["why"] != "nothing holds" && (len(d) != 1 || d[0] != "blockvolume:7")) {
				t.Fatalf("decision %+v", r.Result)
			}
			skips++
		}
	}
	if skips != decided {
		t.Fatalf("%d skips, %d scheduled kills", skips, decided)
	}
	f2.alsoDown = ""
	f2.log = nil
	if out, _, err := replayInto(t, b); err != nil || out["value"] != "match" || kills(f2.log) != 0 {
		t.Fatalf("replay of the skips: %v %v, log %v", out, err, f2.log)
	}

}

// The run hands the model the records its handlers write: a volume whose
// attach or stop answered unknown is offered nothing until a settled record
// places it; a healthy record's not_serving fences reads and writes.
func TestGenRunLearnsFromSettledAndHealthyRecords(t *testing.T) {
	type window struct{ from, to uint64 }
	run := func(f *faultFake) []tr.HistoryRecord {
		f.install(t)
		actx, p := genRunCtx(t)
		if _, err := v3GenRun(context.Background(), actx, faultAct("300", "0.05")); err != nil {
			t.Fatal(err)
		}
		_ = actx.History.Close()
		return records(t, p)
	}
	// unsettledSteps: steps on a volume after its unknown attach or stop
	// and before its settled record (none may exist), and the volumes that
	// had an operation after their settled record.
	check := func(recs []tr.HistoryRecord) (bad []string, served int) {
		since := map[string]bool{}
		settledOnce := map[string]bool{}
		var stepVol, stepAct string
		for _, r := range recs {
			switch {
			case r.Verb == "gen_step":
				stepVol, stepAct = r.Volume, r.Result["action"].(string)
				if since[stepVol] && stepAct != "v3_op_observe" { // observation is all an unsettled volume is offered
					bad = append(bad, fmt.Sprintf("%s %s while unsettled", stepAct, stepVol))
				}
				if settledOnce[stepVol] && r.Result["negative"] == nil {
					served++
					delete(settledOnce, stepVol)
				}
			case r.Kind == tr.HistoryComplete && r.Outcome == tr.OutcomeUnknown && r.Volume == stepVol && (stepAct == "v3_op_attach" || stepAct == "v3_op_stop") && (r.Verb == "attach" || r.Verb == "stop"):
				since[stepVol] = true
			case r.Verb == "settled":
				v := r.Result["volume"].(string)
				delete(since, v)
				settledOnce[v] = true
			case r.Verb == "gen_sweep":
				stepVol = ""
			}
		}
		return bad, served
	}
	recs := run(&faultFake{settle: "held"})
	bad, served := check(recs)
	if len(bad) != 0 || served == 0 {
		t.Fatalf("settled held: %v, %d volumes served again after their settled record", bad, served)
	}
	// Red control: without settled records the volumes stay unsettled.
	bad, served = check(run(&faultFake{}))
	if len(bad) != 0 || served != 0 {
		t.Fatalf("no settled records: %v, %d served", bad, served)
	}
	// not_serving: no write or read between the fencing record and the next
	// healthy record that leaves the volumes out.
	// The volumes written before it (serving then) and not attached again
	// since are the fenced ones.
	recs = run(&faultFake{fenceAll: 1, pool: pool(100)})
	fenced, wrote := map[string]bool{}, map[string]bool{}
	n, during := 0, 0
	inWindow := false
	for _, r := range recs {
		if r.Verb == "healthy" && r.Result["look"] == nil {
			ns, _ := r.Result["not_serving"].([]any)
			inWindow = len(ns) > 0
			if inWindow {
				n++
				for v := range wrote {
					fenced[v] = true
				}
			} else {
				fenced = map[string]bool{}
			}
		}
		if r.Verb != "gen_step" {
			continue
		}
		a := r.Result["action"]
		switch {
		case a == "v3_op_attach":
			delete(fenced, r.Volume)
		case (a == "v3_op_write" || a == "v3_op_read") && fenced[r.Volume]:
			t.Fatalf("%v on %s while not serving", a, r.Volume)
		case a == "v3_op_write":
			wrote[r.Volume] = true
		}
		if inWindow {
			during++
		}
	}
	if n != 1 || during == 0 {
		t.Fatalf("%d fencing records, %d steps in the window", n, during)
	}
	_ = window{}
}

// F10: a scheduled holder kill resolves at kill time to a volume that holds
// (serving per the model), recorded in its fault_kill record and in the
// kill's args; with nothing holding, the kill is skipped.
func TestKillTargetsAVolumeThatHolds(t *testing.T) {
	f := &faultFake{}
	f.install(t)
	actx, p := genRunCtx(t)
	if _, err := v3GenRun(context.Background(), actx, faultAct("300", "0.05")); err != nil {
		t.Fatal(err)
	}
	_ = actx.History.Close()
	names := pool(100)
	serving := map[string]bool{}
	var step map[string]any
	var stepVol string
	resolved, unresolvedHolds := 0, 0
	for _, r := range records(t, p) {
		switch {
		case r.Verb == "gen_step":
			step, stepVol = r.Result, r.Volume
		case r.Kind == tr.HistoryComplete && step != nil && r.Outcome == tr.OutcomeOK && step["negative"] == nil && r.Volume == stepVol:
			switch step["action"] {
			case "v3_op_attach":
				serving[stepVol] = true
			case "v3_op_stop", "v3_op_move", "v3_op_delete":
				serving[stepVol] = false
			}
		case r.Verb == "fault_kill" && r.Result["skipped"] == false:
			proc := r.Result["process"].(string)
			k, err := strconv.Atoi(strings.TrimPrefix(proc, "blockvolume:"))
			if err != nil || !serving[names[k]] {
				t.Fatalf("kill resolved to %s (%v), which does not hold", proc, err)
			}
			if sch := r.Result["scheduled"].(string); sch != proc {
				resolved++
			}
			// The scheduled name taken literally would often name no holder.
			if k2, _ := strconv.Atoi(strings.TrimPrefix(r.Result["scheduled"].(string), "blockvolume:")); !serving[names[k2]] {
				unresolvedHolds++
			}
		}
	}
	if resolved == 0 || unresolvedHolds == 0 {
		t.Fatalf("%d kills resolved away from their scheduled name, %d whose scheduled name held nothing", resolved, unresolvedHolds)
	}
	// Nothing holds: the model names no target.
	g, _ := NewGenerator(GenConfig{Seed: 1, Ops: 10, Pool: pool(3), Blocks: 8})
	if _, ok := g.m.KillTarget("blockvolume:0"); ok {
		t.Fatal("a target with nothing holding")
	}
	drive(g, op("v3_op_create", "g2"), op("v3_op_attach", "g2", "host", "0"), op("v3_op_create", "g3"), op("v3_op_attach", "g3", "host", "0"))
	for sch, want := range map[string]string{"blockvolume:0": "blockvolume:1", "blockvolume:1": "blockvolume:2", "blockvolume:2": "blockvolume:1"} {
		if got, ok := g.m.KillTarget(sch); !ok || got != want {
			t.Errorf("%s resolved to %s %v, want %s", sch, got, ok, want)
		}
	}
	g.m.Observed(healthyRec([]string{"g2"}))
	if got, _ := g.m.KillTarget("blockvolume:0"); got != "blockvolume:2" {
		t.Errorf("a fenced holder is a target: %s", got)
	}
}

// A master outage with no volume serving is skipped and recorded
// ("skipped": "nothing serving"), and a replay skips it too; with one
// volume serving it runs.
func TestMasterOutageSkippedWhenNothingServes(t *testing.T) {
	masterKills := func(log []string) int {
		n := 0
		for _, l := range log {
			n += btoi(l == FaultKill+" blockmaster")
		}
		return n
	}
	outages := func(p string) (skipped, ran int) {
		for _, r := range records(t, p) {
			if r.Verb != "master_outage" {
				continue
			}
			if r.Result["skipped"] == randomrun.OutageNothingServing {
				w, _ := r.Result["writes"].([]any)
				if len(w) != 0 {
					t.Fatalf("a skipped outage with writes: %v", r.Result)
				}
				skipped++
			} else {
				ran++
			}
		}
		return skipped, ran
	}
	for name, c := range map[string]struct {
		pool, weights string
		skips         bool
	}{
		"nothing serving": {"g1 g2 g3 g4 g5 g6 g7 g8", "v3_op_create=1", true},
		"one serving":     {"g1", "v3_op_create=1 v3_op_attach=1 v3_op_write=1", false},
	} {
		t.Run(name, func(t *testing.T) {
			f := &faultFake{}
			f.install(t)
			actx, p := genRunCtx(t)
			a := outageAct("40")
			a.Params["pool"], a.Params["weights"], a.Params["fault_master_outage"] = c.pool, c.weights, "0.5"
			if _, err := v3GenRun(context.Background(), actx, a); err != nil {
				t.Fatal(err)
			}
			_ = actx.History.Close()
			skipped, ran := outages(p)
			kills := masterKills(f.log)
			if c.skips && (skipped == 0 || ran != 0 || kills != 0) {
				t.Fatalf("nothing serving: %d skipped, %d ran, %d master kills", skipped, ran, kills)
			}
			if !c.skips && (ran == 0 || kills != ran) {
				t.Fatalf("one serving: %d skipped, %d ran, %d master kills", skipped, ran, kills)
			}
			f.log = nil
			out, b, err := replayInto(t, p)
			if err != nil || out["value"] != "match" || masterKills(f.log) != kills {
				t.Fatalf("replay: %v %v, %d master kills", out, err, masterKills(f.log))
			}
			if s2, r2 := outages(b); s2 != skipped || r2 != ran {
				t.Fatalf("replay outages %d skipped %d ran, recorded %d %d", s2, r2, skipped, ran)
			}
		})
	}
}

// M2 run 2: after the run killed a volume's holder, nothing but
// observation is proposed for it (no write, read, snapshot, delete, no
// negative either) until its restart answers ok and a healthy record shows
// it serving; a restart answered otherwise keeps it out for good.
func TestNothingProposedAtAKilledHolder(t *testing.T) {
	// deadSteps lists steps on a volume between its holder's kill and the
	// first healthy-true record after its restart answered ok ("out"
	// forever when the restart did not answer ok), and counts the kills.
	deadSteps := func(recs []tr.HistoryRecord) (bad []string, kills, back int) {
		names := pool(100)
		dead := map[string]string{} // volume -> "killed", "restarted", "out"
		open := map[uint64]tr.HistoryRecord{}
		for _, r := range recs {
			switch {
			case r.Kind == tr.HistoryInvoke:
				open[r.Seq] = r
			case r.Kind == tr.HistoryComplete && (open[r.Of].Verb == FaultKill || open[r.Of].Verb == FaultRestart):
				inv := open[r.Of]
				k, err := strconv.Atoi(strings.TrimPrefix(inv.Args["process"].(string), "blockvolume:"))
				if err != nil {
					continue
				}
				v := names[k]
				switch {
				case inv.Verb == FaultKill && r.Outcome == tr.OutcomeOK:
					dead[v] = "killed"
					kills++
				case inv.Verb == FaultRestart && dead[v] == "killed" && r.Outcome == tr.OutcomeOK:
					dead[v] = "restarted"
				case inv.Verb == FaultRestart && dead[v] == "killed":
					dead[v] = "out"
				}
			case r.Verb == "healthy" && r.Result["healthy"] == true:
				for v, s := range dead {
					if s == "restarted" {
						delete(dead, v)
						back++
					}
				}
			case r.Verb == "gen_step" && dead[r.Volume] != "" && r.Result["action"] != "v3_op_observe":
				bad = append(bad, fmt.Sprintf("%v on %s (%s)", r.Result["action"], r.Volume, dead[r.Volume]))
			}
		}
		return bad, kills, back
	}
	run := func(f *faultFake) []tr.HistoryRecord {
		f.install(t)
		actx, p := genRunCtx(t)
		a := faultAct("300", "0.05")
		a.Params["fault_processes"] = "blockvolume:0 blockvolume:1 blockvolume:2"
		if _, err := v3GenRun(context.Background(), actx, a); err != nil {
			t.Fatal(err)
		}
		_ = actx.History.Close()
		return records(t, p)
	}
	bad, kills, back := deadSteps(run(&faultFake{}))
	if len(bad) != 0 || kills == 0 || back == 0 {
		t.Fatalf("restarts ok: %d kills, %d back, steps at a dead holder %v", kills, back, bad)
	}
	bad, kills, back = deadSteps(run(&faultFake{refuseRestart: true}))
	if len(bad) != 0 || kills == 0 || back != 0 {
		t.Fatalf("restarts refused: %d kills, %d back, steps at a dead holder %v", kills, back, bad)
	}
	// The model itself: killed, restarted, then a healthy record.
	g, _ := NewGenerator(GenConfig{Seed: 1, Ops: 10, Pool: pool(3), Blocks: 8})
	drive(g, op("v3_op_create", "g1"), op("v3_op_attach", "g1", "host", "0"), op("v3_op_write", "g1", "lbas", "1", "round", "r"))
	g.m.FaultRan(FaultKill, "blockvolume:0", tr.OutcomeOK)
	if a := offered(g, "g1"); len(a) != 0 {
		t.Fatalf("offered at a killed holder: %v", a)
	}
	if _, ok := g.m.KillTarget("blockvolume:0"); ok {
		t.Fatal("a killed holder is a kill target")
	}
	for _, o := range g.m.Sweep() {
		if o.Volume == "g1" && o.Action != "v3_op_observe" {
			t.Fatalf("the sweep %s a killed holder", o.Action)
		}
	}
	g.m.FaultRan(FaultRestart, "blockvolume:0", tr.OutcomeOK)
	if len(offered(g, "g1")) != 0 {
		t.Fatal("offered after the restart, before a healthy record")
	}
	g.m.Observed(healthyRec(nil))
	if a := offered(g, "g1"); !a["v3_op_write"] || !a["v3_op_delete"] {
		t.Fatalf("not offered again after restart and healthy: %v", a)
	}
	g.m.FaultRan(FaultKill, "blockvolume:0", tr.OutcomeOK)
	g.m.FaultRan(FaultRestart, "blockvolume:0", tr.OutcomeUnknown)
	g.m.Observed(healthyRec(nil))
	if g.vols["g1"].state != stUnknown || len(offered(g, "g1")) != 0 {
		t.Fatalf("an unknown restart: %q %v", g.vols["g1"].state, offered(g, "g1"))
	}
}

// Every holder of the fault processes is killed and restarted BY
// CONSTRUCTION: for the nightly's seeds (the generic schedule kills the
// processes in turn, the first of each forced), a run over four holders
// reaches kill|blockvolume:<i> and restart|blockvolume:<i> for every i in
// its fault coverage.
func TestEveryFaultProcessIsReached(t *testing.T) {
	for _, seed := range []string{"1", "5", "7"} {
		f := &faultFake{}
		f.install(t)
		actx, p := genRunCtx(t)
		act := faultAct("300", "0.05")
		act.Params["seed"] = seed
		act.Params["fault_processes"] = "blockvolume:0 blockvolume:1 blockvolume:2 blockvolume:3"
		if _, err := v3GenRun(context.Background(), actx, act); err != nil {
			t.Fatal(err)
		}
		_ = actx.History.Close()
		var cells map[string]any
		for _, r := range records(t, p) {
			if r.Verb == "gen_coverage" {
				cells, _ = r.Result["fault_cells"].(map[string]any)
			}
		}
		for i := 0; i < 4; i++ {
			for _, a := range []string{"kill", "restart"} {
				if c := fmt.Sprintf("%s|blockvolume:%d", a, i); cells[c] == nil || cells[c].(float64) == 0 {
					t.Errorf("seed %s: %s never reached: %v", seed, c, cells)
				}
			}
		}
	}
}

// cutArgs are the record args of an operation a fault cut in this fake: a
// snapshot's name, request and attempt (as the declared snapshot records
// them), a write's lbas.
func cutArgs(name string, act tr.Action) map[string]any {
	if name == "v3_op_write" {
		return map[string]any{"lbas": act.Params["lbas"]}
	}
	n, _ := strconv.Atoi(act.Params["attempt"])
	if n == 0 {
		n = 1
	}
	return map[string]any{"name": act.Params["name"], "request": act.Params["volume"] + "-" + act.Params["name"], "attempt": n}
}
