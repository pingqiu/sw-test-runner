package v3block

// v3_gen_replay: re-execute the script of a recorded v3_gen_run history and
// compare every outcome with the recorded one (randomrun.Runner.Replay; the
// rule for a step's recorded outcome is documented in randomrun/replay.go).
// It replays GenVersion and GenVersionFaults histories only. Param: history
// (the recorded history.jsonl).

import (
	"context"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/randomrun"
)

// readReplayScript reads a recorded history: its gen_header result and its
// steps with their recorded outcomes.
func readReplayScript(path string) (map[string]any, []randomrun.ReplayStep, error) {
	sc, err := randomrun.ReadScript("v3_gen_replay", path, &blockModel{})
	return sc.Header, sc.Steps, err
}

func v3GenReplay(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	return genRunner("v3_gen_replay", act.Node, &blockModel{}).Replay(ctx, actx, opParam(act, actx, "history"))
}
