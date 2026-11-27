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
