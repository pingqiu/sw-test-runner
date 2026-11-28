package randomrun

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

// storeDriver is the toy driver with a store fault.
type storeDriver struct{ toyDriver }

func (storeDriver) StorePause(e FaultEvent, bound int) (Op, Op) {
	return Op{Action: "pause", Params: map[string]string{"store": e.Process}}, Op{Action: "cont", Params: map[string]string{"store": e.Process}}
}

// inOpRunner is a Runner over toy handlers: "write" and "snapshot" take
// opTime and answer unknown when a kill landed while they ran (ok
// otherwise; "snapshot" answers snapAnswer when set); "kill" and "restart"
// record their invoke and complete; the healthy handler answers healthy
// and nothing down; "pause" and "cont" are a store fault's apply and
// restore.
type inOpRunner struct {
	mu         sync.Mutex
	killed     bool
	opTime     time.Duration
	snapAnswer string
	paused     bool
	preInvoke  time.Duration // a write waits this long before recording its invocation (a handler's own checks)
}

func (x *inOpRunner) runner(d Driver) Runner {
	rec := func(verb string, out func() string) tr.ActionHandlerFunc {
		return func(_ context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
			if verb == "write" {
				time.Sleep(x.preInvoke)
			}
			args := map[string]any{}
			if verb == VerbFaultApply || verb == VerbFaultRestore {
				args["fault_key"] = "store_pause:" + act.Params["store"]
			}
			seq, err := actx.History.Invoke(tr.HistoryRecord{Op: "T." + verb, Verb: verb, Volume: act.Params["volume"], Args: args})
			if err != nil {
				return nil, err
			}
			o := out()
			return map[string]string{"value": o}, actx.History.Complete(seq, o, nil)
		}
	}
	op := func(verb string) tr.ActionHandlerFunc {
		return rec(verb, func() string {
			x.mu.Lock()
			x.killed = false
			x.mu.Unlock()
			time.Sleep(x.opTime)
			x.mu.Lock()
			defer x.mu.Unlock()
			if verb == "snapshot" && x.snapAnswer != "" {
				return x.snapAnswer
			}
			if x.killed || (verb == "snapshot" && x.paused) {
				return tr.OutcomeUnknown
			}
			return tr.OutcomeOK
		})
	}
	ok := func() string { return tr.OutcomeOK }
	return Runner{Name: "t", Node: "n", Driver: d, Handlers: map[string]tr.ActionHandlerFunc{
		"write": op("write"), "snapshot": op("snapshot"),
		// The kill lands when its handler is called, before its own record:
		// an operation that checks after that answers unknown, and its
		// completion record waits for the kill's invocation record (one
		// history), so the records say hit. Set inside out() instead, a slow
		// write of the kill's record (a slow disk's fsync) could let the
		// operation answer ok after the kill's invocation: a flaky fake.
		"kill": func(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
			x.mu.Lock()
			x.killed = true
			x.mu.Unlock()
			return rec(VerbKill, ok)(ctx, actx, act)
		},
		"restart": rec(VerbRestart, ok),
		"pause":   rec(VerbFaultApply, func() string { x.paused = true; return tr.OutcomeOK }),
		"cont":    rec(VerbFaultRestore, func() string { x.paused = false; return tr.OutcomeOK }),
		"healthy": func(_ context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
			if act.Params["look"] != "" {
				return map[string]string{"value": "ok", DownKey: ""}, nil
			}
			return map[string]string{"value": "ok", HealthyKey: HealthyTrue}, actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Verb: VerbHealthy, Result: map[string]any{"healthy": true}})
		},
		"observe": func(_ context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
			return map[string]string{"value": "observed"}, actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Verb: VerbObserve, Volume: act.Params["volume"]})
		},
	}}
}

// inOpShape is a history's records, without the clock: "invoke verb",
// "complete verb outcome", "observe verb"; and the records.
func inOpShape(t *testing.T, p string) ([]string, []tr.HistoryRecord) {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var all []tr.HistoryRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r tr.HistoryRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		all = append(all, r)
	}
	var out []string
	verbOf := map[uint64]string{}
	for _, r := range all {
		switch r.Kind {
		case tr.HistoryInvoke:
			verbOf[r.Seq] = r.Verb
			out = append(out, "invoke "+r.Verb)
		case tr.HistoryComplete:
			out = append(out, "complete "+verbOf[r.Of]+" "+r.Outcome)
		case tr.HistoryObserve:
			out = append(out, "observe "+r.Verb)
		}
	}
	return out, all
}

// A kill timed INTO an operation lands inside it (after the operation's
// invoke, before its completion): the request ends unknown, the record
// "fault_during" says hit and when the kill ran. Red control: an offset
// past the operation's duration is MISSED (the kill after the completion),
// recorded so, and the operation answered ok.
func TestKillDuringLandsInsideTheOperation(t *testing.T) {
	for name, c := range map[string]struct {
		offset  int
		outcome string
		hit     bool
		shape   []string
	}{
		"inside": {50, tr.OutcomeUnknown, true, []string{"observe gen_step", "invoke write", "invoke kill", "complete kill ok", "complete write unknown", "observe fault_during"}},
		"after":  {600, tr.OutcomeOK, false, []string{"observe gen_step", "invoke write", "complete write ok", "invoke kill", "complete kill ok", "observe fault_during"}},
	} {
		x := &inOpRunner{opTime: 300 * time.Millisecond}
		r := x.runner(toyDriver{})
		actx, p := rerunHistory(t)
		e := FaultEvent{AfterOp: 0, Action: FaultKillDuring, Process: "p0", Target: "write", Permille: 500}
		fr := newFaultRun(r, []FaultEvent{e}, nil, map[string]any{}, nil)
		fr.inOpRecs = map[int]inOpRecord{1: {Action: FaultKillDuring, AfterOp: 0, OpNo: 1, Process: "p0", OffsetMs: c.offset}}
		fr.arm(e)
		got, err := fr.stepOp(context.Background(), actx, r, Op{No: 1, Action: "write", Volume: "v1", Params: map[string]string{"volume": "v1"}})
		actx.History.Close()
		shape, recs := inOpShape(t, p)
		if err != nil || got != c.outcome || strings.Join(shape, "|") != strings.Join(c.shape, "|") {
			t.Fatalf("%s: %q %v\n%s", name, got, err, strings.Join(shape, "\n"))
		}
		var last tr.HistoryRecord
		for _, r := range recs {
			if r.Verb == "fault_during" {
				last = r
			}
		}
		if last.Verb != "fault_during" || last.Result["hit"] != c.hit || last.Result["offset_ms"] != float64(c.offset) ||
			(!c.hit && !strings.HasPrefix(last.Result["why"].(string), "MISSED: the kill ran after the operation completed")) {
			t.Fatalf("%s: record %+v", name, last.Result)
		}
		if fr.down != "p0" || len(fr.armed) != 0 {
			t.Fatalf("%s: down %q, armed %v", name, fr.down, fr.armed)
		}
	}
}

// The only concurrency is one fault beside one operation: a second
// operation while one runs is refused (red control: two at once).
func TestTwoOperationsAtOnceAreRefused(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	r := Runner{Name: "t", Driver: toyDriver{}, Handlers: map[string]tr.ActionHandlerFunc{
		"write": func(context.Context, *tr.ActionContext, tr.Action) (map[string]string, error) {
			close(started)
			<-release
			return map[string]string{"value": tr.OutcomeOK}, nil
		},
		"read": func(context.Context, *tr.ActionContext, tr.Action) (map[string]string, error) {
			return map[string]string{"value": tr.OutcomeOK}, nil
		},
	}}
	actx, _ := rerunHistory(t)
	defer actx.History.Close()
	done := make(chan error)
	go func() {
		_, err := r.Exec(context.Background(), actx, Op{Action: "write", Volume: "v1", Params: map[string]string{}})
		done <- err
	}()
	<-started
	_, err := r.Exec(context.Background(), actx, Op{Action: "read", Volume: "v1", Params: map[string]string{}})
	if !errors.Is(err, errTwoOperations) {
		t.Fatalf("a second operation at once: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// One after the other is fine.
	if _, err := r.Exec(context.Background(), actx, Op{Action: "read", Volume: "v1", Params: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
}

// A store pause wraps the operation: apply, the operation (unknown while
// the store is paused), restore; then a healthy point that resolves the
// unknown by observation, the "store_pause" record (hit), and the follow-up
// request. Red control: an operation that answers ok through the pause is
// MISSED.
func TestStorePauseAroundTheOperation(t *testing.T) {
	for name, c := range map[string]struct {
		answer string
		hit    bool
	}{
		"paused, unknown": {"", true},
		"a late ok":       {tr.OutcomeOK, false},
	} {
		x := &inOpRunner{snapAnswer: c.answer}
		r := x.runner(storeDriver{})
		actx, p := rerunHistory(t)
		e := FaultEvent{AfterOp: 0, Action: FaultStorePause, Process: "store", Target: "snapshot"}
		fr := newFaultRun(r, []FaultEvent{e}, nil, map[string]any{"fault_store_bound": 75}, nil)
		follow := Op{Action: "write", Volume: "v1", Params: map[string]string{"volume": "v1"}}
		fr.inOpRecs = map[int]inOpRecord{1: {Action: FaultStorePause, AfterOp: 0, OpNo: 1, Followups: []Op{follow}}}
		fr.arm(e)
		o := Op{No: 1, Action: "snapshot", Volume: "v1", Params: map[string]string{"volume": "v1"}}
		got, err := fr.stepOp(context.Background(), actx, r, o)
		if err == nil {
			fr.note(o, got)
			err = fr.afterStep(context.Background(), actx)
		}
		actx.History.Close()
		shape, recs := inOpShape(t, p)
		want := []string{"observe gen_step", "invoke fault_apply", "complete fault_apply ok", "invoke snapshot", "complete snapshot " + map[bool]string{true: "unknown", false: "ok"}[c.hit],
			"invoke fault_restore", "complete fault_restore ok", "observe healthy"}
		if c.hit {
			want = append(want, "observe observe") // the resolution of the unknown
		}
		want = append(want, "observe store_pause", "invoke write", "complete write ok")
		if err != nil || strings.Join(shape, "|") != strings.Join(want, "|") {
			t.Fatalf("%s: %q %v\n%s", name, got, err, strings.Join(shape, "\n"))
		}
		var rec tr.HistoryRecord
		for _, r := range recs {
			if r.Verb == "store_pause" {
				rec = r
			}
		}
		if rec.Result["hit"] != c.hit || rec.Result["bound_seconds"] != float64(75) || (!c.hit && !strings.HasPrefix(rec.Result["why"].(string), "MISSED")) {
			t.Fatalf("%s: record %+v", name, rec.Result)
		}
	}
}

// The in-operation faults are placed BY CONSTRUCTION: one kill_during per
// target action and one store_pause in every schedule, outside every kill
// window, whatever the seed; the schedule passes its checker. Red controls:
// a kill_during without its target or permille, and a store pause while a
// process is down, are refused.
func TestInOpFaultsArePlacedByConstruction(t *testing.T) {
	in := InOpFaults{During: []string{"write", "snapshot"}, Store: "store", StoreAction: "snapshot"}
	for seed := uint64(1); seed <= 300; seed++ {
		s, err := NewFaultScheduleIn(seed, 300, faultProcs, 0.05, 20, in, testProcess)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		n := map[string]int{}
		killed := map[string]bool{}
		for _, e := range s.Events {
			n[e.Action+"|"+e.Target]++
			if e.Action == FaultKill {
				killed[e.Process] = true
			}
		}
		if len(killed) != len(faultProcs) {
			t.Fatalf("seed %d: the ordinary kills around the windows reach %v", seed, killed)
		}
		if n["kill_during|write"] != 1 || n["kill_during|snapshot"] != 1 || n["store_pause|snapshot"] != 1 {
			t.Fatalf("seed %d: %v", seed, n)
		}
		if err := CheckFaultSchedule(s.Events, 300, 20, testProcess); err != nil {
			t.Fatalf("seed %d: %v\n%v", seed, err, s.Events)
		}
	}
	for name, ev := range map[string][]FaultEvent{
		"kill_during without a target": {{AfterOp: 2, Action: FaultKillDuring, Process: "p0", Permille: 500}, {AfterOp: 5, Action: FaultRestart, Process: "p0"}},
		"kill_during without permille": {{AfterOp: 2, Action: FaultKillDuring, Process: "p0", Target: "write"}, {AfterOp: 5, Action: FaultRestart, Process: "p0"}},
		"a store pause while down":     {{AfterOp: 2, Action: FaultKill, Process: "p0"}, {AfterOp: 3, Action: FaultStorePause, Process: "store", Target: "snapshot"}, {AfterOp: 5, Action: FaultRestart, Process: "p0"}},
	} {
		if err := CheckFaultSchedule(ev, 30, 20, testProcess); err == nil {
			t.Errorf("red control %q not caught", name)
		}
	}
}

// ReadScript folds an in-operation step as its operation alone: the kill
// beside the open operation, and a store fault's apply and restore, are
// not the step's outcome and do not end it; the records are read for the
// replay.
func TestReadScriptReadsInOpSteps(t *testing.T) {
	x := &inOpRunner{opTime: 200 * time.Millisecond}
	r := x.runner(storeDriver{})
	actx, p := rerunHistory(t)
	_ = actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: "gen_header", Result: map[string]any{"generator": toyDriver{}.Version(true), "seed": "1", "ops": 2, "fault_restart_within": 20}})
	_ = actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: "fault_schedule", Result: FaultSchedule{Seed: 1, Events: []FaultEvent{
		{AfterOp: 1, Action: FaultKillDuring, Process: "p1", Target: "write", Permille: 500}, {AfterOp: 2, Action: FaultRestart, Process: "p1"}}}.Record()})
	e := FaultEvent{AfterOp: 0, Action: FaultKillDuring, Process: "p1", Target: "write", Permille: 500}
	fr := newFaultRun(r, nil, nil, map[string]any{}, nil)
	fr.inOpRecs = map[int]inOpRecord{1: {Action: FaultKillDuring, OpNo: 1, Process: "p1", OffsetMs: 40}}
	fr.arm(e)
	if _, err := fr.stepOp(context.Background(), actx, r, Op{No: 1, Action: "write", Volume: "v1", Params: map[string]string{"volume": "v1"}}); err != nil {
		t.Fatal(err)
	}
	actx.History.Close()
	sc, err := ReadScript("t", p, toyDriver{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Steps) != 1 || sc.Steps[0].Recorded != tr.OutcomeUnknown {
		t.Fatalf("steps %+v", sc.Steps)
	}
	if rec := sc.InOp[1]; rec.Action != FaultKillDuring || rec.Process != "p1" || rec.OffsetMs != 40 {
		t.Fatalf("in-op record %+v", sc.InOp)
	}
}

// The kill's offset runs from the operation's own invocation record, not
// from the handler call: a handler that checks for 200 ms before recording
// its invocation still has the kill land inside it (red control: timed from
// the handler call, the 50 ms kill ran before the operation, MISSED).
func TestKillDuringIsTimedFromTheInvocation(t *testing.T) {
	x := &inOpRunner{opTime: 300 * time.Millisecond, preInvoke: 200 * time.Millisecond}
	r := x.runner(toyDriver{})
	actx, p := rerunHistory(t)
	e := FaultEvent{AfterOp: 0, Action: FaultKillDuring, Process: "p0", Target: "write", Permille: 500}
	fr := newFaultRun(r, []FaultEvent{e}, nil, map[string]any{}, nil)
	fr.inOpRecs = map[int]inOpRecord{1: {Action: FaultKillDuring, AfterOp: 0, OpNo: 1, Process: "p0", OffsetMs: 50}}
	fr.arm(e)
	got, err := fr.stepOp(context.Background(), actx, r, Op{No: 1, Action: "write", Volume: "v1", Params: map[string]string{"volume": "v1"}})
	actx.History.Close()
	shape, recs := inOpShape(t, p)
	want := []string{"observe gen_step", "invoke write", "invoke kill", "complete kill ok", "complete write unknown", "observe fault_during"}
	if err != nil || got != tr.OutcomeUnknown || strings.Join(shape, "|") != strings.Join(want, "|") {
		t.Fatalf("%q %v\n%s", got, err, strings.Join(shape, "\n"))
	}
	for _, r := range recs {
		if r.Verb == "fault_during" && r.Result["hit"] != true {
			t.Fatalf("%v", r.Result)
		}
	}
}

// The test seam: the handlers of the operation a kill is timed into see
// KillTimedInto(ctx) true; an ordinary step's do not. It changes nothing
// else: the kill, its offset and the records are those of
// TestKillDuringLandsInsideTheOperation.
func TestKillTimedIntoMarksOnlyTheInOpOperation(t *testing.T) {
	x := &inOpRunner{opTime: 100 * time.Millisecond}
	r := x.runner(toyDriver{})
	var seen []bool
	inner := r.Handlers["write"]
	r.Handlers["write"] = func(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
		seen = append(seen, KillTimedInto(ctx))
		return inner(ctx, actx, act)
	}
	actx, _ := rerunHistory(t)
	defer actx.History.Close()
	e := FaultEvent{AfterOp: 0, Action: FaultKillDuring, Process: "p0", Target: "write", Permille: 500}
	fr := newFaultRun(r, []FaultEvent{e}, nil, map[string]any{}, nil)
	fr.inOpRecs = map[int]inOpRecord{1: {Action: FaultKillDuring, AfterOp: 0, OpNo: 1, Process: "p0", OffsetMs: 20}}
	fr.arm(e)
	if got, err := fr.stepOp(context.Background(), actx, r, Op{No: 1, Action: "write", Volume: "v1", Params: map[string]string{"volume": "v1"}}); err != nil || got != tr.OutcomeUnknown {
		t.Fatalf("in-op write: %q %v", got, err)
	}
	if _, err := r.Exec(context.Background(), actx, Op{No: 2, Action: "write", Volume: "v1", Params: map[string]string{"volume": "v1"}}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || !seen[0] || seen[1] {
		t.Fatalf("KillTimedInto per write call: %v, want [true false]", seen)
	}
	if KillTimedInto(context.Background()) {
		t.Fatal("a plain context is marked")
	}
}

// Red controls of the replay rule's first condition: every driver decision
// of an in-op fault must match (inOpDecision): fired at all, the same
// fault and process, the same offset (a kill), the same follow-ups (a
// store pause). A differing hit alone is not a decision.
func TestInOpDecisionMustMatch(t *testing.T) {
	kill := inOpRecord{Action: FaultKillDuring, OpNo: 5, Process: "p0", OffsetMs: 12, Hit: true}
	if why := inOpDecision(kill, inOpRecord{Action: FaultKillDuring, OpNo: 5, Process: "p0", OffsetMs: 12}, true); why != "" {
		t.Fatalf("a differing hit alone is not a decision: %s", why)
	}
	for name, c := range map[string]struct {
		got   inOpRecord
		fired bool
	}{
		"not fired":       {inOpRecord{}, false},
		"another process": {inOpRecord{Action: FaultKillDuring, Process: "p1", OffsetMs: 12}, true},
		"another offset":  {inOpRecord{Action: FaultKillDuring, Process: "p0", OffsetMs: 13}, true},
		"another fault":   {inOpRecord{Action: FaultStorePause, Process: "p0"}, true},
	} {
		if inOpDecision(kill, c.got, c.fired) == "" {
			t.Errorf("%s: not a difference", name)
		}
	}
	store := inOpRecord{Action: FaultStorePause, Process: "filer", Followups: []Op{{Action: "snapshot", Volume: "v1"}}}
	if inOpDecision(store, inOpRecord{Action: FaultStorePause, Process: "filer", Followups: []Op{{Action: "stop", Volume: "v1"}}}, true) == "" {
		t.Error("other follow-ups: not a difference")
	}
	if why := inOpDecision(store, store, true); why != "" {
		t.Errorf("the same store pause: %s", why)
	}
}

// The replay_checks_fail message calls only a violation a product finding:
// a checker of another kind (unknown, coverage) "did not pass".
func TestChecksFailNamesOnlyAViolationAProductFinding(t *testing.T) {
	_, err := checksFailAnswer("r", 3, []checkFail{{Checker: "C6", Verdict: "FAIL", Kind: CheckKindUnknown}, {Checker: "C5", Verdict: "UNKNOWN", Kind: CheckKindUnknown}})
	if err == nil || !strings.Contains(err.Error(), "C6 FAIL, C5 UNKNOWN did not pass") || strings.Contains(err.Error(), "product finding") {
		t.Fatalf("%v", err)
	}
	_, err = checksFailAnswer("r", 3, []checkFail{{Checker: "C1", Verdict: "FAIL", Kind: CheckKindViolation}, {Checker: "C6", Verdict: "FAIL", Kind: "coverage"}})
	if err == nil || !strings.Contains(err.Error(), "the violation(s) of C1 are a product finding") || strings.Contains(err.Error(), "C6 are") {
		t.Fatalf("%v", err)
	}
}

// The guards end with their History: after a History's operation and
// fault guards were used and it is closed and dropped, nothing keeps it
// (the garbage collector runs its finalizer). Red control: a
// package-level registry of guards by History keeps every one forever.
func TestAClosedHistoryLeavesNoGuardBehind(t *testing.T) {
	done := make(chan struct{})
	func() {
		h, err := tr.OpenHistory(filepath.Join(t.TempDir(), "h.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range []string{opGuard, faultGuard} {
			leave, ok := enter(g, h)
			if !ok {
				t.Fatalf("%s: refused on a fresh history", g)
			}
			if _, again := enter(g, h); again {
				t.Fatalf("%s: a second holder entered", g)
			}
			leave()
		}
		runtime.SetFinalizer(h, func(*tr.History) { close(done) })
		if err := h.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	for i := 0; i < 100; i++ {
		runtime.GC()
		select {
		case <-done:
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("a closed, dropped History is still referenced after its guards were used")
}
