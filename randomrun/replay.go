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
// {steps, differences: [{opno, action, volume, recorded, replayed}]}, and
// notes [{opno, action, volume, note}] when a step's rerun made another
// number of attempts than recorded (a note, not a difference).
//
// An in-operation fault whose HIT differs (the night's kill landed inside
// the operation and the replay's after it, or the reverse; or a store
// pause the operation answered through) is its own line, never a silent
// match and not a failure by itself: "in-op fault hit differs at op N
// (night: hit, replay: missed)", with both outcomes, in the gen_replay
// record's "inop_hit_differs" and its count in the replay's answer. It
// holds only when:
//   - every driver decision still matches: the replay fired the same fault
//     at the same operation, on the same process, at the same offset (a
//     kill) or with the same follow-ups (a store pause); otherwise that
//     operation is a difference;
//   - every other difference is downstream of that operation's outcome on
//     the same volume (a later step on that volume); one at another volume,
//     or before the fault, is a difference as before;
//   - the replay's own history passes its checkers: the replay judges it
//     (replayjudge.go), and a required checker not PASS answers
//     "replay_checks_fail" instead, for a plain match too.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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
	// Outages are the writes of each recorded master outage, by after_op;
	// OutageSkips the outages recorded skipped.
	Outages     map[int][]Op
	OutageSkips map[int]bool
	// Kills are the recorded kill decisions (fault_kill), in order; nil when
	// the history has none.
	Kills []KillDecision
	// InOp are the recorded in-operation faults that fired, by operation
	// number (inop.go).
	InOp map[int]inOpRecord
}

// ReplayStep is one step of a recorded script with its recorded outcome.
type ReplayStep struct {
	Op       Op
	Recorded string // "" until a complete record is seen
	Attempts int    // the step's invokes that carry an attempt number
}

// inOpHitDiff is an in-operation fault whose hit differs between the night
// and the replay, with the steps downstream of it on its volume whose
// outcome differs too.
type inOpHitDiff struct {
	OpNo                        int
	Action, Volume, Fault       string
	NightHit, ReplayHit         bool
	NightOutcome, ReplayOutcome string
	Downstream                  []replayDiff
}

func hitWord(hit bool) string {
	if hit {
		return "hit"
	}
	return "missed"
}

// line is the replay's report of the difference.
func (d inOpHitDiff) line() string {
	s := fmt.Sprintf("in-op fault hit differs at op %d (night: %s, replay: %s): %s %s %s, outcome night %s, replay %s",
		d.OpNo, hitWord(d.NightHit), hitWord(d.ReplayHit), d.Fault, d.Action, d.Volume, d.NightOutcome, d.ReplayOutcome)
	if len(d.Downstream) > 0 {
		s += fmt.Sprintf("; %d later step(s) on %s differ downstream of it", len(d.Downstream), d.Volume)
	}
	return s
}

// inOpDecision compares the replay's fired in-operation fault with the
// night's record: "" when every driver decision matches, else what differs.
func inOpDecision(night inOpRecord, replay inOpRecord, fired bool) string {
	switch {
	case !fired:
		return fmt.Sprintf("in-op %s not fired in the replay", night.Action)
	case replay.Action != night.Action || replay.Process != night.Process:
		return fmt.Sprintf("in-op fault %s on %s, replayed %s on %s", night.Action, night.Process, replay.Action, replay.Process)
	case night.Action == FaultKillDuring && replay.OffsetMs != night.OffsetMs:
		return fmt.Sprintf("in-op kill at %d ms, replayed at %d ms", night.OffsetMs, replay.OffsetMs)
	case night.Action == FaultStorePause && fmt.Sprint(opList(replay.Followups)) != fmt.Sprint(opList(night.Followups)):
		return fmt.Sprintf("store pause follow-ups %v, replayed %v", opList(night.Followups), opList(replay.Followups))
	}
	return ""
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
	outages, outageSkips := map[int][]Op{}, map[int]bool{}
	var kills []KillDecision
	faulty, needSchedule := false, false
	cur := -1 // the index of the step whose records are being read; -1 for none
	// A step's outcome is the worst of its operations', where the attempts
	// of one request (its invokes carry "attempt") count once, as their
	// last attempt: a rerun's earlier answers are history, not the
	// operation's outcome (stepFold).
	folds := map[int]*stepFold{}
	attemptOf := map[uint64]string{}
	// open: the current step's invokes not yet completed; faultSeq: the
	// invokes of faults inside a step (not the step's operation).
	open, faultSeq := map[uint64]bool{}, map[uint64]bool{}
	inOp := map[int]inOpRecord{}
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
				if events, err = replaySchedule(header, r.Result, d); err != nil {
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
				open = map[uint64]bool{}
			case "gen_sweep":
				if sweep != nil {
					return Script{}, fmt.Errorf("%s: %s line %d: a second gen_sweep", name, path, line)
				}
				if sweep, err = parseOps(r.Result, "ops"); err != nil {
					return Script{}, fmt.Errorf("%s: %s line %d: %w", name, path, line, err)
				}
				cur = -1
			case "fault_kill":
				k, ok := r.Result["after_op"].(float64)
				p, ok2 := r.Result["process"].(string)
				s, ok3 := r.Result["skipped"].(bool)
				if !ok || !ok2 || !ok3 || k != float64(int(k)) {
					return Script{}, fmt.Errorf("%s: %s line %d: fault_kill %v", name, path, line, r.Result)
				}
				sch, _ := r.Result["scheduled"].(string) // "" in a history from before targets were resolved
				kills = append(kills, KillDecision{AfterOp: int(k), Scheduled: sch, Process: p, Skipped: s})
				cur = -1
			case "master_outage":
				k, ok := r.Result["after_op"].(float64)
				w, err := parseOps(r.Result, "writes")
				if !ok || k != float64(int(k)) || err != nil || outages[int(k)] != nil {
					return Script{}, fmt.Errorf("%s: %s line %d: master_outage %v %v", name, path, line, r.Result["after_op"], err)
				}
				outages[int(k)] = w
				if r.Result["skipped"] != nil {
					outageSkips[int(k)] = true
				}
				cur = -1
			case "fault_during":
				// A kill that fired beside an operation (with its operation
				// number); one that never fired has none and replays as none.
				if no, ok := r.Result["opno"].(float64); ok {
					rec := inOpRecord{Action: FaultKillDuring, OpNo: int(no)}
					a, _ := r.Result["after_op"].(float64)
					off, _ := r.Result["offset_ms"].(float64)
					rec.AfterOp, rec.OffsetMs = int(a), int(off)
					rec.Process, _ = r.Result["process"].(string)
					rec.Hit = r.Result["hit"] == true
					inOp[rec.OpNo] = rec
				}
			case "store_pause":
				no, ok := r.Result["opno"].(float64)
				if !ok {
					cur = -1
					continue // a pause that never fired (MISSED: no target in its window)
				}
				a, ok2 := r.Result["after_op"].(float64)
				fu, err := parseOps(r.Result, "followups")
				if !ok2 || err != nil {
					return Script{}, fmt.Errorf("%s: %s line %d: store_pause %v %v", name, path, line, r.Result, err)
				}
				inOp[int(no)] = inOpRecord{Action: FaultStorePause, OpNo: int(no), AfterOp: int(a), Followups: fu, Process: fmt.Sprint(r.Result["process"]), Hit: r.Result["hit"] == true}
				cur = -1
			case "gen_coverage", "gen_replay", "gen_stop", VerbHealthy:
				cur = -1
			}
		case tr.HistoryInvoke:
			// A store fault's apply and restore, and a kill beside an
			// operation still open (kill_during), are inside the step but are
			// not its operation; any other kill or restart ends the step.
			if r.Verb == VerbFaultApply || r.Verb == VerbFaultRestore {
				faultSeq[r.Seq] = true
				continue
			}
			if faulty && (r.Verb == VerbKill || r.Verb == VerbRestart) {
				if cur >= 0 && len(open) > 0 {
					faultSeq[r.Seq] = true
					continue
				}
				cur = -1
			}
			if cur >= 0 {
				open[r.Seq] = true
				if key, ok := requestKey(rerunnerOf(d), steps[cur].Op.Action, r); ok {
					attemptOf[r.Seq] = key
					steps[cur].Attempts++
				}
			}
		case tr.HistoryComplete:
			delete(open, r.Of)
			if faultSeq[r.Of] {
				continue
			}
			if cur >= 0 {
				if folds[cur] == nil {
					folds[cur] = &stepFold{}
				}
				key, retried := attemptOf[r.Of]
				steps[cur].Recorded = folds[cur].complete(key, retried, r.Outcome)
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
	last := 0 // the last drawn step: events after it never ran
	for _, s := range steps {
		if s.Op.Forced != "tail" {
			last = max(last, s.Op.No)
		}
	}
	for _, e := range events {
		if e.Action == FaultMasterOutage && e.AfterOp <= last && outages[e.AfterOp] == nil {
			return Script{}, fmt.Errorf("%s: %s: the master outage after operation %d has no master_outage record", name, path, e.AfterOp)
		}
	}
	return Script{Header: header, Steps: steps, Events: events, Sweep: sweep, Outages: outages, OutageSkips: outageSkips, Kills: kills, InOp: inOp}, nil
}

// requestKey is the request an attempt record belongs to: its verb, volume
// and the value of the arg the action declares as its request id
// (Rerunner.RequestArg); without a Rerunner it is keyed by every arg
// but "attempt" (the checkers' rule). ok is false for an invoke that carries
// no attempt.
func requestKey(rr Rerunner, action string, r tr.HistoryRecord) (string, bool) {
	if _, ok := r.Args[AttemptParam]; !ok {
		return "", false
	}
	arg := ""
	if rr != nil {
		arg = rr.RequestArg(action)
	}
	if arg != "" {
		return fmt.Sprintf("%s|%s|%v", r.Verb, r.Volume, r.Args[arg]), true
	}
	rest := map[string]any{}
	for k, v := range r.Args {
		if k != AttemptParam {
			rest[k] = v
		}
	}
	b, _ := json.Marshal(rest) // map keys marshal sorted
	return fmt.Sprintf("%s|%s|%s", r.Verb, r.Volume, b), true
}

// rerunnerOf is d's Rerunner, nil when it declares no rerun.
func rerunnerOf(d Driver) Rerunner {
	rr, _ := d.(Rerunner)
	return rr
}

// stepFold folds a step's complete records into its recorded outcome: the
// worst over its operations, where the attempts of one request count once,
// as their LAST attempt.
type stepFold struct {
	parts []string
	slot  map[string]int
}

// complete takes one complete record (key: its request when retried) and
// returns the step's recorded outcome so far.
func (f *stepFold) complete(key string, retried bool, outcome string) string {
	if f.slot == nil {
		f.slot = map[string]int{}
	}
	if i, seen := f.slot[key]; retried && seen {
		f.parts[i] = outcome
	} else {
		if retried {
			f.slot[key] = len(f.parts)
		}
		f.parts = append(f.parts, outcome)
	}
	got := ""
	for _, o := range f.parts {
		got = worseOutcome(got, o)
	}
	return got
}

// attemptNote adds to notes the step whose replay made another number of
// attempts than its record (n): the count rests on the product's answers,
// so it is a note, never a difference; the step is compared on its
// requests' last outcomes.
func attemptNote(notes []any, st ReplayStep, n int) []any {
	if n == st.Attempts {
		return notes
	}
	return append(notes, map[string]any{"opno": st.Op.No, "action": st.Op.Action, "volume": st.Op.Volume,
		"note": fmt.Sprintf("attempts: recorded %d, replayed %d", st.Attempts, n)})
}

// replaySchedule reads and checks a recorded fault_schedule against its header.
func replaySchedule(header, res map[string]any, d Driver) ([]FaultEvent, error) {
	events, err := parseFaultEvents(res)
	if err != nil {
		return nil, err
	}
	ops, _ := header["ops"].(float64)
	within, _ := header["fault_restart_within"].(float64)
	if res["seed"] != header["seed"] {
		return nil, fmt.Errorf("fault_schedule seed %v, header seed %v", res["seed"], header["seed"])
	}
	if err := CheckFaultSchedule(events, int(ops), int(within), d.ValidProcess); err != nil {
		return nil, fmt.Errorf("fault_schedule: %w", err)
	}
	master, _, ok := d.MasterOutage()
	for _, e := range events {
		if e.Action == FaultMasterOutage && (!ok || e.Process != master) {
			return nil, fmt.Errorf("fault_schedule: a master outage of %s, the product's master is %q", e.Process, master)
		}
		if e.Action != FaultMasterOutage && ok && e.Process == master {
			return nil, fmt.Errorf("fault_schedule: an ordinary %s of the master %s", e.Action, master)
		}
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
	if f, present := r.Result["forced"]; present {
		if o.Forced, ok = f.(string); !ok {
			return Op{}, fmt.Errorf("gen_step %d: forced %v", o.No, f)
		}
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
		fr = newFaultRun(r, events, nil, header, sc.Outages)
		fr.kills = sc.Kills
		fr.outageSkips = sc.OutageSkips
		fr.inOpRecs = sc.InOp
		fr.stopOnUnhealthy = true
	}
	// The coverage tail's steps ran after the run's end (the final healthy
	// point): the replay runs them there too.
	var tail []ReplayStep
	for len(steps) > 0 && steps[len(steps)-1].Op.Forced == "tail" {
		tail = append([]ReplayStep{steps[len(steps)-1]}, tail...)
		steps = steps[:len(steps)-1]
	}
	diffs := []replayDiff{}
	var notes []any // a rerun's attempt count that differs: a note, not a difference
	var hitDiffs []*inOpHitDiff
	tainted := map[string]*inOpHitDiff{} // volume -> the in-op fault whose differing hit its later steps follow
	for i, st := range steps {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%s: %w at operation %d", r.Name, ctx.Err(), st.Op.No)
		}
		o := st.Op
		replayed, n, err := fr.stepOpN(ctx, actx, r, o)
		if err != nil {
			return nil, fmt.Errorf("%s: operation %d (%s %s): %w", r.Name, o.No, o.Action, o.Volume, err)
		}
		notes = attemptNote(notes, st, n)
		var differs *replayDiff
		if replayed != st.Recorded {
			rec := st.Recorded
			if rec == "" {
				rec = "none"
			}
			differs = &replayDiff{OpNo: o.No, Action: o.Action, Volume: o.Volume, Recorded: rec, Replayed: replayed}
		}
		var afterErr error
		if fr != nil {
			fr.note(o, replayed)
			afterErr = fr.afterStep(ctx, actx)
			if afterErr == nil {
				afterErr = fr.afterOp(ctx, actx, o.No)
			}
			// An in-operation fault of the night at this step: the replay's
			// decisions must match; a differing hit is its own line, and the
			// step's outcome follows it.
			if night, ok := sc.InOp[o.No]; ok {
				got, fired := fr.inOpSeen[o.No]
				if why := inOpDecision(night, got, fired); why != "" {
					d := replayDiff{OpNo: o.No, Action: o.Action, Volume: o.Volume, Recorded: st.Recorded, Replayed: replayed + " (" + why + ")"}
					differs = &d
				} else if got.Hit != night.Hit {
					hd := &inOpHitDiff{OpNo: o.No, Action: o.Action, Volume: o.Volume, Fault: night.Action, NightHit: night.Hit, ReplayHit: got.Hit,
						NightOutcome: st.Recorded, ReplayOutcome: replayed}
					hitDiffs = append(hitDiffs, hd)
					tainted[o.Volume] = hd
					differs = nil
				}
			} else if differs != nil && tainted[o.Volume] != nil {
				// Downstream of a differing hit on the same volume.
				tainted[o.Volume].Downstream = append(tainted[o.Volume].Downstream, *differs)
				differs = nil
			}
		}
		if differs != nil {
			diffs = append(diffs, *differs)
		}
		if fr != nil {
			if err := afterErr; errors.Is(err, errUnhealthy) {
				if err := stopRun(actx, o.No); err != nil {
					return nil, err
				}
				// The steps the replay no longer runs differ from the record.
				for _, rest := range steps[i+1:] {
					diffs = append(diffs, replayDiff{OpNo: rest.Op.No, Action: rest.Op.Action, Volume: rest.Op.Volume, Recorded: rest.Recorded, Replayed: "not run"})
				}
				break
			} else if err != nil {
				return nil, fmt.Errorf("%s: %w", r.Name, err)
			}
		}
	}
	if fr != nil {
		if err := fr.finish(ctx, actx); err != nil {
			return nil, fmt.Errorf("%s: end of run: %w", r.Name, err)
		}
	}
	for _, st := range tail {
		o := st.Op
		replayed, n, err := r.step(ctx, actx, o)
		if err != nil {
			return nil, fmt.Errorf("%s: tail operation %d (%s %s): %w", r.Name, o.No, o.Action, o.Volume, err)
		}
		notes = attemptNote(notes, st, n)
		if replayed != st.Recorded {
			rec := st.Recorded
			if rec == "" {
				rec = "none"
			}
			diffs = append(diffs, replayDiff{OpNo: o.No, Action: o.Action, Volume: o.Volume, Recorded: rec, Replayed: replayed})
		}
	}
	steps = append(steps, tail...)
	if sc.Sweep != nil {
		if err := r.sweep(ctx, actx, sc.Sweep); err != nil {
			return nil, fmt.Errorf("%s: end-of-run sweep: %w", r.Name, err)
		}
	}
	dl := make([]any, 0, len(diffs))
	for _, d := range diffs {
		dl = append(dl, map[string]any{"opno": d.OpNo, "action": d.Action, "volume": d.Volume, "recorded": d.Recorded, "replayed": d.Replayed})
	}
	res := map[string]any{"steps": len(steps), "differences": dl}
	if notes != nil {
		res["notes"] = notes
	}
	if hitDiffs != nil {
		hl := make([]any, 0, len(hitDiffs))
		for _, h := range hitDiffs {
			down := make([]any, 0, len(h.Downstream))
			for _, d := range h.Downstream {
				down = append(down, map[string]any{"opno": d.OpNo, "action": d.Action, "volume": d.Volume, "recorded": d.Recorded, "replayed": d.Replayed})
			}
			hl = append(hl, map[string]any{"line": h.line(), "opno": h.OpNo, "action": h.Action, "volume": h.Volume, "fault": h.Fault,
				"night_hit": h.NightHit, "replay_hit": h.ReplayHit, "night_outcome": h.NightOutcome, "replay_outcome": h.ReplayOutcome, "downstream": down})
		}
		res["inop_hit_differs"] = hl
	}
	if err := actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: "gen_replay", Result: res}); err != nil {
		return nil, err
	}
	if len(diffs) > 0 {
		d := diffs[0]
		return nil, fmt.Errorf("%s: %d of %d steps differ; first: operation %d (%s %s) recorded %s, replayed %s",
			r.Name, len(diffs), len(steps), d.OpNo, d.Action, d.Volume, d.Recorded, d.Replayed)
	}
	checked, suffix := "not run", "_unjudged"
	if r.Judge != nil {
		cs, _ := header["checkers"].(string) // the run's required set; none recorded: every checker
		fails, err := r.judgeReplay(ctx, actx, cs)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", r.Name, err)
		}
		if len(fails) > 0 {
			return checksFailAnswer(r.Name, len(steps), fails)
		}
		checked, suffix = "pass", ""
	} else {
		why := r.Unjudged
		if why == "" {
			why = "no checker entry"
		}
		checked = "not run: " + why
		actx.Log("  %s: the replay's own history is not judged (%s): a match is answered _unjudged", r.Name, why)
	}
	if len(hitDiffs) > 0 {
		for _, h := range hitDiffs {
			actx.Log("  %s: %s", r.Name, h.line())
		}
		actx.Log("  %s: %d steps match %s, but for %d in-op fault hit(s) that differ", r.Name, len(steps), src, len(hitDiffs))
		return map[string]string{"value": "match_inop_hit_differs" + suffix, "steps": strconv.Itoa(len(steps)), "inop_hit_differs": strconv.Itoa(len(hitDiffs)), "checks": checked}, nil
	}
	actx.Log("  %s: %d steps match %s", r.Name, len(steps), src)
	return map[string]string{"value": "match" + suffix, "steps": strconv.Itoa(len(steps)), "inop_hit_differs": "0", "checks": checked}, nil
}
