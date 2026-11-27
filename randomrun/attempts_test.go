package randomrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

// rerunDriver declares a rerun of "delete" (request arg "request"): again
// while an attempt answers refused, at most 3 attempts.
type rerunDriver struct{ toyDriver }

func (rerunDriver) RequestArg(action string) string {
	if action == "delete" {
		return "request"
	}
	return ""
}

func (rerunDriver) Rerun(action string, _ map[string]string, n int, _ time.Duration, out map[string]string) (bool, time.Duration, string) {
	if action != "delete" || out["value"] != tr.OutcomeRefused {
		return false, 0, ""
	}
	if n >= 3 {
		return false, 0, "max_attempts"
	}
	return true, time.Millisecond, ""
}

// rerunRunner is a Runner whose "delete" answers the scripted outcomes in
// turn (the last repeats), recording each call as its own operation, and
// whose "observe" records an observation; calls counts the delete calls.
func rerunRunner(answers []string, calls *int) Runner {
	del := func(_ context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
		out := answers[min(*calls, len(answers)-1)]
		*calls++
		args := map[string]any{"request": "delete-" + act.Params["volume"]}
		if a := act.Params[AttemptParam]; a != "" {
			var n int
			_ = json.Unmarshal([]byte(a), &n)
			args[AttemptParam] = n
		}
		seq, err := actx.History.Invoke(tr.HistoryRecord{Op: "M.Del", Verb: "delete", Volume: act.Params["volume"], Args: args})
		if err != nil {
			return nil, err
		}
		return map[string]string{"value": out}, actx.History.Complete(seq, out, nil)
	}
	obs := func(_ context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
		return map[string]string{"value": "observed"}, actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Verb: "observe", Volume: act.Params["volume"]})
	}
	return Runner{Name: "t", Node: "n", Driver: rerunDriver{}, Handlers: map[string]tr.ActionHandlerFunc{"delete": del, "observe": obs}}
}

func rerunHistory(t *testing.T) (*tr.ActionContext, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "history.jsonl")
	h, err := tr.OpenHistory(p)
	if err != nil {
		t.Fatal(err)
	}
	return &tr.ActionContext{History: h, Log: func(string, ...interface{}) {}}, p
}

// shape is a history's records as "kind verb [attempt outcome]" lines.
func shape(t *testing.T, p string) []string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var r tr.HistoryRecord
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatal(err)
		}
		switch r.Kind {
		case tr.HistoryInvoke:
			out = append(out, "invoke "+r.Verb+" attempt "+jsonOf(r.Args[AttemptParam]))
		case tr.HistoryComplete:
			out = append(out, "complete "+r.Outcome)
		case tr.HistoryObserve:
			out = append(out, "observe "+r.Verb)
		}
	}
	return out
}

func jsonOf(v any) string { b, _ := json.Marshal(v); return string(b) }

// The driver re-issues a declared rerun as separate operations, observing
// the subject after each refused attempt it re-issues (the "after" of
// attempt n, the "before" of n+1); at most the declared count; the last
// attempt's outcome is the step's, and Step observes after it as after any
// refusal.
func TestDriverReissuesAndObservesAfterEachRefusedAttempt(t *testing.T) {
	for name, c := range map[string]struct {
		answers []string
		want    []string
		outcome string
	}{
		"refused, refused, ok": {[]string{"refused", "refused", "ok"}, []string{
			"observe gen_step",
			"invoke delete attempt 1", "complete refused", "observe observe",
			"invoke delete attempt 2", "complete refused", "observe observe",
			"invoke delete attempt 3", "complete ok"}, tr.OutcomeOK},
		"refused past the count": {[]string{"refused"}, []string{
			"observe gen_step",
			"invoke delete attempt 1", "complete refused", "observe observe",
			"invoke delete attempt 2", "complete refused", "observe observe",
			"invoke delete attempt 3", "complete refused", "observe observe"}, tr.OutcomeRefused},
		"an answer not rerun": {[]string{"unknown", "ok"}, []string{
			"observe gen_step",
			"invoke delete attempt 1", "complete unknown"}, tr.OutcomeUnknown},
	} {
		calls := 0
		actx, p := rerunHistory(t)
		got, err := rerunRunner(c.answers, &calls).Step(context.Background(), actx, Op{No: 1, Action: "delete", Volume: "v1", Params: map[string]string{"volume": "v1"}})
		actx.History.Close()
		if err != nil || got != c.outcome || calls != strings.Count(strings.Join(c.want, "|"), "invoke") {
			t.Fatalf("%s: %q %v after %d calls", name, got, err, calls)
		}
		if s := shape(t, p); strings.Join(s, "|") != strings.Join(c.want, "|") {
			t.Fatalf("%s: history\n%s\nwant\n%s", name, strings.Join(s, "\n"), strings.Join(c.want, "\n"))
		}
	}
}

// Red control: only the driver numbers attempts. A call that sets the
// attempt itself is a runner error that calls nothing, so a handler wrapped
// twice (a driver inside a driver) fails loudly.
func TestAttemptsRefusesACallerSetAttempt(t *testing.T) {
	calls := 0
	r := rerunRunner([]string{"refused"}, &calls)
	actx, _ := rerunHistory(t)
	defer actx.History.Close()
	_, _, err := Attempts(context.Background(), actx, r.Handlers["delete"], tr.Action{Action: "delete", Params: map[string]string{"volume": "v1", AttemptParam: "2"}}, rerunDriver{}, nil)
	if err == nil || calls != 0 {
		t.Fatalf("%v after %d calls", err, calls)
	}
	// An action with no declared rerun is one call, numbered by nobody.
	out, n, err := Attempts(context.Background(), actx, r.Handlers["observe"], tr.Action{Action: "observe", Params: map[string]string{"volume": "v1"}}, rerunDriver{}, nil)
	if err != nil || n != 0 || out["value"] != "observed" {
		t.Fatalf("observe: %v %d %v", out, n, err)
	}
}

// A replay compares each request's LAST outcome: a replay that makes
// another number of attempts matches, and says so in a note; a last outcome
// that differs is a difference (the red control).
func TestReplayNotesADifferingAttemptCount(t *testing.T) {
	calls := 0
	actx, src := rerunHistory(t)
	_ = actx.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: "gen_header", Result: map[string]any{"generator": toyDriver{}.Version(false), "seed": 1}})
	if _, err := rerunRunner([]string{"refused", "refused", "ok"}, &calls).Step(context.Background(), actx, Op{No: 1, Action: "delete", Volume: "v1", Params: map[string]string{"volume": "v1"}}); err != nil {
		t.Fatal(err)
	}
	actx.History.Close()
	sc, err := ReadScript("t", src, rerunDriver{})
	if err != nil || len(sc.Steps) != 1 || sc.Steps[0].Recorded != tr.OutcomeOK || sc.Steps[0].Attempts != 3 {
		t.Fatalf("%+v %v", sc.Steps, err)
	}

	calls = 0
	ra, rp := rerunHistory(t)
	out, err := rerunRunner([]string{"refused", "ok"}, &calls).Replay(context.Background(), ra, src)
	ra.History.Close()
	if err != nil || out["value"] != "match" || calls != 2 {
		t.Fatalf("fewer attempts: %v %v after %d calls", out, err, calls)
	}
	b, _ := os.ReadFile(rp)
	if !strings.Contains(string(b), `"note":"attempts: recorded 3, replayed 2"`) || !strings.Contains(string(b), `"differences":[]`) {
		t.Fatalf("no attempt note: %s", b)
	}

	calls = 0
	ra, _ = rerunHistory(t)
	_, err = rerunRunner([]string{"refused"}, &calls).Replay(context.Background(), ra, src)
	ra.History.Close()
	if err == nil || !strings.Contains(err.Error(), "recorded ok, replayed refused") {
		t.Fatalf("a last outcome that differs: %v", err)
	}
}
