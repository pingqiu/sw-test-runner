package v3block

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/randomrun"
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
// handlers): three operations, the volume observed right after each refused
// attempt the driver re-issues and again right before the next. Red control (f): a registration that
// bypasses the driver makes one call.
func TestDriverObservesAfterEachRefusedAttempt(t *testing.T) {
	defer func(p func(time.Duration) time.Duration) { blockActions.Pause = p }(blockActions.Pause)
	blockActions.Pause = func(time.Duration) time.Duration { return time.Millisecond }
	live := chunkErr("not_admitted", "layout: delete reservation: workspace: deletion refused before persistence: live until T")
	deleted := eqAnswer{out: `{"volume":"v1","state":"deleted"}`}
	c := eqCase{params: map[string]string{"volume": "v1"}, rules: []eqRule{{"volume-delete", []eqAnswer{live, live, deleted}}}}
	want := []string{
		"invoke delete v1 attempt 1", "complete refused", "observe observe v1", "observe observe v1",
		"invoke delete v1 attempt 2", "complete refused", "observe observe v1", "observe observe v1",
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

// The COUNT ends a normal run, never the guard: each declared guard
// (within_param's default) is at least count x (period + the action's
// runner deadline), and with the slowest attempts the count allows the
// last one is still within it (Rerun answers max_attempts, not guard).
func TestRerunGuardIsAboveTheCount(t *testing.T) {
	for _, a := range blockActions.File.Actions {
		r := a.Retry
		if r == nil || r.WithinParam == "" {
			continue
		}
		guard := -1
		for _, p := range a.Params {
			if p.Name == r.WithinParam {
				guard, _ = strconv.Atoi(p.Default)
			}
		}
		deadline := 0
		for _, v := range a.Variants {
			deadline = max(deadline, v.RunnerDeadlineSeconds)
		}
		if want := r.MaxAttempts * (r.EverySeconds + deadline); guard < want {
			t.Errorf("%s: guard %s default %d is below count x (period + deadline) = %d", a.Action, r.WithinParam, guard, want)
		}
		params := map[string]string{"volume": "v1"}
		refused := map[string]string{"value": tr.OutcomeRefused, "detail": r.WhileCode + ": " + r.WhileMessageContains}
		slowest := time.Duration((r.MaxAttempts-1)*r.EverySeconds+r.MaxAttempts*deadline) * time.Second
		if again, _, why := blockActions.Rerun(a.Action, params, r.MaxAttempts-1, slowest, refused); !again {
			t.Errorf("%s: the guard ended attempt %d (%s) before the count", a.Action, r.MaxAttempts-1, why)
		}
		if _, _, why := blockActions.Rerun(a.Action, params, r.MaxAttempts, slowest, refused); why != "max_attempts" {
			t.Errorf("%s: attempt %d ended by %q, not the count", a.Action, r.MaxAttempts, why)
		}
	}
}

// A run of max_attempts refusals ends by the COUNT: 41 delete attempts, and
// no rerun_guard record (the guard did not decide).
func TestDeleteRefusalsEndByTheCount(t *testing.T) {
	defer func(p func(time.Duration) time.Duration) { blockActions.Pause = p }(blockActions.Pause)
	blockActions.Pause = func(time.Duration) time.Duration { return time.Millisecond }
	live := chunkErr("not_admitted", "layout: delete reservation: workspace: deletion refused before persistence: live until T")
	r := runEq(t, reissued("v3_op_delete"), eqCase{params: map[string]string{"volume": "v1"}, rules: []eqRule{{"volume-delete", []eqAnswer{live}}}}, true)
	max := blockActions.File.Actions[5].Retry.MaxAttempts
	if r.err != nil || r.ret["value"] != tr.OutcomeRefused || len(r.calls) != max || max != 41 {
		t.Fatalf("%v %v, %d calls of %d", r.ret, r.err, len(r.calls), max)
	}
	for _, rec := range r.records {
		if rec.Verb == randomrun.VerbRerunGuard {
			t.Fatalf("a guard record: %+v", rec)
		}
	}
}

// snapshotRerun runs one snapshot s1 of v1 through the random run's driver
// (genExec), the snapshot answering answers in turn and HEAD's snapshot
// names in turn shows (each a Snapshots object; the last repeats).
func snapshotRerun(t *testing.T, answers []eqAnswer, shows ...string) eqRun {
	t.Helper()
	defer func(p func(time.Duration) time.Duration) { blockActions.Pause = p }(blockActions.Pause)
	blockActions.Pause = func(time.Duration) time.Duration { return time.Millisecond }
	var heads []eqAnswer
	for _, s := range shows {
		heads = append(heads, eqAnswer{out: `{"head":{"Gen":5,"Epoch":2,"Holder":"r1","State":"serving","Snapshots":` + s + `}}`})
	}
	c := eqCase{params: map[string]string{"volume": "v1", "name": "s1"}, rules: []eqRule{
		{"ops chunk snapshot", answers}, {"ops chunk show", heads}}}
	return runEq(t, func(ctx context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
		got, err := genExec(ctx, actx, act.Node, GenOp{Action: "v3_op_snapshot", Volume: "v1", Params: act.Params})
		return map[string]string{"value": got}, err
	}, c, true)
}

func snapshotCalls(r eqRun) int {
	n := 0
	for _, c := range r.calls {
		if strings.Contains(c, "ops chunk snapshot ") {
			n++
		}
	}
	return n
}

// An unknown snapshot attempt with the page's answer (head_conflict) is
// never re-issued before the driver observed the volume and placed the
// attempt by the placement row (HEAD's snapshot names): did_not re-issues
// it, its "before" that observation; took_effect, third and not_placed end
// the request unknown; and with no names recorded nothing is re-issued.
func TestSnapshotUnknownIsPlacedBeforeAnyReissue(t *testing.T) {
	conflict := chunkErr("head_conflict", "storage: capture CAS not applied: storage: chunk HEAD precondition failed")
	for name, c := range map[string]struct {
		answers []eqAnswer
		shows   []string
		calls   int
		outcome string
		shape   []string
	}{
		"name absent: did_not, re-issued": {[]eqAnswer{conflict, {}}, []string{`{}`, `{}`, `{"s1":{}}`}, 2, tr.OutcomeOK, []string{
			"observe observe v1", "invoke snapshot v1 attempt 1", "complete unknown", "observe observe v1",
			"invoke snapshot v1 attempt 2", "complete ok"}},
		"name present: took_effect, not re-issued": {[]eqAnswer{conflict, {}}, []string{`{}`, `{"s1":{}}`}, 1, tr.OutcomeUnknown, []string{
			"observe observe v1", "invoke snapshot v1 attempt 1", "complete unknown", "observe observe v1"}},
		"another name lost: third, not re-issued": {[]eqAnswer{conflict, {}}, []string{`{"s0":{}}`, `{}`}, 1, tr.OutcomeUnknown, []string{
			"observe observe v1", "invoke snapshot v1 attempt 1", "complete unknown", "observe observe v1"}},
		"name there before: not_placed, not re-issued": {[]eqAnswer{conflict, {}}, []string{`{"s1":{}}`}, 1, tr.OutcomeUnknown, []string{
			"observe observe v1", "invoke snapshot v1 attempt 1", "complete unknown", "observe observe v1"}},
		"no names recorded: not re-issued": {[]eqAnswer{conflict, {}}, []string{`[1]`}, 1, tr.OutcomeUnknown, []string{
			"observe observe v1", "invoke snapshot v1 attempt 1", "complete unknown", "observe observe v1"}},
	} {
		r := snapshotRerun(t, c.answers, c.shows...)
		if r.err != nil || r.ret["value"] != c.outcome || snapshotCalls(r) != c.calls || strings.Join(recordShape(r), "|") != strings.Join(c.shape, "|") {
			t.Errorf("%s: %v %v, %d snapshot calls, records\n%s", name, r.ret, r.err, snapshotCalls(r), strings.Join(recordShape(r), "\n"))
		}
	}
	// Red control: no observation, no re-issue. The bare driver function
	// with no observe makes one call for the same unknown.
	defer func(p func(time.Duration) time.Duration) { blockActions.Pause = p }(blockActions.Pause)
	blockActions.Pause = func(time.Duration) time.Duration { return time.Millisecond }
	r := runEq(t, reissued("v3_op_snapshot"), eqCase{params: map[string]string{"volume": "v1", "name": "s1"}, rules: []eqRule{{"ops chunk snapshot", []eqAnswer{conflict, {}}}}}, true)
	if r.err != nil || r.ret["value"] != tr.OutcomeUnknown || snapshotCalls(r) != 1 {
		t.Fatalf("no observation: %v %v, %d calls", r.ret, r.err, snapshotCalls(r))
	}
}
