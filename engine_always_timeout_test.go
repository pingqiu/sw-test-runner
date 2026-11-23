package testrunner

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestAlwaysTimeoutSurvivesWorkCancellationAndSettlesAll(t *testing.T) {
	s, err := Parse([]byte(`name: finalization
timeout: 1s
always_timeout: 25m
phases:
 - name: work
   actions: [{action: stop}]
 - name: cleanup
   always: true
   actions: [{action: heal}, {action: teardown}, {action: closeout}, {action: evidence}, {action: quarantine}]
`))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := NewRegistry()
	r.RegisterFunc("stop", TierCore, func(context.Context, *ActionContext, Action) (map[string]string, error) {
		cancel()
		return nil, fmt.Errorf("work failed")
	})
	var calls []string
	for _, name := range []string{"heal", "teardown", "closeout", "evidence", "quarantine"} {
		r.RegisterFunc(name, TierCore, func(ctx context.Context, _ *ActionContext, a Action) (map[string]string, error) {
			if ctx.Err() != nil {
				t.Fatalf("cleanup inherited work cancellation: %v", ctx.Err())
			}
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) < 24*time.Minute || time.Until(deadline) > 25*time.Minute {
				t.Fatalf("always budget=%v bounded=%t; want 25m", time.Until(deadline), ok)
			}
			calls = append(calls, a.Action)
			if a.Action == "heal" {
				return nil, fmt.Errorf("heal unknown")
			}
			return nil, nil
		})
	}
	result := NewEngine(r, nil).Run(ctx, s, &ActionContext{Vars: map[string]string{}})
	if result.Status != StatusFail || strings.Join(calls, ",") != "heal,teardown,closeout,evidence,quarantine" {
		t.Fatalf("status=%s calls=%v", result.Status, calls)
	}
}
