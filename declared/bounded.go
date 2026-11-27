package declared

// The operation frame of the step kinds (device, process, sequence): one
// operation is its invoke record, the step's work under the ACTION's runner
// deadline, and its complete record. The deadline is the runner's bound on
// the whole operation, never a product bound: the work gets a context that
// ends at it, every command is bounded by that context, and an operation
// that has not answered ok by then is completed unknown, "timeout after
// <n>s", its last detail kept as detail_at_deadline.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

// opDeadline is the action's runner deadline for this step: its declared
// seconds plus the int param PlusParam names (its default when absent or
// not an int), as the test hook OpDeadline may replace it.
func (x *Executor) opDeadline(s *scope, a *Action) time.Duration {
	d := time.Duration(a.RunnerDeadlineSeconds) * time.Second
	if p := a.PlusParam; p != nil {
		n, err := strconv.Atoi(s.params[p.Name])
		if err != nil {
			n = p.Default
		}
		d += time.Duration(max(n, 0)) * time.Second
	}
	if x.OpDeadline != nil {
		d = x.OpDeadline(d)
	}
	return d
}

// RunnerDeadline is the declared runner deadline of an action of a step
// kind for a step with params; false for the other kinds.
func (a *Action) RunnerDeadline(params map[string]string) (time.Duration, bool) {
	if a.RunnerDeadlineSeconds <= 0 {
		return 0, false
	}
	d := time.Duration(a.RunnerDeadlineSeconds) * time.Second
	if p := a.PlusParam; p != nil {
		n, err := strconv.Atoi(params[p.Name])
		if err != nil {
			n = p.Default
		}
		d += time.Duration(max(n, 0)) * time.Second
	}
	return d, true
}

// runValues reads the description's run values into {{run.<name>}}.
func (x *Executor) runValues(s *scope, actx *tr.ActionContext, act tr.Action) error {
	if len(x.File.RunValues) == 0 {
		return nil
	}
	if x.Values == nil {
		return fmt.Errorf("the description names run values and the product supplies none")
	}
	vals, err := x.Values(actx, act)
	if err != nil {
		return err
	}
	for _, r := range x.File.RunValues {
		v, ok := vals[r.Name]
		if !ok {
			return fmt.Errorf("run value %q not supplied", r.Name)
		}
		s.vals["run."+r.Name] = v
	}
	return nil
}

func (x *Executor) poll() time.Duration {
	if x.Poll > 0 {
		return x.Poll
	}
	return 200 * time.Millisecond
}

// recordOf is the invocation's subject name and args (with the step's
// declared expect).
func (x *Executor) recordOf(s *scope, a *Action, expect string) (string, map[string]any, error) {
	vol, err := s.expand(a.Record.Volume)
	if err != nil {
		return "", nil, fmt.Errorf("%s: %w", a.Action, err)
	}
	args := map[string]any{}
	for _, k := range sortedKeys2(a.Record.Args) {
		val := a.Record.Args[k]
		t, err := s.expand(val.Template)
		if err != nil {
			if val.Optional {
				continue
			}
			return "", nil, fmt.Errorf("%s: %w", a.Action, err)
		}
		if val.Optional && t == "" {
			continue
		}
		switch val.Type {
		case "int":
			n, err := strconv.Atoi(t)
			if err != nil {
				if val.Optional {
					continue
				}
				return "", nil, fmt.Errorf("%s: arg %s %q is not an int", a.Action, k, t)
			}
			args[k] = n
		case "bool":
			b, err := strconv.ParseBool(t)
			if err != nil {
				return "", nil, fmt.Errorf("%s: arg %s %q is not a bool", a.Action, k, t)
			}
			args[k] = b
		default:
			args[k] = t
		}
	}
	if expect != "" {
		args["expect"] = expect
	}
	return vol, args, nil
}

// bounded records one operation: the watcher's look, the invoke, do under
// the runner deadline, the timeout rule, the complete and the log line.
func (x *Executor) bounded(s *scope, actx *tr.ActionContext, act tr.Action, a *Action, vol string, args map[string]any, do func(context.Context) (string, map[string]any)) (map[string]string, error) {
	if err := Check(s.ctx, s.node, actx.History, false); err != nil {
		return nil, err
	}
	seq, err := actx.History.Invoke(tr.HistoryRecord{Client: s.vals["config."+x.File.Config.Client], Op: a.Op, Verb: a.Verb, Volume: vol, Args: args})
	if err != nil {
		return nil, err
	}
	deadline := x.opDeadline(s, a)
	octx, cancel := context.WithTimeout(WithOp(s.ctx, OpRef{History: actx.History, Seq: seq, Volume: vol}), deadline)
	outcome, result := do(octx)
	timedOut := octx.Err() == context.DeadlineExceeded && s.ctx.Err() == nil
	cancel()
	if timedOut && outcome != tr.OutcomeOK {
		if result == nil {
			result = map[string]any{}
		}
		if d, ok := result["detail"]; ok {
			result["detail_at_deadline"] = d
		}
		result["detail"] = fmt.Sprintf("timeout after %ds", int(deadline/time.Second))
		outcome = tr.OutcomeUnknown
	}
	if err := actx.History.Complete(seq, outcome, result); err != nil {
		return nil, err
	}
	actx.Log("  %s(%s): %s", a.Action, vol, outcome)
	return map[string]string{"value": outcome}, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedKeys2(m map[string]Value) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// checkWords checks every template of a command's words.
func checkWords(words []Word, check func(string) error) error {
	for _, w := range words {
		if w.Raw != "" {
			if strings.Contains(w.Raw, "{{") {
				return fmt.Errorf("raw %q: raw shell text takes no template", w.Raw)
			}
			continue
		}
		if w.When != nil {
			if err := check("{{" + w.When.Field + "}}"); err != nil {
				return err
			}
			if err := checkWords(w.Words, check); err != nil {
				return err
			}
			continue
		}
		for _, t := range append([]string{w.Text}, mapValues(w.JSON)...) {
			if err := check(t); err != nil {
				return err
			}
		}
	}
	return nil
}
