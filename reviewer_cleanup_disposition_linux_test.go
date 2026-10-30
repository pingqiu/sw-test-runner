//go:build linux

package testrunner

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
)

type reviewerFailedRestoreExit struct {
	OwnedJobBackend
	node *fakeOwnedNode
}

func (b reviewerFailedRestoreExit) Wait(ctx context.Context, id string) (ExitInfo, error) {
	b.node.mu.Lock()
	restoration := b.node.specs[id].Restoration
	b.node.mu.Unlock()
	x, e := b.OwnedJobBackend.Wait(ctx, id)
	if e == nil && restoration {
		x.Code = 9
	}
	return x, e
}
func TestReviewerSinglePhaseMustRetainCleanupFailure(t *testing.T) {
	for attempt := 0; attempt < 100; attempt++ {
		f, run, _, backend, plan := approvedFaultFixture(t, func(f *approvedFixture, _ *CleanupGrant) {
			raw := []byte("name: single\nphases:\n  - name: only\n    actions:\n      - action: probe\n")
			if err := os.WriteFile(f.binding.Scenario.Path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			f.binding.Scenario.SHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
		})
		run.runtime.Jobs = reviewerFailedRestoreExit{OwnedJobBackend: backend, node: f.node}
		var first error
		r := NewRegistry()
		r.RegisterFunc("probe", TierCore, func(ctx context.Context, a *ActionContext, _ Action) (map[string]string, error) {
			if err := a.Safety.ApplyWitnessedFault(ctx, plan.Key(), plan); err != nil {
				return nil, err
			}
			first = a.Safety.ClearFault(context.Background(), plan.Key())
			return nil, nil
		})
		result := run.Execute(context.Background(), NewEngine(r, nil), &ActionContext{})
		if first == nil {
			t.Fatal("missing fixture error")
		}
		if result.Status != StatusFail {
			t.Fatalf("attempt=%d actual engine=%s despite cleanup_error=%v final_Close=%v", attempt, result.Status, first, run.Close())
		}
	}
}
func TestReviewerTerminalCleanupErrorStillFailsActualEngine(t *testing.T) {
	f, run, _, backend, plan := approvedFaultFixture(t, nil)
	run.runtime.Jobs = reviewerFailedRestoreExit{OwnedJobBackend: backend, node: f.node}
	var first error
	r := NewRegistry()
	r.RegisterFunc("probe", TierCore, func(ctx context.Context, a *ActionContext, _ Action) (map[string]string, error) {
		if err := a.Safety.ApplyWitnessedFault(ctx, plan.Key(), plan); err != nil {
			return nil, err
		}
		first = a.Safety.ClearFault(context.Background(), plan.Key())
		return nil, nil // Deliberately swallow the action error to test final disposition.
	})
	result := run.Execute(context.Background(), NewEngine(r, nil), &ActionContext{})
	closeErr := run.Close()
	t.Logf("first_cleanup_error=%v final_Close=%v actual_engine_status=%s engine_error=%s", first, closeErr, result.Status, result.Error)
	if first == nil {
		t.Fatal("fixture failed to exercise restore that changed state but exited nonzero")
	}
	if result.Status != StatusFail {
		t.Fatal("actual engine lost cleanup failure disposition")
	}
}
