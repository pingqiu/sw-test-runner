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

func (rerunDriver) Placeable(string) bool { return false }
func (rerunDriver) Place(string, map[string]string, map[string]any, map[string]any) (string, string, bool) {
	return PlacedNotPlaced, "", false
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
// the subject right after each refused attempt it re-issues (the "after" of
// attempt n) and again after the pause (the "before" of n+1); at most the declared count; the last
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
			"invoke delete attempt 1", "complete refused", "observe observe", "observe observe",
			"invoke delete attempt 2", "complete refused", "observe observe", "observe observe",
			"invoke delete attempt 3", "complete ok"}, tr.OutcomeOK},
		"refused past the count": {[]string{"refused"}, []string{
			"observe gen_step",
			"invoke delete attempt 1", "complete refused", "observe observe", "observe observe",
			"invoke delete attempt 2", "complete refused", "observe observe", "observe observe",
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
	_, _, err := Attempts(context.Background(), actx, r.Handlers["delete"], tr.Action{Action: "delete", Params: map[string]string{"volume": "v1", AttemptParam: "2"}}, rerunDriver{}, "v1", nil)
	if err == nil || calls != 0 {
		t.Fatalf("%v after %d calls", err, calls)
	}
	// An action with no declared rerun is one call, numbered by nobody.
	out, n, err := Attempts(context.Background(), actx, r.Handlers["observe"], tr.Action{Action: "observe", Params: map[string]string{"volume": "v1"}}, rerunDriver{}, "v1", nil)
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

// guardDriver's guard ends the attempts after the second (the count would
// allow a third).
type guardDriver struct{ rerunDriver }

func (guardDriver) Rerun(action string, _ map[string]string, n int, _ time.Duration, out map[string]string) (bool, time.Duration, string) {
	if out["value"] != tr.OutcomeRefused {
		return false, 0, ""
	}
	if n >= 2 {
		return false, 0, "guard"
	}
	return true, time.Millisecond, ""
}

// A guard trip is its own record: an observe record rerun_guard on the
// subject with the action, the attempt it ended at and the elapsed time.
// An end by the count writes none (TestDriverReissuesAndObserves...).
func TestDriverRecordsAGuardTrip(t *testing.T) {
	calls := 0
	r := rerunRunner([]string{"refused"}, &calls)
	r.Driver = guardDriver{}
	actx, p := rerunHistory(t)
	got, err := r.Exec(context.Background(), actx, Op{Action: "delete", Volume: "v1", Params: map[string]string{"volume": "v1"}})
	actx.History.Close()
	if err != nil || got != tr.OutcomeRefused || calls != 2 {
		t.Fatalf("%q %v after %d calls", got, err, calls)
	}
	b, _ := os.ReadFile(p)
	var guard *tr.HistoryRecord
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var rec tr.HistoryRecord
		_ = json.Unmarshal([]byte(l), &rec)
		if rec.Verb == VerbRerunGuard {
			guard = &rec
		}
	}
	if guard == nil || guard.Kind != tr.HistoryObserve || guard.Volume != "v1" || guard.Result["action"] != "delete" || guard.Result["attempt"] != float64(2) {
		t.Fatalf("guard record %+v in %s", guard, b)
	}
	if _, ok := guard.Result["elapsed_ms"].(float64); !ok {
		t.Fatalf("no elapsed_ms: %+v", guard.Result)
	}
}

// Took is the first attempt's own window in the history, from its
// invocation record to its completion record: the time a handler spends
// before it records the invocation is not in it (a kill timed into the
// operation runs from that record). Red control: a handler that works
// 120 ms before its invocation and 20 ms inside it measures about 20 ms,
// not 140.
func TestAttemptsWithTookIsTheRecordWindow(t *testing.T) {
	actx, _ := rerunHistory(t)
	defer actx.History.Close()
	h := func(_ context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
		time.Sleep(120 * time.Millisecond)
		seq, err := actx.History.Invoke(tr.HistoryRecord{Op: "X.Y", Verb: "write", Volume: "v1"})
		if err != nil {
			return nil, err
		}
		time.Sleep(20 * time.Millisecond)
		return map[string]string{"value": tr.OutcomeOK}, actx.History.Complete(seq, tr.OutcomeOK, nil)
	}
	hk := &AttemptHooks{}
	if _, _, err := AttemptsWith(context.Background(), actx, h, tr.Action{Action: "write", Params: map[string]string{}}, nil, "v1", nil, hk); err != nil {
		t.Fatal(err)
	}
	if hk.Took < 20*time.Millisecond || hk.Took >= 120*time.Millisecond {
		t.Fatalf("took %v, want the record window (about 20 ms)", hk.Took)
	}
	// recordWindow: a kill's records between are not the operation's; no
	// completion gives 0.
	recs := []tr.HistoryRecord{{Seq: 1, Kind: tr.HistoryInvoke, Verb: VerbKill, MonoNs: 5}, {Seq: 2, Kind: tr.HistoryInvoke, Verb: "write", MonoNs: 10},
		{Seq: 3, Kind: tr.HistoryComplete, Of: 1, MonoNs: 12}, {Seq: 4, Kind: tr.HistoryComplete, Of: 2, MonoNs: 40}}
	if got := recordWindow(recs); got != 30 {
		t.Errorf("window %v, want 30ns", got)
	}
	if got := recordWindow(recs[:3]); got != 0 {
		t.Errorf("window without completion %v, want 0", got)
	}
}
