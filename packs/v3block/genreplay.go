package v3block

// v3_gen_replay: re-execute the script of a recorded v3_gen_run history and
// compare every outcome with the recorded one (randomrun.Runner.Replay; the
// rule for a step's recorded outcome is documented in randomrun/replay.go).
// It replays GenVersion and GenVersionFaults histories only. Param: history
// (the recorded history.jsonl).
//
// With runner_src set (a scenario always sets it), the replay judges its own
// history through the SAME entry the scenarios' judge step calls
// (judgeArgs: checkers/history_check.py with --product
// packs/v3block/product, the one checks.json) and the run header's checker
// set (the scenario's one list, recorded in the header): a checker added to
// the pack is run by both. python (default python3) runs it. Without
// runner_src the replay's history is not judged: a match is answered
// "match_unjudged" (or "match_inop_hit_differs_unjudged"), "checks" "not
// run: no runner_src".

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/randomrun"
)

// readReplayScript reads a recorded history: its gen_header result and its
// steps with their recorded outcomes.
func readReplayScript(path string) (map[string]any, []randomrun.ReplayStep, error) {
	sc, err := randomrun.ReadScript("v3_gen_replay", path, &blockModel{})
	return sc.Header, sc.Steps, err
}

// judgeArgs is the judge step's command (scenarios/v3block/*.yaml, the exec
// of history_check.py), one definition for the replay: the entry, the
// history, its --json, the pack's product directory and the required set.
func judgeArgs(runnerSrc, hist, out, checkers string) []string {
	a := []string{"-I", filepath.Join(runnerSrc, "checkers", "history_check.py"), hist, "--json", out,
		"--product", filepath.Join(runnerSrc, "packs", "v3block", "product")}
	if checkers != "" {
		a = append(a, "--checkers", checkers)
	}
	return a
}

// pythonJudge runs the pack's checkers on a history (randomrun.JudgeFunc):
// exit 0 (every required checker PASS) or 1 (one did not) is an answer,
// read from --json; anything else is the judge's own failure.
func pythonJudge(python, runnerSrc string) randomrun.JudgeFunc {
	return func(ctx context.Context, hist, checkers string) (map[string]randomrun.CheckerVerdict, error) {
		out := hist + ".checks.json"
		cmd := exec.CommandContext(ctx, python, judgeArgs(runnerSrc, hist, out, checkers)...)
		b, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if err != nil && !(errors.As(err, &ee) && ee.ExitCode() == 1) {
			return nil, fmt.Errorf("%s %v: %w: %s", python, cmd.Args[1:], err, b)
		}
		raw, err := os.ReadFile(out)
		if err != nil {
			return nil, fmt.Errorf("the checkers wrote no verdicts (%w): %s", err, b)
		}
		var v map[string]randomrun.CheckerVerdict
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("%s: %w", out, err)
		}
		return v, nil
	}
}

func v3GenReplay(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	r := genRunner("v3_gen_replay", act.Node, &blockModel{})
	if src := opParam(act, actx, "runner_src"); src != "" {
		py := opParam(act, actx, "python")
		if py == "" {
			py = "python3"
		}
		r.Judge = pythonJudge(py, src)
	} else {
		r.Unjudged = "no runner_src"
	}
	return r.Replay(ctx, actx, opParam(act, actx, "history"))
}
