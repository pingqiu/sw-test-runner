package v3block

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
)

// fakeProduct stands in for the typed handlers: it records each call as an
// invoke/complete pair (as record() does) and answers refused to every step
// that declares expect=refused, ok otherwise; failWrite makes one lba's write
// unknown, boom makes one action a runner error.
type fakeProduct struct {
	calls     []tr.Action
	failWrite string
	boom      string
}

func (f *fakeProduct) install(t *testing.T) {
	old := genHandlers
	t.Cleanup(func() { genHandlers = old })
	genHandlers = map[string]tr.ActionHandlerFunc{}
	for name := range old {
		name := name
		genHandlers[name] = func(_ context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
			f.calls = append(f.calls, act)
			if name == f.boom {
				return nil, errors.New("handler failed")
			}
			out := tr.OutcomeOK
			if act.Params["expect"] == tr.OutcomeRefused {
				out = tr.OutcomeRefused
			}
			if name == "v3_op_write" && act.Params["lbas"] == f.failWrite {
				out = tr.OutcomeUnknown
			}
			verb := strings.TrimPrefix(name, "v3_op_")
			if name == "v3_op_observe" {
				// The product past the lease horizon: every observation shows it.
				_ = actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Verb: "observe", Volume: act.Params["volume"],
					Result: map[string]any{"workspace": map[string]any{"exit": 0, "text": []any{"lease state: expired"}}}})
				return map[string]string{"value": "observed"}, nil // as v3OpObserve: an observation, not an outcome
			}
			seq, _ := actx.History.Invoke(tr.HistoryRecord{Op: "X." + verb, Verb: verb, Volume: act.Params["volume"]})
			_ = actx.History.Complete(seq, out, nil)
			return map[string]string{"value": out}, nil
		}
	}
}

func records(t *testing.T, p string) []tr.HistoryRecord {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []tr.HistoryRecord
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<20), 1<<20)
	for s.Scan() {
		var r tr.HistoryRecord
		if err := json.Unmarshal(s.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func genRunCtx(t *testing.T) (*tr.ActionContext, string) {
	dir := t.TempDir()
	p := filepath.Join(dir, "history.jsonl")
	h, err := tr.OpenHistory(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() }) // an open file blocks the temp dir's removal on Windows
	// A fault run reads the holder unit's policy from the product tree; here
	// it is the fixture's, as if already read.
	pol, err := parseUnit(testUnit)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(pol)
	return &tr.ActionContext{History: h, Bundle: &tr.RunBundle{Dir: dir}, Vars: map[string]string{unitPolicyVar: string(b)}, Log: func(string, ...interface{}) {}}, p
}

func genAct(ops string) tr.Action {
	return tr.Action{Action: "v3_gen_run", Node: "host", Params: map[string]string{
		"seed": "5", "ops": ops, "blocks": "8", "pool": strings.Join(pool(30), " ")}}
}

func TestGenRunRecordsHeaderStepsObservationsAndCoverage(t *testing.T) {
	f := &fakeProduct{}
	f.install(t)
	actx, p := genRunCtx(t)
	out, err := v3GenRun(context.Background(), actx, genAct("300"))
	if err != nil {
		t.Fatal(err)
	}
	_ = actx.History.Close()
	recs := records(t, p)
	if recs[0].Verb != "gen_header" || recs[0].Result["generator"] != GenVersion || recs[0].Result["seed"] != "5" {
		t.Fatalf("first record %+v", recs[0])
	}
	var last tr.HistoryRecord
	for _, r := range recs {
		if r.Kind != tr.HistoryEnd {
			last = r
		}
	}
	if last.Verb != "gen_coverage" || last.Result["drawn"] != float64(300) || out["proposed"] != fmt.Sprint(last.Result["proposed"]) {
		t.Fatalf("last record %+v, out %v", last, out)
	}
	if _, err := os.Stat(filepath.Join(actx.Bundle.Dir, "gen-coverage.json")); err != nil {
		t.Fatal(err)
	}
	// Every operation record follows its gen_step; a negative is preceded by two
	// observations of each volume it names (C5's baseline pair; a restore or a
	// release names its source and its target) and followed by one of each.
	steps, refusals, pairs := 0, 0, 0
	for i, r := range recs {
		if r.Verb != "gen_step" {
			continue
		}
		if r.Result["forced"] == nil {
			steps++
		}
		next := recs[i+1]
		if r.Result["negative"] != nil {
			subjects := []string{r.Volume}
			if a := r.Result["action"]; a == "v3_op_restore" || a == "v3_op_release" {
				subjects = append(subjects, r.Result["params"].(map[string]any)["target_volume"].(string))
				pairs++
			}
			j := i + 1
			for _, s := range subjects {
				for k := 0; k < 2; k++ {
					if o := recs[j]; o.Verb != "observe" || o.Volume != s {
						t.Fatalf("negative step %v: %s not observed twice before: %+v", r.Result, s, o)
					}
					j++
				}
			}
			refusals++
			// invoke, complete(refused), then the observations after it
			if recs[j].Kind != tr.HistoryInvoke || recs[j+1].Outcome != tr.OutcomeRefused {
				t.Fatalf("negative step %v: %+v %+v", r.Result, recs[j], recs[j+1])
			}
			for k, s := range subjects {
				if o := recs[j+2+k]; o.Verb != "observe" || o.Volume != s {
					t.Fatalf("refusal at step %v: %s not observed after: %+v", r.Result, s, o)
				}
			}
		} else if next.Kind != tr.HistoryInvoke && r.Result["action"] != "v3_op_observe" {
			t.Fatalf("step %v not followed by its invocation: %+v", r.Result, next)
		}
	}
	if steps != 300 || refusals == 0 || pairs == 0 {
		t.Fatalf("%d steps, %d refusals, %d of them on a pair", steps, refusals, pairs)
	}
}

func TestGenRunSplitsAWriteAndTakesTheWorstOutcome(t *testing.T) {
	f := &fakeProduct{}
	f.install(t)
	o := GenOp{Action: "v3_op_write", Volume: "g1", Params: map[string]string{"volume": "g1", "lbas": "1 3 5", "round": "g1"}}
	f.failWrite = "3"
	actx, _ := genRunCtx(t)
	got, err := genExec(context.Background(), actx, "host", o)
	if err != nil || got != tr.OutcomeUnknown {
		t.Fatalf("%q %v", got, err)
	}
	if len(f.calls) != 3 || f.calls[1].Params["lbas"] != "3" || f.calls[0].Params["round"] != "g1" {
		t.Fatalf("calls %+v", f.calls)
	}
	if o.Params["lbas"] != "1 3 5" {
		t.Fatal("the proposal's params were changed")
	}
}

func TestGenRunStopsOnARunnerErrorNotOnAProductAnswer(t *testing.T) {
	f := &fakeProduct{boom: "v3_op_attach"}
	f.install(t)
	actx, _ := genRunCtx(t)
	if _, err := v3GenRun(context.Background(), actx, genAct("300")); err == nil || !strings.Contains(err.Error(), "v3_op_attach") {
		t.Fatalf("a handler error did not stop the run: %v", err)
	}
	f2 := &fakeProduct{}
	f2.install(t)
	actx2, _ := genRunCtx(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := v3GenRun(ctx, actx2, genAct("10")); err == nil {
		t.Fatal("a cancelled run went on")
	}
	for name, act := range map[string]tr.Action{
		"no seed":    {Params: map[string]string{"ops": "5", "blocks": "4", "pool": "a"}},
		"bad weight": {Params: map[string]string{"seed": "1", "ops": "5", "blocks": "4", "pool": "a", "weights": "v3_op_write"}},
	} {
		if _, err := v3GenRun(context.Background(), actx2, act); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The scenario's required checker set is recorded in the header; a value
// that is no checker list is refused before anything runs.
func TestGenRunRecordsTheCheckerSetInTheHeader(t *testing.T) {
	f := &fakeProduct{}
	f.install(t)
	actx, p := genRunCtx(t)
	a := genAct("20")
	a.Params["checkers"] = "C0,C1,C5,MO"
	if _, err := v3GenRun(context.Background(), actx, a); err != nil {
		t.Fatal(err)
	}
	_ = actx.History.Close()
	if h := records(t, p)[0]; h.Verb != "gen_header" || h.Result["checkers"] != "C0,C1,C5,MO" {
		t.Fatalf("header %+v", h)
	}
	for _, bad := range []string{"C0, C1", "C0,,C1", "C0;C1", "{{ checkers }}"} {
		actx2, p2 := genRunCtx(t)
		a.Params["checkers"] = bad
		if _, err := v3GenRun(context.Background(), actx2, a); err == nil {
			t.Errorf("checkers %q accepted", bad)
		}
		_ = actx2.History.Close()
		if n := len(records(t, p2)); n != 1 { // the end record only
			t.Errorf("checkers %q: %d records written", bad, n)
		}
	}
}

// One source for the checker set: each random scenario's env checkers is
// what v3_gen_run records and what its judge passes to --checkers.
func TestRandomScenariosPassOneCheckerSet(t *testing.T) {
	for file, want := range map[string]string{
		"../../scenarios/v3block/m1-random.yaml": "C0,C1,C2,C3,C5,C7",
		"../../scenarios/v3block/m2-faults.yaml": "C0,C1,C2,C3,C5,C6,C7,DUR,MO",
	} {
		s, err := tr.ParseFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if s.Env["checkers"] != want {
			t.Errorf("%s: env checkers %q, want %q", file, s.Env["checkers"], want)
		}
		gen, judge := 0, 0
		for _, ph := range s.Phases {
			for _, a := range ph.Actions {
				if a.Action == "v3_gen_run" {
					gen++
					if a.Params["checkers"] != "{{ checkers }}" {
						t.Errorf("%s: v3_gen_run checkers %q", file, a.Params["checkers"])
					}
				}
				if cmd := a.Params["cmd"]; strings.Contains(cmd, "history_check.py") || strings.Contains(cmd, "--checkers") {
					judge++
					if !strings.Contains(cmd, `--checkers "{{ checkers }}"`) {
						t.Errorf("%s: judge %q", file, cmd)
					}
				}
			}
		}
		if gen != 1 || judge != 1 {
			t.Errorf("%s: %d v3_gen_run, %d judge lines", file, gen, judge)
		}
	}
}
