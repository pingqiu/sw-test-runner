package randomrun

import (
	"context"
	"path/filepath"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
)

// toyDriver is a product of one subject kind with process p0..p9, whose
// actions are named after their verbs.
type toyDriver struct{}

func (toyDriver) Version(faults bool) string {
	if faults {
		return "toy-f/1"
	}
	return "toy/1"
}
func (toyDriver) Calls(o Op) []map[string]string { return []map[string]string{o.Params} }
func (toyDriver) Observe(s string) Op {
	return Op{Action: "observe", Volume: s, Params: map[string]string{"volume": s}}
}
func (toyDriver) Resolution(o Op) (*Op, []string) { return nil, []string{o.Volume} }
func (toyDriver) Fault(e FaultEvent) Op {
	return Op{Action: e.Action, Params: map[string]string{"process": e.Process}}
}
func (toyDriver) Healthy(string) Op                 { return Op{Action: "healthy", Params: map[string]string{}} }
func (toyDriver) ValidProcess(p string) bool        { return testProcess(p) }
func (toyDriver) MasterOutage() (string, int, bool) { return "p0", 10, true }
func (toyDriver) Look() Op {
	return Op{Action: "healthy", Params: map[string]string{"look": LookSingle}}
}
func (toyDriver) OutageRestart() Op {
	return Op{Action: "restart", Params: map[string]string{"process": "p0"}}
}

// The contract's names are pinned: a product's handlers write them.
func TestHandlerVerbContractIsPinned(t *testing.T) {
	for _, c := range [][2]string{{VerbKill, "kill"}, {VerbRestart, "restart"}, {VerbHealthy, "healthy"},
		{LookSingle, "single"}, {HealthyKey, "healthy"}, {HealthyTrue, "true"}, {VerbObserve, "observe"}} {
		if c[0] != c[1] {
			t.Errorf("contract name %q, pinned %q", c[0], c[1])
		}
	}
}

// Replay ends a step at a fault's invoke and at a healthy observation: their
// completions are not the step's. A handler that writes another verb breaks
// it (the red control: the kill's unknown is taken as the step's outcome).
func TestReplayReadsFaultsAndHealthyByTheirVerbs(t *testing.T) {
	write := func(kill, restart, healthy string) string {
		p := filepath.Join(t.TempDir(), "history.jsonl")
		h, err := tr.OpenHistory(p)
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		obs := func(verb string, res map[string]any) {
			_ = h.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: verb, Result: res})
		}
		op := func(verb, outcome string) {
			seq, _ := h.Invoke(tr.HistoryRecord{Op: "X." + verb, Verb: verb})
			_ = h.Complete(seq, outcome, nil)
		}
		obs("gen_header", map[string]any{"generator": "toy-f/1", "seed": "1", "ops": 2, "fault_restart_within": 20})
		obs("fault_schedule", FaultSchedule{Seed: 1, Events: []FaultEvent{{1, FaultKill, "p1"}, {2, FaultRestart, "p1"}}}.Record())
		obs("gen_step", map[string]any{"opno": 1, "action": "write", "params": map[string]any{}})
		op("write", tr.OutcomeOK)
		op(kill, tr.OutcomeUnknown)
		obs("gen_step", map[string]any{"opno": 2, "action": "write", "params": map[string]any{}})
		op("write", tr.OutcomeOK)
		_ = h.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "runner", Verb: healthy, Result: map[string]any{"healthy": true}})
		op("read", tr.OutcomeUnknown) // a resolving read after the healthy point
		op(restart, tr.OutcomeOK)
		return p
	}
	sc, err := ReadScript("toy", write(VerbKill, VerbRestart, VerbHealthy), toyDriver{})
	if err != nil {
		t.Fatal(err)
	}
	if sc.Steps[0].Recorded != tr.OutcomeOK || sc.Steps[1].Recorded != tr.OutcomeOK {
		t.Fatalf("steps %+v", sc.Steps)
	}
	for name, p := range map[string]string{
		"a kill recorded as Kill":      write("Kill", VerbRestart, VerbHealthy),
		"a healthy recorded as health": write(VerbKill, VerbRestart, "health"),
	} {
		sc, err := ReadScript("toy", p, toyDriver{})
		if err == nil && sc.Steps[0].Recorded == tr.OutcomeOK && sc.Steps[1].Recorded == tr.OutcomeOK {
			t.Errorf("red control %q: the steps read the same", name)
		}
	}
}

// Only healthy="true" is a healthy point: it resolves the pending unknowns;
// "false", or any other spelling, resolves nothing.
func TestHealthyAnswerResolvesOnlyOnTrue(t *testing.T) {
	for answer, resolves := range map[string]bool{HealthyTrue: true, "false": false, "True": false, "": false} {
		observed := 0
		r := Runner{Name: "toy", Node: "n", Driver: toyDriver{}, Handlers: map[string]tr.ActionHandlerFunc{
			"healthy": func(context.Context, *tr.ActionContext, tr.Action) (map[string]string, error) {
				return map[string]string{"value": tr.OutcomeOK, HealthyKey: answer}, nil
			},
			"observe": func(context.Context, *tr.ActionContext, tr.Action) (map[string]string, error) {
				observed++
				return map[string]string{"value": "observed"}, nil
			},
		}}
		f := newFaultRun(r, nil, nil, map[string]any{}, nil)
		f.note(Op{Action: "write", Volume: "v1", Params: map[string]string{}}, tr.OutcomeUnknown)
		if err := f.healthy(context.Background(), &tr.ActionContext{}); err != nil {
			t.Fatal(err)
		}
		if (observed == 1) != resolves || (len(f.pending) == 0) != resolves {
			t.Errorf("healthy=%q: %d observations, %d pending", answer, observed, len(f.pending))
		}
	}
}
