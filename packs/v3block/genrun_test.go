package v3block

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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
				_ = actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Verb: "observe", Volume: act.Params["volume"]})
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
	if last.Verb != "gen_coverage" || out["proposed"] != "300" {
		t.Fatalf("last record %+v, out %v", last, out)
	}
	if _, err := os.Stat(filepath.Join(actx.Bundle.Dir, "gen-coverage.json")); err != nil {
		t.Fatal(err)
	}
	// Every operation record follows its gen_step; a negative is preceded by two
	// observations of its volume (C5's baseline pair) and followed by one.
	steps, refusals := 0, 0
	for i, r := range recs {
		if r.Verb != "gen_step" {
			continue
		}
		steps++
		next := recs[i+1]
		if r.Result["negative"] != nil {
			for _, o := range recs[i+1 : i+3] {
				if o.Verb != "observe" || o.Volume != r.Volume {
					t.Fatalf("negative step %v not observed twice before: %+v", r.Result, o)
				}
			}
			refusals++
			// invoke, complete(refused), then the observation after it
			if recs[i+3].Kind != tr.HistoryInvoke || recs[i+4].Outcome != tr.OutcomeRefused || recs[i+5].Verb != "observe" || recs[i+5].Volume != r.Volume {
				t.Fatalf("refusal at step %v not observed after", r.Result)
			}
		} else if next.Kind != tr.HistoryInvoke {
			t.Fatalf("step %v not followed by its invocation: %+v", r.Result, next)
		}
	}
	if steps != 300 || refusals == 0 {
		t.Fatalf("%d steps, %d refusals", steps, refusals)
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
