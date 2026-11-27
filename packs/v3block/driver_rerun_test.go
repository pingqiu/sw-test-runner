package v3block

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

// recordShape is a run's records as "kind verb volume [attempt] [outcome]".
func recordShape(r eqRun) []string {
	var out []string
	for _, rec := range r.records {
		switch rec.Kind {
		case tr.HistoryInvoke:
			out = append(out, fmt.Sprintf("invoke %s %s attempt %v", rec.Verb, rec.Volume, rec.Args["attempt"]))
		case tr.HistoryComplete:
			out = append(out, "complete "+rec.Outcome)
		case tr.HistoryObserve:
			out = append(out, fmt.Sprintf("observe %s %s", rec.Verb, rec.Volume))
		}
	}
	return out
}

// The delete refused twice (the lease-horizon refusal before persistence),
// then ok, as a SCRIPTED step runs it (the handler register.go registers)
// and as a random run's driver runs it (Runner.Exec over the bare
// handlers): three operations, the volume observed after each refused
// attempt the driver re-issues. Red control (f): a registration that
// bypasses the driver makes one call.
func TestDriverObservesAfterEachRefusedAttempt(t *testing.T) {
	defer func(p func(time.Duration) time.Duration) { blockActions.Pause = p }(blockActions.Pause)
	blockActions.Pause = func(time.Duration) time.Duration { return time.Millisecond }
	live := chunkErr("not_admitted", "layout: delete reservation: workspace: deletion refused before persistence: live until T")
	deleted := eqAnswer{out: `{"volume":"v1","state":"deleted"}`}
	c := eqCase{params: map[string]string{"volume": "v1"}, rules: []eqRule{{"volume-delete", []eqAnswer{live, live, deleted}}}}
	want := []string{
		"invoke delete v1 attempt 1", "complete refused", "observe observe v1",
		"invoke delete v1 attempt 2", "complete refused", "observe observe v1",
		"invoke delete v1 attempt 3", "complete ok",
	}
	deletes := func(r eqRun) int {
		n := 0
		for _, c := range r.calls {
			if strings.Contains(c, "volume-delete") {
				n++
			}
		}
		return n
	}

	reg := tr.NewRegistry()
	RegisterPack(reg)
	scripted, err := reg.Get("v3_op_delete")
	if err != nil {
		t.Fatal(err)
	}
	r := runEq(t, func(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
		act.Action = "v3_op_delete"
		return scripted.Execute(ctx, actx, act)
	}, c, true)
	if r.err != nil || r.ret["value"] != tr.OutcomeOK || deletes(r) != 3 || strings.Join(recordShape(r), "|") != strings.Join(want, "|") {
		t.Fatalf("scripted: %v %v, %d deletes, records\n%s", r.ret, r.err, deletes(r), strings.Join(recordShape(r), "\n"))
	}

	r = runEq(t, func(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
		got, err := genExec(ctx, actx, act.Node, GenOp{Action: "v3_op_delete", Volume: "v1", Params: act.Params})
		return map[string]string{"value": got}, err
	}, c, true)
	if r.err != nil || r.ret["value"] != tr.OutcomeOK || deletes(r) != 3 || strings.Join(recordShape(r), "|") != strings.Join(want, "|") {
		t.Fatalf("random run: %v %v, %d deletes, records\n%s", r.ret, r.err, deletes(r), strings.Join(recordShape(r), "\n"))
	}

	// The red control: the bare handler (no driver) is one attempt.
	r = runEq(t, blockActions.Handler("v3_op_delete"), c, true)
	if r.err != nil || r.ret["value"] != tr.OutcomeRefused || deletes(r) != 1 {
		t.Fatalf("bare handler: %v %v, %d deletes", r.ret, r.err, deletes(r))
	}
	// A scripted step may not number attempts itself.
	c.params = map[string]string{"volume": "v1", "attempt": "2"}
	r = runEq(t, func(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
		return scripted.Execute(ctx, actx, act)
	}, c, true)
	if r.err == nil || len(r.calls) != 0 {
		t.Fatalf("a step's attempt: %v, %d calls", r.err, len(r.calls))
	}
}

// Every registration of an action with a declared rerun goes through the
// driver, and no other: the wrapped set is the declarations'.
func TestRerunSetIsTheDeclarations(t *testing.T) {
	var reruns []string
	for _, a := range blockActions.File.Actions {
		if blockActions.RequestArg(a.Action) != "" {
			reruns = append(reruns, a.Action)
		}
	}
	if strings.Join(reruns, " ") != "v3_op_snapshot v3_op_delete" {
		t.Fatalf("declared reruns: %v", reruns)
	}
	if blockActions.RequestArg("v3_op_snapshot") != "request" || blockActions.RequestArg("v3_op_delete") != "request" {
		t.Fatal("request args")
	}
}
