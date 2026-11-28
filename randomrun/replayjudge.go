package randomrun

// The replay judges its own history (the architect's shape (a)): after its
// steps, the replay closes a copy of its history (the records so far and an
// end record, as Close writes it) and hands it to the Runner's Judge, the
// SAME entry the pack's judge step calls, with the run header's checker
// set (the one list, recorded in every run's header). Each required checker
// must PASS:
//   - a replay whose steps match, or whose only differences are in-op
//     faults whose hit differs (match_inop_hit_differs), answers that only
//     with every required checker PASS on its own history;
//   - otherwise it answers "replay_checks_fail", naming each failing
//     checker and its kind (a violation is a product finding, not a replay
//     mismatch; "differs" keeps meaning the driver or another record
//     diverged). The kind is "unknown" until the checkers report it.
//
// Without a Judge (a Runner that sets none) nothing is judged, and a match
// is never answered as one: "match_unjudged" or
// "match_inop_hit_differs_unjudged", with "checks" "not run: <why>" (the
// Runner's Unjudged). "match" always means judged.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	tr "github.com/pingqiu/sw-test-runner"
)

// CheckerVerdict is one checker's verdict on a history.
type CheckerVerdict struct {
	Verdict  string   `json:"verdict"`
	Findings []string `json:"findings"`
}

// JudgeFunc runs the pack's checkers on a closed history: every checker's
// verdict by name. checkers is the run header's required set ("" for every
// checker).
type JudgeFunc func(ctx context.Context, history, checkers string) (map[string]CheckerVerdict, error)

// CheckKindUnknown is a failing checker's kind until the checkers say
// whether a finding is a violation or a coverage gap.
const CheckKindUnknown = "unknown"

// checkFail is a required checker that did not PASS on the replay's history.
type checkFail struct {
	Checker, Verdict, Kind string
	Findings               []string
}

// closedCopy writes the history so far with an end record after it (the
// record Close writes) to path, so a checker can read it whole.
func closedCopy(h *tr.History, path string) error {
	b, err := os.ReadFile(h.Path())
	if err != nil {
		return err
	}
	var last struct {
		Seq uint64 `json:"seq"`
	}
	lines := bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n"))
	if len(lines) == 0 || json.Unmarshal(lines[len(lines)-1], &last) != nil {
		return fmt.Errorf("history %s: no last record", h.Path())
	}
	end, err := json.Marshal(tr.HistoryRecord{Seq: last.Seq + 1, Kind: tr.HistoryEnd, MonoNs: h.Now(), Result: map[string]any{"open_invocations": h.Open()}})
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(append(bytes.TrimRight(b, "\n"), '\n'), append(end, '\n')...), 0o644)
}

// judgeReplay runs the Judge on a closed copy of the replay's history and
// answers the required checkers that did not PASS (nil when all did), and
// records them ("gen_replay_checks").
func (r Runner) judgeReplay(ctx context.Context, actx *tr.ActionContext, checkers string) ([]checkFail, error) {
	path := actx.History.Path() + ".judged.jsonl"
	if err := closedCopy(actx.History, path); err != nil {
		return nil, err
	}
	verdicts, err := r.Judge(ctx, path, checkers)
	if err != nil {
		return nil, fmt.Errorf("judging the replay's history: %w", err)
	}
	required := strings.FieldsFunc(checkers, func(c rune) bool { return c == ',' || c == ' ' })
	if len(required) == 0 {
		for name := range verdicts {
			required = append(required, name)
		}
	}
	sort.Strings(required)
	var fails []checkFail
	all := map[string]any{}
	for name, v := range verdicts {
		all[name] = v.Verdict
	}
	var fl []any
	for _, name := range required {
		v, ok := verdicts[name]
		if !ok {
			v = CheckerVerdict{Verdict: "missing", Findings: []string{"the checker gave no verdict"}}
		}
		if v.Verdict == "PASS" {
			continue
		}
		f := checkFail{Checker: name, Verdict: v.Verdict, Kind: CheckKindUnknown, Findings: v.Findings}
		fails = append(fails, f)
		fl = append(fl, map[string]any{"checker": name, "verdict": v.Verdict, "kind": f.Kind, "findings": v.Findings})
	}
	if err := actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: "gen_replay_checks",
		Result: map[string]any{"required": required, "verdicts": all, "failing": fl, "judged": path}}); err != nil {
		return nil, err
	}
	return fails, nil
}

// checksFailAnswer is the replay's answer when a required checker failed.
func checksFailAnswer(name string, steps int, fails []checkFail) (map[string]string, error) {
	var names, kinds []string
	for _, f := range fails {
		names = append(names, f.Checker+" "+f.Verdict)
		kinds = append(kinds, f.Checker+":"+f.Kind)
	}
	out := map[string]string{"value": "replay_checks_fail", "steps": fmt.Sprint(steps), "failing": strings.Join(names, ", "), "kinds": strings.Join(kinds, ", ")}
	return out, fmt.Errorf("%s: replay_checks_fail: %s on the replay's own history (kind %s): a product finding, not a replay mismatch",
		name, strings.Join(names, ", "), strings.Join(kinds, ", "))
}
