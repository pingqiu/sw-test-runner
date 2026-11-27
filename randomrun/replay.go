package randomrun

// Replay: re-execute the script of a recorded run and compare every outcome
// with the recorded one. Replay passes only if every outcome equals the
// recorded one, and it reports each difference.
//
// The script is read from the recorded history alone:
//   - the first observe record must be "gen_header" naming one of the
//     Driver's two versions; otherwise the replay is refused before any
//     operation runs. A second gen_header is refused too: one run per file;
//   - each "gen_step" observe record is one step: its opno, action, params,
//     volume and negative limit;
//   - a fault-run history carries its "fault_schedule" right after the
//     header; the replay re-executes the schedule through the same faultRun
//     as a run (kills, restarts, healthy points, resolving observations);
//   - a "gen_sweep" observe record lists the end-of-run sweep; its records
//     belong to no step, and the replay re-executes the same sweep after its
//     last step (its outcomes are not compared: they are the checkers' facts).
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
// with "replay_of" (the source path), then each step through Step (the same
// gen_step record, observations and handlers as a run), so a replayed history
// is itself replayable and judged by the same rule. It goes through every
// step, and writes last an observe record "gen_replay":
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

// Script is a recorded run as a replay reads it.
type Script struct {
	Header map[string]any
	Steps  []ReplayStep
	Events []FaultEvent // the fault schedule of a fault-run history; nil otherwise
	Sweep  []Op         // the end-of-run sweep; nil when the history has none
}

// ReplayStep is one step of a recorded script with its recorded outcome.
type ReplayStep struct {
	Op       Op
	Recorded string // "" until a complete record is seen
}

// replayDiff is one step whose replayed outcome differs from the recorded one.
type replayDiff struct {
	OpNo     int
	Action   string
	Volume   string
	Recorded string
	Replayed string
}

// ReadScript reads a recorded history: its gen_header result, its steps with
// their recorded outcomes, and the fault schedule of a fault-run history (nil
// otherwise): the observe record "fault_schedule" must be the one right after
// the gen_header, and its events must pass CheckFaultSchedule. A fault
// event's records (the kill or restart invocation, the healthy observation and
// the resolving operations after it) end the current step: they are not the
// step's operation.
func ReadScript(name, path string, d Driver) (Script, error) {
	plain, faults := d.Version(false), d.Version(true)
	f, err := os.Open(path)
	if err != nil {
		return Script{}, fmt.Errorf("%s: %w", name, err)
	}
	defer f.Close()
	var header map[string]any
	var steps []ReplayStep
	var events, sweep = []FaultEvent(nil), []Op(nil)
	faulty, needSchedule := false, false
	cur := -1 // the index of the step whose records are being read; -1 for none
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<20), 16<<20)
	line := 0
	for s.Scan() {
		line++
		var r tr.HistoryRecord
		if err := json.Unmarshal(s.Bytes(), &r); err != nil {
			return Script{}, fmt.Errorf("%s: %s line %d: %w", name, path, line, err)
		}
		switch r.Kind {
		case tr.HistoryObserve:
			if header == nil {
				if r.Verb != "gen_header" {
					return Script{}, fmt.Errorf("%s: %s: the first observation is %q, not gen_header", name, path, r.Verb)
				}
				g, _ := r.Result["generator"].(string)
				if g != plain && g != faults {
					return Script{}, fmt.Errorf("%s: %s: generator %v, this runner replays %s and %s", name, path, r.Result["generator"], plain, faults)
				}
				header = r.Result
				faulty = g == faults
				needSchedule = faulty
				continue
			}
			if needSchedule {
				if r.Verb != "fault_schedule" {
					return Script{}, fmt.Errorf("%s: %s line %d: a %s history without fault_schedule after its header", name, path, line, faults)
				}
				needSchedule = false
				if events, err = replaySchedule(header, r.Result, d.ValidProcess); err != nil {
					return Script{}, fmt.Errorf("%s: %s line %d: %w", name, path, line, err)
				}
				continue
			}
			switch r.Verb {
			case "gen_header":
				return Script{}, fmt.Errorf("%s: %s line %d: a second gen_header (one generator run per history)", name, path, line)
			case "fault_schedule":
				return Script{}, fmt.Errorf("%s: %s line %d: a fault_schedule not right after the header", name, path, line)
			case "gen_step":
				o, err := replayOp(r)
				if err != nil {
					return Script{}, fmt.Errorf("%s: %s line %d: %w", name, path, line, err)
				}
				steps = append(steps, ReplayStep{Op: o})
				cur = len(steps) - 1
			case "gen_sweep":
				if sweep != nil {
					return Script{}, fmt.Errorf("%s: %s line %d: a second gen_sweep", name, path, line)
				}
				if sweep, err = parseSweep(r.Result); err != nil {
					return Script{}, fmt.Errorf("%s: %s line %d: %w", name, path, line, err)
				}
				cur = -1
			case "gen_coverage", "gen_replay", "healthy":
				cur = -1
			}
		case tr.HistoryInvoke:
			if faulty && (r.Verb == FaultKill || r.Verb == FaultRestart) {
				cur = -1
			}
		case tr.HistoryComplete:
			if cur >= 0 {
				steps[cur].Recorded = worseOutcome(steps[cur].Recorded, r.Outcome)
			}
		case tr.HistoryEnd:
			cur = -1
		}
	}
	if err := s.Err(); err != nil {
		return Script{}, fmt.Errorf("%s: %s: %w", name, path, err)
	}
	if header == nil {
		return Script{}, fmt.Errorf("%s: %s: no gen_header", name, path)
	}
	if needSchedule {
		return Script{}, fmt.Errorf("%s: %s: a %s history without fault_schedule", name, path, faults)
	}
	if len(steps) == 0 {
		return Script{}, fmt.Errorf("%s: %s: no gen_step records", name, path)
	}
	return Script{Header: header, Steps: steps, Events: events, Sweep: sweep}, nil
}

// replaySchedule reads and checks a recorded fault_schedule against its header.
func replaySchedule(header, res map[string]any, valid func(string) bool) ([]FaultEvent, error) {
	events, err := parseFaultEvents(res)
	if err != nil {
		return nil, err
	}
	ops, _ := header["ops"].(float64)
	within, _ := header["fault_restart_within"].(float64)
	if res["seed"] != header["seed"] {
		return nil, fmt.Errorf("fault_schedule seed %v, header seed %v", res["seed"], header["seed"])
	}
	if err := CheckFaultSchedule(events, int(ops), int(within), valid); err != nil {
		return nil, fmt.Errorf("fault_schedule: %w", err)
	}
	return events, nil
}

// replayOp reads a gen_step record back into the proposal it recorded.
func replayOp(r tr.HistoryRecord) (Op, error) {
	no, ok := r.Result["opno"].(float64)
	if !ok || no != float64(int(no)) {
		return Op{}, fmt.Errorf("gen_step opno %v", r.Result["opno"])
	}
	o := Op{No: int(no), Volume: r.Volume, Params: map[string]string{}}
	if o.Action, ok = r.Result["action"].(string); !ok || o.Action == "" {
		return Op{}, fmt.Errorf("gen_step %d: action %v", o.No, r.Result["action"])
	}
	params, ok := r.Result["params"].(map[string]any)
	if !ok {
		return Op{}, fmt.Errorf("gen_step %d: params %v", o.No, r.Result["params"])
	}
	for k, v := range params {
		sv, ok := v.(string)
		if !ok {
			return Op{}, fmt.Errorf("gen_step %d: param %s is %T, not a string", o.No, k, v)
		}
		o.Params[k] = sv
	}
	if n, present := r.Result["negative"]; present {
		if o.Negative, ok = n.(string); !ok {
			return Op{}, fmt.Errorf("gen_step %d: negative %v", o.No, n)
		}
	}
	return o, nil
}

// Replay re-executes the recorded history src.
func (r Runner) Replay(ctx context.Context, actx *tr.ActionContext, src string) (map[string]string, error) {
	if actx.History == nil {
		return nil, fmt.Errorf("%s: no history (typed operations run only with a run bundle)", r.Name)
	}
	if src == "" {
		return nil, fmt.Errorf("%s: param history (the recorded history.jsonl) is required", r.Name)
	}
	sc, err := ReadScript(r.Name, src, r.Driver)
	if err != nil {
		return nil, err
	}
	header, steps, events := sc.Header, sc.Steps, sc.Events
	hdr := map[string]any{}
	for k, v := range header {
		hdr[k] = v
	}
	hdr["replay_of"] = src
	if err := actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: "gen_header", Result: hdr}); err != nil {
		return nil, err
	}
	var fr *faultRun
	if events != nil {
		sched := FaultSchedule{Events: events}
		sched.Seed, _ = strconv.ParseUint(fmt.Sprint(header["seed"]), 10, 64)
		if err := actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: "fault_schedule", Result: sched.Record()}); err != nil {
			return nil, err
		}
		fr = newFaultRun(r, events, nil, header)
	}
	diffs := []replayDiff{}
	for _, st := range steps {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%s: %w at operation %d", r.Name, ctx.Err(), st.Op.No)
		}
		o := st.Op
		replayed, err := r.Step(ctx, actx, o)
		if err != nil {
			return nil, fmt.Errorf("%s: operation %d (%s %s): %w", r.Name, o.No, o.Action, o.Volume, err)
		}
		if replayed != st.Recorded {
			rec := st.Recorded
			if rec == "" {
				rec = "none"
			}
			diffs = append(diffs, replayDiff{OpNo: o.No, Action: o.Action, Volume: o.Volume, Recorded: rec, Replayed: replayed})
		}
		if fr != nil {
			fr.note(o, replayed)
			if err := fr.afterOp(ctx, actx, o.No); err != nil {
				return nil, fmt.Errorf("%s: %w", r.Name, err)
			}
		}
	}
	if fr != nil {
		if err := fr.finish(ctx, actx); err != nil {
			return nil, fmt.Errorf("%s: end of run: %w", r.Name, err)
		}
	}
	if sc.Sweep != nil {
		if err := r.sweep(ctx, actx, sc.Sweep); err != nil {
			return nil, fmt.Errorf("%s: end-of-run sweep: %w", r.Name, err)
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
		return nil, fmt.Errorf("%s: %d of %d steps differ; first: operation %d (%s %s) recorded %s, replayed %s",
			r.Name, len(diffs), len(steps), d.OpNo, d.Action, d.Volume, d.Recorded, d.Replayed)
	}
	actx.Log("  %s: %d steps match %s", r.Name, len(steps), src)
	return map[string]string{"value": "match", "steps": strconv.Itoa(len(steps))}, nil
}
