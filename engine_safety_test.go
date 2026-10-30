package testrunner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestSafetyAlwaysFailureCannotPass(t *testing.T) {
	for _, ignored := range []bool{false, true} {
		t.Run(fmt.Sprint(ignored), func(t *testing.T) {
			r := NewRegistry()
			calls := 0
			r.RegisterFunc("cleanup", TierCore, func(ctx context.Context, _ *ActionContext, _ Action) (map[string]string, error) {
				calls++
				if ctx.Err() != nil {
					t.Fatal("cleanup inherited cancellation")
				}
				return nil, errors.New("exit unknown")
			})
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			s := &Scenario{Name: "cleanup", Phases: []Phase{{Name: "always", Always: true, Actions: []Action{
				{Action: "cleanup", IgnoreError: ignored}, {Action: "cleanup"},
			}}}}
			got := NewEngine(r, nil).Run(ctx, s, &ActionContext{})
			if got.Status != StatusFail || calls != 2 || !strings.Contains(got.Error, "exit unknown") {
				t.Fatalf("cleanup must settle all and fail: result=%+v calls=%d", got, calls)
			}
		})
	}
}

func TestSafetyIgnoredCleanupErrorStillFails(t *testing.T) {
	r := NewRegistry()
	r.RegisterFunc("cleanup", TierCore, func(context.Context, *ActionContext, Action) (map[string]string, error) {
		return nil, errors.New("not settled")
	})
	s := &Scenario{Name: "ignored-cleanup", Phases: []Phase{{Name: "cleanup", Always: true, Actions: []Action{{Action: "cleanup", IgnoreError: true}}}}}
	if got := NewEngine(r, nil).Run(context.Background(), s, &ActionContext{}); got.Status != StatusFail {
		t.Fatalf("ignored cleanup qualified: %+v", got)
	}
}

func TestSafetyParallelOutputsAreIsolated(t *testing.T) {
	r := NewRegistry()
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	r.RegisterFunc("write", TierCore, func(_ context.Context, a *ActionContext, act Action) (map[string]string, error) {
		a.Vars[act.Params["key"]] = act.Params["key"]
		arrived <- struct{}{}
		<-release
		if len(a.Vars) != 3 {
			return nil, fmt.Errorf("sibling output visible: %v", a.Vars)
		}
		return map[string]string{"value": a.Vars["input"]}, nil
	})
	go func() { <-arrived; <-arrived; close(release) }()
	s := &Scenario{Name: "parallel", Phases: []Phase{{Name: "writers", Parallel: true, Actions: []Action{
		{Action: "write", Params: map[string]string{"key": "a"}, SaveAs: "out_a"},
		{Action: "write", Params: map[string]string{"key": "b"}, SaveAs: "out_b"},
	}}}}
	a := &ActionContext{Vars: map[string]string{"input": "before"}}
	got := NewEngine(r, nil).Run(context.Background(), s, a)
	if got.Status != StatusPass || a.Vars["a"] != "a" || a.Vars["b"] != "b" || a.Vars["out_b"] != "before" {
		t.Fatalf("isolated outputs not merged: %+v %v", got, a.Vars)
	}
}

func TestSafetyParallelConflictsFailWithoutPublishing(t *testing.T) {
	r := NewRegistry()
	r.RegisterFunc("write", TierCore, func(_ context.Context, a *ActionContext, act Action) (map[string]string, error) {
		a.Vars["shared"] = act.Params["value"]
		return nil, nil
	})
	s := &Scenario{Name: "conflict", Phases: []Phase{{Name: "writers", Parallel: true, Actions: []Action{
		{Action: "write", Params: map[string]string{"value": "first"}},
		{Action: "write", Params: map[string]string{"value": "second"}},
	}}}}
	a := &ActionContext{Vars: map[string]string{"shared": "original"}}
	got := NewEngine(r, nil).Run(context.Background(), s, a)
	if got.Status != StatusFail || a.Vars["shared"] != "original" {
		t.Fatalf("conflict published: %+v %v", got, a.Vars)
	}
}

func TestSafetyParallelPartialOutputSurvivesForCleanup(t *testing.T) {
	r := NewRegistry()
	r.RegisterFunc("partial", TierCore, func(context.Context, *ActionContext, Action) (map[string]string, error) {
		return map[string]string{"__partial": "must settle"}, errors.New("partial setup")
	})
	cleaned := false
	r.RegisterFunc("clean", TierCore, func(_ context.Context, a *ActionContext, _ Action) (map[string]string, error) {
		cleaned = a.Vars["__partial"] == "must settle"
		return nil, nil
	})
	s := &Scenario{Name: "partial", Phases: []Phase{
		{Name: "work", Parallel: true, Actions: []Action{{Action: "partial"}}},
		{Name: "cleanup", Always: true, Actions: []Action{{Action: "clean"}}},
	}}
	got := NewEngine(r, nil).Run(context.Background(), s, &ActionContext{})
	if got.Status != StatusFail || !cleaned {
		t.Fatal("lost partial cleanup output")
	}
}

func TestSafetyPanicStillCleansAndCancelledActionDoesNotRun(t *testing.T) {
	r := NewRegistry()
	cleaned, started := false, false
	r.RegisterFunc("panic", TierCore, func(context.Context, *ActionContext, Action) (map[string]string, error) { panic("post boundary") })
	r.RegisterFunc("clean", TierCore, func(context.Context, *ActionContext, Action) (map[string]string, error) {
		cleaned = true
		return nil, nil
	})
	r.RegisterFunc("start", TierCore, func(context.Context, *ActionContext, Action) (map[string]string, error) {
		started = true
		return nil, nil
	})
	s := &Scenario{Name: "panic", Phases: []Phase{
		{Name: "work", Actions: []Action{{Action: "panic"}}},
		{Name: "cleanup", Always: true, Actions: []Action{{Action: "clean"}}},
	}}
	got := NewEngine(r, nil).Run(context.Background(), s, &ActionContext{})
	if got.Status != StatusFail || !cleaned {
		t.Fatalf("panic bypassed cleanup: %+v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Phases[0].Actions[0].Action = "start"
	got = NewEngine(r, nil).Run(ctx, s, &ActionContext{})
	if got.Status != StatusFail || started {
		t.Fatalf("cancelled action dispatched: %+v", got)
	}
}
