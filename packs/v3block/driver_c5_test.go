package v3block

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

// checkerPython is the Python that runs the checkers, or "" when none is
// installed (the test is skipped then).
func checkerPython() string {
	for _, p := range []string{"python3", "python"} {
		if path, err := exec.LookPath(p); err == nil {
			if exec.Command(path, "-c", "import sys; sys.exit(sys.version_info < (3, 8))").Run() == nil {
				return path
			}
		}
	}
	return ""
}

// End to end, from the driver to the checker: a delete refused twice (the
// lease-horizon refusal before persistence), then ok, run by the random
// run's driver over the declared handlers, observed right after each
// refused attempt and right before the next; the history is judged by
// checkers/history_check.py. Green: HEAD
// stays put, C5 judges both refused attempts under "ordinary delete" and
// passes. Red control: the observation after the first refused attempt
// shows HEAD moved; C5 FAILS, although the request's last attempt is ok.
func TestDriverRerunJudgedByC5(t *testing.T) {
	py := checkerPython()
	if py == "" {
		t.Skip("no python3 to run the checkers")
	}
	defer func(p func(time.Duration) time.Duration) { blockActions.Pause = p }(blockActions.Pause)
	blockActions.Pause = func(time.Duration) time.Duration { return time.Millisecond }
	live := chunkErr("not_admitted", "layout: delete reservation: chunkstore: volume delete not admitted: rpc error: code = FailedPrecondition desc = reserve volume deletion: workspace: deletion refused before persistence: workspace delete: live until T")
	deleted := eqAnswer{out: `{"volume":"v1","state":"deleted"}`}
	head := func(gen string) eqAnswer {
		return eqAnswer{out: `{"head":{"Gen":` + gen + `,"Epoch":2,"Holder":"r1","State":"serving","Snapshots":{}}}`}
	}
	ws := eqAnswer{out: strings.Join([]string{"volume: v1 (query snapshot, not authority to serve)", "replica: none", "epoch: none", "lease state: none", "holder: none",
		"recorded ACK: none", "host: unknown (not recorded by the master); issued moves and bindings below are separate facts"}, "\n") + "\n"}
	judge := func(shows []eqAnswer) map[string]any {
		t.Helper()
		var hist string
		c := eqCase{params: map[string]string{"volume": "v1"}, rules: []eqRule{
			{"volume-delete", []eqAnswer{live, live, deleted}}, {"ops chunk show", shows}, {"ops workspace show", []eqAnswer{ws}},
			{"namePattern", []eqAnswer{{out: `["v1-01234567"]` + "\n"}}}, {"python3", []eqAnswer{{out: `["HEAD","VOLUME"]`}}}}}
		r := runEq(t, func(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
			hist = actx.History.Path()
			obs := blockActions.Handler("v3_op_observe")
			for i := 0; i < 2; i++ { // the baseline pair, and the first attempt's "before"
				if _, err := obs(ctx, actx, tr.Action{Action: "v3_op_observe", Node: act.Node, Params: map[string]string{"volume": "v1"}}); err != nil {
					return nil, err
				}
			}
			got, err := genExec(ctx, actx, act.Node, GenOp{Action: "v3_op_delete", Volume: "v1", Params: map[string]string{"volume": "v1"}})
			return map[string]string{"value": got}, err
		}, c, true)
		if r.err != nil || r.ret["value"] != tr.OutcomeOK {
			t.Fatalf("%v %v", r.ret, r.err)
		}
		out := filepath.Join(t.TempDir(), "verdict.json")
		cmd := exec.Command(py, "-I", filepath.Join("..", "..", "checkers", "history_check.py"), hist, "--json", out)
		if b, err := cmd.CombinedOutput(); err != nil && !strings.Contains(err.Error(), "exit status") {
			t.Fatalf("history_check: %v %s", err, b)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]map[string]any
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		return v["C5"]
	}
	findings := func(v map[string]any) string {
		b, _ := json.MarshalIndent(v, "", " ")
		return string(b)
	}

	green := judge([]eqAnswer{head("5")})
	if green["verdict"] != "PASS" || !strings.Contains(findings(green), "limit ordinary delete: 2 of 2 refusal(s) judged") {
		t.Fatalf("green: %s", findings(green))
	}
	red := judge([]eqAnswer{head("5"), head("5"), head("6")})
	if red["verdict"] != "FAIL" || !strings.Contains(findings(red), "(attempt 1, replaced by a later attempt) (ordinary delete) refused but its head changed") {
		t.Fatalf("red: %s", findings(red))
	}
	// HEAD moves during the pause after attempt 1 (between the observation
	// right after it and the one right before attempt 2): a baseline
	// difference, C5 UNKNOWN, and neither attempt is blamed.
	pause := judge([]eqAnswer{head("5"), head("5"), head("5"), head("6")})
	if pause["verdict"] != "UNKNOWN" || !strings.Contains(findings(pause), "baseline: v1 changed with no operation on it") ||
		strings.Contains(findings(pause), "refused but its") {
		t.Fatalf("a change during the pause: %s", findings(pause))
	}
	t.Logf("a change during the pause, C5 findings: %s", findings(pause))
}

// judgeFile runs checkers/history_check.py on history hist: the verdicts by
// checker.
func judgeFile(t *testing.T, py, hist string) map[string]map[string]any {
	t.Helper()
	out := filepath.Join(t.TempDir(), "verdict.json")
	cmd := exec.Command(py, "-I", filepath.Join("..", "..", "checkers", "history_check.py"), hist, "--json", out)
	if b, err := cmd.CombinedOutput(); err != nil && !strings.Contains(err.Error(), "exit status") {
		t.Fatalf("history_check: %v %s", err, b)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// End to end, from the driver to the checker: a snapshot answered
// head_conflict (unknown, the page's answer). The driver observes the volume
// and places the attempt by the placement row; C6 places each head_conflict
// attempt by the same row and the same observation. Name absent: attempt 1
// placed "did not", re-issued, attempt 2 ok. Name present: placed "took
// effect", not re-issued, the request stays unknown.
func TestSnapshotPlacementJudgedByC6(t *testing.T) {
	py := checkerPython()
	if py == "" {
		t.Skip("no python3 to run the checkers")
	}
	defer func(p func(time.Duration) time.Duration) { blockActions.Pause = p }(blockActions.Pause)
	blockActions.Pause = func(time.Duration) time.Duration { return time.Millisecond }
	conflict := chunkErr("head_conflict", "storage: capture CAS not applied: storage: chunk HEAD precondition failed")
	run := func(shows ...string) (map[string]map[string]any, eqRun) {
		var heads []eqAnswer
		for _, s := range shows {
			heads = append(heads, eqAnswer{out: `{"head":{"Gen":5,"Epoch":2,"Holder":"r1","State":"serving","Snapshots":` + s + `}}`})
		}
		var hist string
		c := eqCase{params: map[string]string{"volume": "v1", "name": "s1"}, rules: []eqRule{
			{"ops chunk snapshot", []eqAnswer{conflict, {}}}, {"ops chunk show", heads}}}
		r := runEq(t, func(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
			hist = actx.History.Path()
			got, err := genExec(ctx, actx, act.Node, GenOp{Action: "v3_op_snapshot", Volume: "v1", Params: act.Params})
			return map[string]string{"value": got}, err
		}, c, true)
		if r.err != nil {
			t.Fatal(r.err)
		}
		return judgeFile(t, py, hist), r
	}
	show := func(v map[string]any) string { b, _ := json.MarshalIndent(v, "", " "); return string(b) }

	absent, r := run(`{}`, `{}`, `{"s1":{}}`)
	if r.ret["value"] != tr.OutcomeOK || absent["C6"]["verdict"] != "PASS" ||
		!strings.Contains(show(absent["C6"]), "seq 2 snapshot v1 (attempt 1, replaced by a later attempt) placed did not: by snapshot names: 's1' absent") {
		t.Fatalf("name absent: %v\nC6 %s", r.ret, show(absent["C6"]))
	}
	t.Logf("name absent, C6: %s\nC0: %s", show(absent["C6"]), show(absent["C0"]))
	present, r := run(`{}`, `{"s1":{}}`)
	if r.ret["value"] != tr.OutcomeUnknown || present["C6"]["verdict"] != "PASS" ||
		!strings.Contains(show(present["C6"]), "seq 2 snapshot v1 placed took effect: by snapshot names: 's1' present") {
		t.Fatalf("name present: %v\nC6 %s", r.ret, show(present["C6"]))
	}
	t.Logf("name present, C6: %s\nC0: %s", show(present["C6"]), show(present["C0"]))
	third, _ := run(`{"s0":{}}`, `{}`)
	if third["C6"]["verdict"] != "FAIL" || !strings.Contains(show(third["C6"]), "unknown in a third state: seq 2 snapshot v1") {
		t.Fatalf("third: C6 %s", show(third["C6"]))
	}
}
