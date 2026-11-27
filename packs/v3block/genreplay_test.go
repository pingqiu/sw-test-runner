package v3block

import (
	"context"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
)

func replayAct(src string) tr.Action {
	return tr.Action{Action: "v3_gen_replay", Node: "host", Params: map[string]string{"history": src}}
}

// recordRun records a v3_gen_run with the installed fake into a closed history A.
func recordRun(t *testing.T, ops string) string {
	t.Helper()
	actx, p := genRunCtx(t)
	if _, err := v3GenRun(context.Background(), actx, genAct(ops)); err != nil {
		t.Fatal(err)
	}
	if err := actx.History.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// replayInto replays src into a fresh history, closes it and returns its path.
func replayInto(t *testing.T, src string) (map[string]string, string, error) {
	t.Helper()
	actx, p := genRunCtx(t)
	out, err := v3GenReplay(context.Background(), actx, replayAct(src))
	_ = actx.History.Close()
	return out, p, err
}

func stepsOf(t *testing.T, p string) map[int]map[string]any {
	t.Helper()
	m := map[int]map[string]any{}
	for _, r := range records(t, p) {
		if r.Verb == "gen_step" {
			m[int(r.Result["opno"].(float64))] = r.Result
		}
	}
	return m
}

func lastVerb(t *testing.T, p, verb string) tr.HistoryRecord {
	t.Helper()
	var got tr.HistoryRecord
	for _, r := range records(t, p) {
		if r.Verb == verb {
			got = r
		}
	}
	if got.Verb == "" {
		t.Fatalf("%s has no %s record", p, verb)
	}
	return got
}

func TestGenReplayOfARecordedRunMatches(t *testing.T) {
	f := &fakeProduct{}
	f.install(t)
	a := recordRun(t, "300")
	out, b, err := replayInto(t, a)
	if err != nil {
		t.Fatal(err)
	}
	sa, sb := stepsOf(t, a), stepsOf(t, b)
	if out["value"] != "match" || out["steps"] != strconv.Itoa(len(sa)) || len(sa) != 300 {
		t.Fatalf("out %v, %d steps", out, len(sa))
	}
	if !reflect.DeepEqual(sa, sb) {
		t.Fatal("the replayed steps differ from the recorded ones")
	}
	hdr := records(t, b)[0]
	if hdr.Verb != "gen_header" || hdr.Result["replay_of"] != a || hdr.Result["seed"] != "5" {
		t.Fatalf("replay header %+v", hdr)
	}
	rep := lastVerb(t, b, "gen_replay")
	if rep.Result["steps"] != float64(300) || len(rep.Result["differences"].([]any)) != 0 {
		t.Fatalf("gen_replay %+v", rep.Result)
	}
	// A replayed history is itself replayable, judged the same way.
	if out, c, err := replayInto(t, b); err != nil || out["value"] != "match" || !reflect.DeepEqual(stepsOf(t, c), sa) {
		t.Fatalf("replay of the replay: %v %v", out, err)
	}
}

func TestGenReplayReportsAnOutcomeThatDiffers(t *testing.T) {
	f := &fakeProduct{}
	f.install(t)
	a := recordRun(t, "300")
	// Pick the third ok attach of the recorded run and make the product refuse it.
	const action = "v3_op_attach"
	n, opno, vol := 0, 0, ""
	for _, r := range records(t, a) {
		if r.Verb == "gen_step" && r.Result["action"] == action {
			n++
			if n == 3 {
				opno, vol = int(r.Result["opno"].(float64)), r.Volume
				break
			}
		}
	}
	if opno == 0 {
		t.Fatal("the recorded run has fewer than three attaches")
	}
	inner, calls := genHandlers[action], 0
	genHandlers[action] = func(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
		if calls++; calls == 3 {
			seq, _ := actx.History.Invoke(tr.HistoryRecord{Op: "X.attach", Verb: "attach", Volume: act.Params["volume"]})
			_ = actx.History.Complete(seq, tr.OutcomeRefused, nil)
			return map[string]string{"value": tr.OutcomeRefused}, nil
		}
		return inner(ctx, actx, act)
	}
	_, b, err := replayInto(t, a)
	if err == nil || !strings.Contains(err.Error(), "operation "+strconv.Itoa(opno)+" ") || !strings.Contains(err.Error(), "1 of 300") {
		t.Fatalf("the difference at operation %d was not named: %v", opno, err)
	}
	diffs := lastVerb(t, b, "gen_replay").Result["differences"].([]any)
	want := map[string]any{"opno": float64(opno), "action": action, "volume": vol, "recorded": tr.OutcomeOK, "replayed": tr.OutcomeRefused}
	if len(diffs) != 1 || !reflect.DeepEqual(diffs[0], want) {
		t.Fatalf("differences %v, want [%v]", diffs, want)
	}
	if len(stepsOf(t, b)) != 300 {
		t.Fatal("the replay did not go through every step")
	}
}

// writeHistory builds a history file by hand.
func writeHistory(t *testing.T, fill func(h *tr.History)) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "history.jsonl")
	h, err := tr.OpenHistory(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	fill(h)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func obs(h *tr.History, verb, vol string, res map[string]any) {
	_ = h.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: verb, Volume: vol, Result: res})
}

func opRec(h *tr.History, verb, vol, outcome string) {
	seq, _ := h.Invoke(tr.HistoryRecord{Op: "X." + verb, Verb: verb, Volume: vol})
	_ = h.Complete(seq, outcome, nil)
}

// The recorded outcome of a step is the WORST of its completions, not the
// first: a write split per lba whose second lba was unknown is unknown.
func TestGenReplayRecordedOutcomeIsTheWorstCompletion(t *testing.T) {
	p := writeHistory(t, func(h *tr.History) {
		obs(h, "gen_header", "", map[string]any{"generator": GenVersion, "seed": "1"})
		obs(h, "gen_step", "g1", map[string]any{"opno": 1, "action": "v3_op_write", "params": map[string]any{"volume": "g1", "lbas": "1 3", "round": "g1"}})
		opRec(h, "write", "g1", tr.OutcomeOK)
		opRec(h, "write", "g1", tr.OutcomeUnknown)
		obs(h, "gen_step", "g1", map[string]any{"opno": 2, "action": "v3_op_read", "params": map[string]any{"volume": "g1", "expect": "refused"}, "negative": "x"})
		obs(h, "observe", "g1", nil)
		opRec(h, "read", "g1", tr.OutcomeRefused)
		obs(h, "observe", "g1", nil)
		obs(h, "gen_coverage", "", nil)
		opRec(h, "read", "g1", tr.OutcomeUnknown) // after the script: no step's
	})
	_, steps, err := readReplayScript(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 || steps[0].Recorded != tr.OutcomeUnknown || steps[1].Recorded != tr.OutcomeRefused || steps[1].Op.Negative != "x" {
		t.Fatalf("steps %+v", steps)
	}
	f := &fakeProduct{failWrite: "3"}
	f.install(t)
	if out, _, err := replayInto(t, p); err != nil || out["value"] != "match" {
		t.Fatalf("%v %v", out, err)
	}
}

func TestGenReplayRefusesABadScriptBeforeAnyCall(t *testing.T) {
	step := map[string]any{"opno": 1, "action": "v3_op_create", "params": map[string]any{"volume": "g1"}}
	for name, fill := range map[string]func(h *tr.History){
		"no header": func(h *tr.History) {
			obs(h, "gen_step", "g1", step)
			opRec(h, "create", "g1", tr.OutcomeOK)
		},
		// The previous generator: its proposals differ from this one's, so its
		// histories are not replayed.
		"generator m1-gen/1": func(h *tr.History) {
			obs(h, "gen_header", "", map[string]any{"generator": "m1-gen/1"})
			obs(h, "gen_step", "g1", step)
			opRec(h, "create", "g1", tr.OutcomeOK)
		},
		"other generator": func(h *tr.History) {
			obs(h, "gen_header", "", map[string]any{"generator": "m1-gen/0"})
			obs(h, "gen_step", "g1", step)
			opRec(h, "create", "g1", tr.OutcomeOK)
		},
		"no steps": func(h *tr.History) {
			obs(h, "gen_header", "", map[string]any{"generator": GenVersion})
			opRec(h, "create", "g1", tr.OutcomeOK)
			obs(h, "gen_coverage", "", nil)
		},
		"two headers": func(h *tr.History) {
			obs(h, "gen_header", "", map[string]any{"generator": GenVersion})
			obs(h, "gen_step", "g1", step)
			opRec(h, "create", "g1", tr.OutcomeOK)
			obs(h, "gen_header", "", map[string]any{"generator": GenVersion})
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeProduct{}
			f.install(t)
			p := writeHistory(t, fill)
			_, b, err := replayInto(t, p)
			if err == nil {
				t.Fatal("accepted")
			}
			if len(f.calls) != 0 {
				t.Fatalf("%d handler calls before the refusal", len(f.calls))
			}
			for _, r := range records(t, b) {
				if r.Kind != tr.HistoryEnd {
					t.Fatalf("the refused replay wrote %+v", r)
				}
			}
		})
	}
	f := &fakeProduct{}
	f.install(t)
	if _, _, err := replayInto(t, filepath.Join(t.TempDir(), "absent.jsonl")); err == nil {
		t.Fatal("a missing file was accepted")
	}
}
