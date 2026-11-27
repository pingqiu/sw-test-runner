package randomrun

import (
	"path/filepath"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
)

// A step's recorded outcome counts a rerun's attempts once, as the last
// attempt: refused, refused, ok is ok; other operations of the step still
// count by the worst.
func TestReadScriptCountsTheLastAttempt(t *testing.T) {
	p := filepath.Join(t.TempDir(), "h.jsonl")
	h, err := tr.OpenHistory(p)
	if err != nil {
		t.Fatal(err)
	}
	d := toyDriver{}
	_ = h.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: "gen_header", Result: map[string]any{"generator": d.Version(false)}})
	opno := 0
	step := func(params map[string]any) {
		opno++
		_ = h.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: "gen_step", Result: map[string]any{"action": "x", "opno": opno, "params": params}})
	}
	op := func(args map[string]any, out string) {
		seq, _ := h.Invoke(tr.HistoryRecord{Op: "M.A", Verb: "delete", Volume: "v1", Args: args})
		_ = h.Complete(seq, out, nil)
	}
	step(map[string]any{"volume": "v1"})
	for n, out := range []string{tr.OutcomeRefused, tr.OutcomeRefused, tr.OutcomeOK} {
		op(map[string]any{"request": "delete-v1", "attempt": n + 1}, out)
	}
	step(map[string]any{"volume": "v1"})
	op(map[string]any{"request": "delete-v1", "attempt": 1}, tr.OutcomeOK)
	op(map[string]any{"lba": 0}, tr.OutcomeUnknown)
	h.Close()
	s, err := ReadScript("t", p, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Steps) != 2 || s.Steps[0].Recorded != tr.OutcomeOK || s.Steps[1].Recorded != tr.OutcomeUnknown {
		t.Fatalf("%+v", s.Steps)
	}
}
