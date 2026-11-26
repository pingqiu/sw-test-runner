package v3block

// v3_gen_replay: re-execute the script of a recorded v3_gen_run history and
// compare every outcome with the recorded one. Replay passes only if every
// outcome equals the recorded one, and it reports each difference.
//
// The script is read from the recorded history alone:
//   - the first observe record must be "gen_header" naming this generator
//     version (GenVersion); otherwise the replay is refused before any
//     operation runs. A second gen_header is refused too: one run per file;
//   - each "gen_step" observe record is one step: its opno, action, params,
//     volume and negative limit.
//
// The recorded outcome of a step (the rule):
//
//	the worst outcome (unknown > refused > ok) over the COMPLETE records
//	that follow the step's gen_step record, up to the next gen_step,
//	gen_coverage, gen_replay or end record. Invoke and observe records do
//	not count (the observations around a refusal are not the operation).
//	A step with no complete record has no recorded outcome ("none"), and
//	that is a difference whatever the replay answers: an invocation left
//	open is not a match.
//
// The replay writes into the current run's history a copy of the gen_header
// with "replay_of" (the source path), then each step through genStep (the
// same gen_step record, observations and typed handlers as v3_gen_run), so a
// replayed history is itself replayable and judged by the same rule. It goes
// through every step, and writes last an observe record "gen_replay":
// {steps, differences: [{opno, action, volume, recorded, replayed}]}.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	tr "github.com/pingqiu/sw-test-runner"
)

// replayStep is one step of a recorded script with its recorded outcome.
type replayStep struct {
	op       GenOp
	recorded string // "" until a complete record is seen
}

// genReplayDiff is one step whose replayed outcome differs from the recorded one.
type genReplayDiff struct {
	OpNo     int    `json:"opno"`
	Action   string `json:"action"`
	Volume   string `json:"volume"`
	Recorded string `json:"recorded"`
	Replayed string `json:"replayed"`
}

// worseOutcome returns the worse of two outcomes; "" (none yet) loses to any.
func worseOutcome(a, b string) string {
	if a == "" || outcomeRank[b] > outcomeRank[a] {
		return b
	}
	return a
}

// readReplayScript reads a recorded history: its gen_header result and its
// steps with their recorded outcomes.
func readReplayScript(path string) (map[string]any, []replayStep, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("v3_gen_replay: %w", err)
	}
	defer f.Close()
	var header map[string]any
	var steps []replayStep
	cur := -1 // the index of the step whose records are being read; -1 for none
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<20), 16<<20)
	line := 0
	for s.Scan() {
		line++
		var r tr.HistoryRecord
		if err := json.Unmarshal(s.Bytes(), &r); err != nil {
			return nil, nil, fmt.Errorf("v3_gen_replay: %s line %d: %w", path, line, err)
		}
		switch r.Kind {
		case tr.HistoryObserve:
			if header == nil {
				if r.Verb != "gen_header" {
					return nil, nil, fmt.Errorf("v3_gen_replay: %s: the first observation is %q, not gen_header", path, r.Verb)
				}
				if g, _ := r.Result["generator"].(string); g != GenVersion {
					return nil, nil, fmt.Errorf("v3_gen_replay: %s: generator %v, this runner replays %s", path, r.Result["generator"], GenVersion)
				}
				header = r.Result
				continue
			}
			switch r.Verb {
			case "gen_header":
				return nil, nil, fmt.Errorf("v3_gen_replay: %s line %d: a second gen_header (one generator run per history)", path, line)
			case "gen_step":
				o, err := replayOp(r)
				if err != nil {
					return nil, nil, fmt.Errorf("v3_gen_replay: %s line %d: %w", path, line, err)
				}
				steps = append(steps, replayStep{op: o})
				cur = len(steps) - 1
			case "gen_coverage", "gen_replay":
				cur = -1
			}
		case tr.HistoryComplete:
			if cur >= 0 {
				steps[cur].recorded = worseOutcome(steps[cur].recorded, r.Outcome)
			}
		case tr.HistoryEnd:
			cur = -1
		}
	}
	if err := s.Err(); err != nil {
		return nil, nil, fmt.Errorf("v3_gen_replay: %s: %w", path, err)
	}
	if header == nil {
		return nil, nil, fmt.Errorf("v3_gen_replay: %s: no gen_header", path)
	}
	if len(steps) == 0 {
		return nil, nil, fmt.Errorf("v3_gen_replay: %s: no gen_step records", path)
	}
	return header, steps, nil
}

// replayOp reads a gen_step record back into the proposal it recorded.
func replayOp(r tr.HistoryRecord) (GenOp, error) {
	no, ok := r.Result["opno"].(float64)
	if !ok || no != float64(int(no)) {
		return GenOp{}, fmt.Errorf("gen_step opno %v", r.Result["opno"])
	}
	o := GenOp{No: int(no), Volume: r.Volume, Params: map[string]string{}}
	if o.Action, ok = r.Result["action"].(string); !ok || o.Action == "" {
		return GenOp{}, fmt.Errorf("gen_step %d: action %v", o.No, r.Result["action"])
	}
	params, ok := r.Result["params"].(map[string]any)
	if !ok {
		return GenOp{}, fmt.Errorf("gen_step %d: params %v", o.No, r.Result["params"])
	}
	for k, v := range params {
		sv, ok := v.(string)
		if !ok {
			return GenOp{}, fmt.Errorf("gen_step %d: param %s is %T, not a string", o.No, k, v)
		}
		o.Params[k] = sv
	}
	if n, present := r.Result["negative"]; present {
		if o.Negative, ok = n.(string); !ok {
			return GenOp{}, fmt.Errorf("gen_step %d: negative %v", o.No, n)
		}
	}
	return o, nil
}

func v3GenReplay(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	if actx.History == nil {
		return nil, fmt.Errorf("v3_gen_replay: no history (typed operations run only with a run bundle)")
	}
	src := opParam(act, actx, "history")
	if src == "" {
		return nil, fmt.Errorf("v3_gen_replay: param history (the recorded history.jsonl) is required")
	}
	header, steps, err := readReplayScript(src)
	if err != nil {
		return nil, err
	}
	hdr := map[string]any{}
	for k, v := range header {
		hdr[k] = v
	}
	hdr["replay_of"] = src
	if err := actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: "gen_header", Result: hdr}); err != nil {
		return nil, err
	}
	diffs := []genReplayDiff{}
	for _, st := range steps {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("v3_gen_replay: %w at operation %d", ctx.Err(), st.op.No)
		}
		o := st.op
		replayed, err := genStep(ctx, actx, act.Node, o)
		if err != nil {
			return nil, fmt.Errorf("v3_gen_replay: operation %d (%s %s): %w", o.No, o.Action, o.Volume, err)
		}
		if replayed != st.recorded {
			rec := st.recorded
			if rec == "" {
				rec = "none"
			}
			diffs = append(diffs, genReplayDiff{OpNo: o.No, Action: o.Action, Volume: o.Volume, Recorded: rec, Replayed: replayed})
		}
	}
	dl := make([]any, 0, len(diffs))
	for _, d := range diffs {
		dl = append(dl, map[string]any{"opno": d.OpNo, "action": d.Action, "volume": d.Volume, "recorded": d.Recorded, "replayed": d.Replayed})
	}
	if err := actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: "gen_replay", Result: map[string]any{"steps": len(steps), "differences": dl}}); err != nil {
		return nil, err
	}
	if len(diffs) > 0 {
		d := diffs[0]
		return nil, fmt.Errorf("v3_gen_replay: %d of %d steps differ; first: operation %d (%s %s) recorded %s, replayed %s",
			len(diffs), len(steps), d.OpNo, d.Action, d.Volume, d.Recorded, d.Replayed)
	}
	actx.Log("  v3_gen_replay: %d steps match %s", len(steps), src)
	return map[string]string{"value": "match", "steps": strconv.Itoa(len(steps))}, nil
}
