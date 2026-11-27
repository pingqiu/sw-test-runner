package declared

// Step sequences (kind "sequence"). An operation that is several product
// steps, each feeding the next: steps run in order, once each; a later
// step's template may name a field an earlier step's answer decoded,
// {{step.<name>.<field>}} (Parse checks that the step is earlier and
// captures the field). There are no conditionals and no loops: a step
// either holds or ends the operation.
//
// The outcome rule is not the worst step's: ChangesFrom names the first
// step that changes the product's state, and a failure at or after it makes
// the operation unknown, whatever that step's own answer decodes to, since
// the earlier steps already changed state. A failure before it is the
// failing step's own outcome (refused only where the outcome table says so).
//
// Step kinds:
//
//   - "command": Command runs; Judge decodes the call's own answer ("exit0";
//     "error_body", the product's JSON error body through the outcome table
//     by (action, path); "exit_code", the exit through the table, the code
//     as the row's code). An ok answer is then read by Answer: "json", the
//     trimmed stdout decoded against Schema (as encoding/json decodes a
//     struct of those fields; Unreadable when it does not decode), then
//     each of Checks in order (its holds, and StderrEquals against the
//     trimmed stderr), each with its own failure; or "first_line", the
//     first line of stdout matched by Pattern, each Match group equal to
//     its template. Capture names fields from paths (json) or groups.
//   - "nvme_disconnect": every kernel controller of Subsystem deleted
//     through sysfs, certified only by a scan that then finds none; the
//     disconnect is recorded as an observe record "disconnect" (nqn,
//     controllers deleted, ok) of the operation, which the checkers close
//     an unknown write's window on.
//   - "wait_gone": the process (PID, Start: templates naming its recorded
//     identity) is gone within WithinSeconds, certified by a check.
//   - "poll": Command reruns every EveryMs within WithinSeconds until it
//     exits 0 with a stdout Pattern matches (a template; substituted values
//     are quoted for the pattern); Capture names groups.
//
// Templates of a failure: {{error}} (a command that could not run),
// {{exit}}, {{detail}} and {{outcome}} (the judged answer),
// {{stdout_first_line}}, {{stderr_first_line}}, {{output_first_line}}
// (stdout and stderr joined by a space), {{why}} (a disconnect or a wait).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

// Sequence is a sequence action's steps and outcome rule.
type Sequence struct {
	// StepsOf begins the sequence with another sequence action's steps (a
	// shared hand-over); their failure details get DetailPrefix.
	StepsOf     *StepsOf  `json:"steps_of,omitempty"`
	Steps       []SeqStep `json:"steps"`
	ChangesFrom string    `json:"changes_from"`
	// Result fields recorded from the start (before any step).
	Result map[string]Value `json:"result,omitempty"`
}

// StepsOf names the sequence action whose steps come first.
type StepsOf struct {
	Action       string `json:"action"`
	DetailPrefix string `json:"detail_prefix,omitempty"`
}

// SeqStep is one step of a sequence.
type SeqStep struct {
	Name    string     `json:"name"`
	Kind    string     `json:"kind"`
	Cite    string     `json:"cite"`
	Command []Word     `json:"command,omitempty"`
	AsRoot  bool       `json:"as_root,omitempty"`
	Judge   *Judge     `json:"judge,omitempty"`
	Answer  *SeqAnswer `json:"answer,omitempty"`
	// Failure is the detail when the step does not hold; NotRun when its
	// command could not run (default Failure). ResultFailure fields are
	// added when the answer is not ok, ResultNotRun when it could not run
	// (NotRunDropsResult: the result is the detail alone);
	// ResultAnswered whenever the command answered; Result once it holds.
	Failure           string            `json:"failure"`
	NotRun            string            `json:"not_run,omitempty"`
	NotRunDropsResult bool              `json:"not_run_drops_result,omitempty"`
	ResultFailure     map[string]Value  `json:"result_failure,omitempty"`
	ResultNotRun      map[string]Value  `json:"result_not_run,omitempty"`
	ResultAnswered    map[string]Value  `json:"result_answered,omitempty"`
	Result            map[string]Value  `json:"result,omitempty"`
	OnOKSet           map[string]string `json:"on_ok_set,omitempty"`
	// nvme_disconnect
	Subsystem string `json:"subsystem,omitempty"`
	// wait_gone
	PID   string `json:"pid,omitempty"`
	Start string `json:"start,omitempty"`
	// wait_gone, poll
	WithinSeconds int `json:"within_seconds,omitempty"`
	// poll
	Pattern string            `json:"pattern,omitempty"`
	EveryMs int               `json:"every_ms,omitempty"`
	Capture map[string]string `json:"capture,omitempty"`
}

// Judge decodes a command's own answer.
type Judge struct {
	Kind   string `json:"kind"`
	Action string `json:"action,omitempty"` // the outcome table's action; default the verb
	Path   string `json:"path,omitempty"`
}

// SeqAnswer reads an ok answer.
type SeqAnswer struct {
	Kind       string                     `json:"kind"`
	Schema     map[string]json.RawMessage `json:"schema,omitempty"`
	Unreadable string                     `json:"unreadable,omitempty"`
	Checks     []SeqCheck                 `json:"checks,omitempty"`
	Pattern    string                     `json:"pattern,omitempty"`
	Match      map[string]string          `json:"match,omitempty"` // group index -> template
	Failure    string                     `json:"failure,omitempty"`
	Capture    map[string]string          `json:"capture,omitempty"` // field -> path (json) or group index
}

// SeqCheck is one check of a decoded answer, with its own failure.
type SeqCheck struct {
	Holds        []Hold `json:"holds,omitempty"`
	StderrEquals string `json:"stderr_equals,omitempty"`
	Failure      string `json:"failure"`
}

// sequenceSteps is the action's steps, those of StepsOf first, with the
// detail prefix of each.
func (f *File) sequenceSteps(a *Action) ([]SeqStep, []string, error) {
	q := a.Sequence
	var steps []SeqStep
	var prefix []string
	if so := q.StepsOf; so != nil {
		var inc *Action
		for i := range f.Actions {
			if f.Actions[i].Action == so.Action {
				inc = &f.Actions[i]
			}
		}
		if inc == nil || inc.Sequence == nil || inc.Sequence.StepsOf != nil {
			return nil, nil, fmt.Errorf("steps_of %q is not a sequence action with steps of its own", so.Action)
		}
		for _, st := range inc.Sequence.Steps {
			steps, prefix = append(steps, st), append(prefix, so.DetailPrefix)
		}
	}
	for _, st := range q.Steps {
		steps, prefix = append(steps, st), append(prefix, "")
	}
	return steps, prefix, nil
}

// checkSequences checks every sequence action once all actions are read.
func (f *File) checkSequences(keys, run map[string]bool) error {
	for i := range f.Actions {
		a := &f.Actions[i]
		if a.Kind != "sequence" {
			continue
		}
		if err := f.checkSequence(a, keys, run); err != nil {
			return fmt.Errorf("actions: %s: %w", a.Action, err)
		}
	}
	return nil
}

func (f *File) checkSequence(a *Action, keys, run map[string]bool) error {
	q := a.Sequence
	if q == nil || a.Process != nil || a.Device != nil || a.IO != nil || a.Each != "" || a.Input != nil || len(a.Variants) != 0 || len(a.Steps) != 0 {
		return errors.New("a sequence action has a sequence only")
	}
	steps, _, err := f.sequenceSteps(a)
	if err != nil {
		return err
	}
	if len(steps) == 0 {
		return errors.New("a sequence has steps")
	}
	plain, subjects := map[string]bool{}, map[string]bool{}
	for k := range keys {
		plain["config."+k] = true
	}
	for k := range run {
		plain[k] = true
	}
	for k := range a.Derived {
		plain["derived."+k] = true
	}
	for _, p := range a.Params {
		plain[p.Name] = true
		if p.Kind == "subject" {
			subjects[p.Name] = true
		}
	}
	defined := func(extra map[string]bool) func(string) error {
		return func(t string) error {
			for _, n := range names(t) {
				if plain[n] || extra[n] {
					continue
				}
				if i := strings.IndexByte(n, '.'); i > 0 && subjects[n[:i]] {
					continue
				}
				return fmt.Errorf("template %q: %q is not defined here", t, n)
			}
			return nil
		}
	}
	seen := map[string]bool{}
	captured := map[string]bool{}
	for _, v := range q.Result {
		if err := defined(nil)(v.Template); err != nil {
			return err
		}
	}
	changes := false
	failNames := map[string]bool{"error": true, "exit": true, "detail": true, "outcome": true, "stdout_first_line": true, "stderr_first_line": true, "output_first_line": true, "why": true}
	for i, st := range steps {
		at := fmt.Sprintf("step %d (%s)", i, st.Name)
		if st.Name == "" || seen[st.Name] || strings.Contains(st.Name, ".") || strings.TrimSpace(st.Cite) == "" || st.Failure == "" {
			return fmt.Errorf("%s: a step has its own name, a cite and a failure", at)
		}
		seen[st.Name] = true
		changes = changes || st.Name == q.ChangesFrom
		base := defined(captured)
		fail := map[string]bool{}
		for k := range captured {
			fail[k] = true
		}
		for k := range failNames {
			fail[k] = true
		}
		answerNames := map[string]bool{}
		switch st.Kind {
		case "command":
			if len(st.Command) == 0 || st.Judge == nil {
				return fmt.Errorf("%s: a command step has a command and a judge", at)
			}
			if err := checkWords(st.Command, base); err != nil {
				return fmt.Errorf("%s: %w", at, err)
			}
			switch st.Judge.Kind {
			case "exit0":
			case "error_body", "exit_code":
				if st.Judge.Path == "" {
					return fmt.Errorf("%s: a judge by the outcome table names its path", at)
				}
			default:
				return fmt.Errorf("%s: judge kind %q", at, st.Judge.Kind)
			}
			if an := st.Answer; an != nil {
				switch an.Kind {
				case "json":
					if _, err := structType(an.Schema); err != nil || an.Unreadable == "" || len(an.Checks) == 0 {
						return fmt.Errorf("%s: a json answer has a schema, unreadable and checks", at)
					}
					for k := range an.Schema {
						answerNames["answer."+k] = true
					}
				case "first_line":
					re, err := regexp.Compile(an.Pattern)
					if err != nil || an.Failure == "" {
						return fmt.Errorf("%s: a first_line answer has a pattern and a failure: %v", at, err)
					}
					for g, t := range an.Match {
						if n, err := strconv.Atoi(g); err != nil || n < 1 || n > re.NumSubexp() {
							return fmt.Errorf("%s: match group %q", at, g)
						}
						if err := base(t); err != nil {
							return fmt.Errorf("%s: %w", at, err)
						}
					}
					for _, g := range an.Capture {
						if n, err := strconv.Atoi(g); err != nil || n < 1 || n > re.NumSubexp() {
							return fmt.Errorf("%s: capture group %q", at, g)
						}
					}
				default:
					return fmt.Errorf("%s: answer kind %q", at, an.Kind)
				}
				af := map[string]bool{}
				for k := range fail {
					af[k] = true
				}
				for k := range answerNames {
					af[k] = true
				}
				for _, c := range an.Checks {
					for _, h := range c.Holds {
						for _, t := range []string{deref(h.Equals), h.Above, h.AtLeast} {
							if err := defined(af)(t); err != nil {
								return fmt.Errorf("%s: %w", at, err)
							}
						}
					}
					if c.Failure == "" {
						return fmt.Errorf("%s: a check has its failure", at)
					}
					for _, t := range []string{c.StderrEquals, c.Failure} {
						if err := defined(af)(t); err != nil {
							return fmt.Errorf("%s: %w", at, err)
						}
					}
				}
				for _, t := range []string{an.Unreadable, an.Failure} {
					if err := defined(af)(t); err != nil {
						return fmt.Errorf("%s: %w", at, err)
					}
				}
			}
		case "nvme_disconnect":
			if err := base(st.Subsystem); err != nil || st.Subsystem == "" {
				return fmt.Errorf("%s: a disconnect names its subsystem: %v", at, err)
			}
		case "wait_gone":
			if st.PID == "" || st.Start == "" || st.WithinSeconds <= 0 {
				return fmt.Errorf("%s: a wait names the pid, the start and within_seconds", at)
			}
			for _, t := range []string{st.PID, st.Start} {
				if err := base(t); err != nil {
					return fmt.Errorf("%s: %w", at, err)
				}
			}
		case "poll":
			if len(st.Command) == 0 || st.Pattern == "" || st.EveryMs <= 0 || st.WithinSeconds <= 0 {
				return fmt.Errorf("%s: a poll has a command, a pattern, every_ms and within_seconds", at)
			}
			if err := checkWords(st.Command, base); err != nil {
				return fmt.Errorf("%s: %w", at, err)
			}
			if err := base(st.Pattern); err != nil {
				return fmt.Errorf("%s: %w", at, err)
			}
		default:
			return fmt.Errorf("%s: step kind %q", at, st.Kind)
		}
		for _, t := range []string{st.Failure, st.NotRun} {
			if err := defined(fail)(t); err != nil {
				return fmt.Errorf("%s: %w", at, err)
			}
		}
		for _, m := range []map[string]Value{st.ResultFailure, st.ResultNotRun, st.ResultAnswered} {
			for _, v := range m {
				if err := defined(fail)(v.Template); err != nil {
					return fmt.Errorf("%s: %w", at, err)
				}
			}
		}
		// What this step captures is defined for its own result and every
		// later step.
		caps := st.Capture
		if st.Answer != nil {
			caps = st.Answer.Capture
		}
		for k := range caps {
			captured["step."+st.Name+"."+k] = true
		}
		for _, v := range st.Result {
			if err := defined(captured)(v.Template); err != nil {
				return fmt.Errorf("%s: %w", at, err)
			}
		}
		for k, t := range st.OnOKSet {
			if i := strings.IndexByte(k, '.'); i <= 0 || !subjects[k[:i]] {
				return fmt.Errorf("%s: on_ok_set %q: not <subject>.<field>", at, k)
			}
			if err := defined(captured)(t); err != nil {
				return fmt.Errorf("%s: %w", at, err)
			}
		}
	}
	if !changes {
		return fmt.Errorf("changes_from %q names no step", q.ChangesFrom)
	}
	return nil
}

// sequence runs a sequence action.
func (x *Executor) sequence(s *scope, actx *tr.ActionContext, act tr.Action, a *Action, expect string) (map[string]string, error) {
	steps, prefix, err := x.File.sequenceSteps(a)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", a.Action, err)
	}
	dirty := false
	set := func(k, val string) error {
		i := strings.IndexByte(k, '.')
		if err := s.subjects[k[:i]].Set(k[i+1:], val); err != nil {
			return err
		}
		dirty = true
		return nil
	}
	defer func() {
		if dirty {
			for _, sub := range s.subjects {
				sub.Save()
			}
		}
	}()
	vol, args, err := x.recordOf(s, a, expect)
	if err != nil {
		return nil, err
	}
	out, err := x.bounded(s, actx, act, a, vol, args, func(ctx context.Context) (string, map[string]any) {
		res := map[string]any{}
		if err := x.values(s, res, a.Sequence.Result); err != nil {
			return tr.OutcomeUnknown, map[string]any{"detail": err.Error()}
		}
		changed := false
		for i := range steps {
			st := &steps[i]
			changed = changed || st.Name == a.Sequence.ChangesFrom
			outcome, detail, held := x.seqStep(s, ctx, a, st, res, set)
			if held {
				continue
			}
			res["detail"] = prefix[i] + detail
			if changed {
				outcome = tr.OutcomeUnknown
			}
			return outcome, res
		}
		return tr.OutcomeOK, res
	})
	if err != nil {
		return nil, err
	}
	if sets, ok := a.OnOutcomeSet[out["value"]]; ok {
		for _, k := range sortedKeys(sets) {
			val, err := s.expand(sets[k])
			if err == nil {
				err = set(k, val)
			}
			if err != nil {
				return nil, fmt.Errorf("%s: on_outcome_set %s: %w", a.Action, k, err)
			}
		}
	}
	return out, nil
}

// seqStep runs one step: held, or its outcome and detail (res updated in
// place; a NotRunDropsResult step empties it).
func (x *Executor) seqStep(s *scope, ctx context.Context, a *Action, st *SeqStep, res map[string]any, set func(k, val string) error) (string, string, bool) {
	failWith := func(outcome string, extra map[string]Value, tpl string) (string, string, bool) {
		_ = x.values(s, res, extra)
		msg, _ := s.expand(tpl)
		return outcome, msg, false
	}
	done := func() (string, string, bool) {
		if err := x.values(s, res, st.Result); err != nil {
			return tr.OutcomeUnknown, err.Error(), false
		}
		for _, k := range sortedKeys(st.OnOKSet) {
			val, err := s.expand(st.OnOKSet[k])
			if err == nil {
				err = set(k, val)
			}
			if err != nil {
				return tr.OutcomeUnknown, "on_ok_set " + k + ": " + err.Error(), false
			}
		}
		return "", "", true
	}
	switch st.Kind {
	case "nvme_disconnect":
		nqn, err := s.expand(st.Subsystem)
		if err != nil {
			return tr.OutcomeUnknown, err.Error(), false
		}
		n, why := NVMeDisconnectCount(ctx, s.node, nqn)
		if ref, ok := OpOf(ctx); ok {
			var controllers any
			if n >= 0 {
				controllers = n
			}
			r := map[string]any{"nqn": nqn, "controllers": controllers, "ok": why == ""}
			if why != "" {
				r["detail"] = why
			}
			_ = ref.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "runner", Verb: "disconnect", Volume: ref.Volume, Result: r})
		}
		if why != "" {
			s.vals["why"] = why
			return failWith(tr.OutcomeUnknown, nil, st.Failure)
		}
		return done()
	case "wait_gone":
		pid, err := s.expand(st.PID)
		if err != nil {
			return tr.OutcomeUnknown, err.Error(), false
		}
		start, err := s.expand(st.Start)
		if err != nil {
			return tr.OutcomeUnknown, err.Error(), false
		}
		if why := WaitGone(ctx, s.node, pid, start, time.Duration(st.WithinSeconds)*time.Second); why != "" {
			s.vals["why"] = why
			return failWith(tr.OutcomeUnknown, nil, st.Failure)
		}
		return done()
	case "poll":
		cmd, err := s.command(st.Command)
		if err != nil {
			return tr.OutcomeUnknown, err.Error(), false
		}
		pat, err := s.expandQuoted(st.Pattern, regexp.QuoteMeta)
		if err != nil {
			return tr.OutcomeUnknown, err.Error(), false
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return tr.OutcomeUnknown, err.Error(), false
		}
		run := s.node.Run
		if st.AsRoot {
			run = s.node.RunRoot
		}
		for end := time.Now().Add(time.Duration(st.WithinSeconds) * time.Second); time.Now().Before(end); {
			stdout, _, code, err := Call(ctx, run, cmd)
			if m := re.FindStringSubmatch(stdout); err == nil && code == 0 && m != nil {
				for _, k := range sortedKeys(st.Capture) {
					g, _ := strconv.Atoi(st.Capture[k])
					if g < len(m) {
						s.vals["step."+st.Name+"."+k] = m[g]
					}
				}
				return done()
			}
			if !Pause(ctx, time.Duration(st.EveryMs)*time.Millisecond) {
				break
			}
		}
		return failWith(tr.OutcomeUnknown, nil, st.Failure)
	case "command":
		cmd, err := s.command(st.Command)
		if err != nil {
			return tr.OutcomeUnknown, err.Error(), false
		}
		run := s.node.Run
		if st.AsRoot {
			run = s.node.RunRoot
		}
		stdout, stderr, code, err := Call(ctx, run, cmd)
		s.vals["exit"], s.vals["error"] = strconv.Itoa(code), ""
		s.vals["stdout_first_line"], s.vals["stderr_first_line"] = FirstLine(stdout), FirstLine(stderr)
		s.vals["output_first_line"] = FirstLine(stdout + " " + stderr)
		if err != nil {
			s.vals["error"] = err.Error()
			if st.NotRunDropsResult {
				for k := range res {
					delete(res, k)
				}
			}
			tpl := st.NotRun
			if tpl == "" {
				tpl = st.Failure
			}
			return failWith(tr.OutcomeUnknown, st.ResultNotRun, tpl)
		}
		if err := x.values(s, res, st.ResultAnswered); err != nil {
			return tr.OutcomeUnknown, err.Error(), false
		}
		j := st.Judge
		action := j.Action
		if action == "" {
			action = a.Verb
		}
		outcome, detail := tr.OutcomeOK, ""
		switch j.Kind {
		case "exit0":
			if code != 0 {
				outcome = tr.OutcomeUnknown
			}
		case "error_body":
			outcome, detail, _ = DecodeErrorBody(x.Lookup, action, j.Path, stdout, code)
		case "exit_code":
			if code != 0 {
				outcome, _ = x.Lookup(action, j.Path, strconv.Itoa(code), "")
				if outcome == tr.OutcomeRefusedEffect {
					outcome = tr.OutcomeRefused
				}
			}
		}
		s.vals["detail"], s.vals["outcome"] = detail, outcome
		if outcome != tr.OutcomeOK {
			return failWith(outcome, st.ResultFailure, st.Failure)
		}
		an := st.Answer
		if an == nil {
			return done()
		}
		switch an.Kind {
		case "json":
			data, ok := decodeSchema(an.Schema, strings.TrimSpace(stdout))
			if !ok {
				return failWith(tr.OutcomeUnknown, nil, an.Unreadable)
			}
			for k := range an.Schema {
				s.vals["answer."+k] = valueText(data[k])
			}
			for _, c := range an.Checks {
				hold := true
				for _, h := range c.Holds {
					hold = hold && s.hold(h, data)
				}
				if c.StderrEquals != "" {
					want, err := s.expand(c.StderrEquals)
					hold = hold && err == nil && strings.TrimSpace(stderr) == want
				}
				if !hold {
					return failWith(tr.OutcomeUnknown, nil, c.Failure)
				}
			}
			for _, k := range sortedKeys(an.Capture) {
				s.vals["step."+st.Name+"."+k] = valueText(lookupPath(data, an.Capture[k]))
			}
		case "first_line":
			first, _, _ := strings.Cut(stdout, "\n")
			m := regexp.MustCompile(an.Pattern).FindStringSubmatch(first)
			ok := m != nil
			for _, g := range sortedKeys(an.Match) {
				if !ok {
					break
				}
				n, _ := strconv.Atoi(g)
				want, err := s.expand(an.Match[g])
				ok = err == nil && m[n] == want
			}
			if !ok {
				return failWith(tr.OutcomeUnknown, nil, an.Failure)
			}
			for _, k := range sortedKeys(an.Capture) {
				n, _ := strconv.Atoi(an.Capture[k])
				s.vals["step."+st.Name+"."+k] = m[n]
			}
		}
		return done()
	}
	return tr.OutcomeUnknown, "step kind " + st.Kind, false
}

// expandQuoted is expand with every substituted value passed through q (a
// value put into a pattern is quoted for it).
func (s *scope) expandQuoted(t string, q func(string) string) (string, error) {
	var b strings.Builder
	last := 0
	for _, m := range ref.FindAllStringSubmatchIndex(t, -1) {
		v, err := s.get(t[m[2]:m[3]])
		if err != nil {
			return "", err
		}
		b.WriteString(t[last:m[0]])
		b.WriteString(q(v))
		last = m[1]
	}
	b.WriteString(t[last:])
	return b.String(), nil
}
