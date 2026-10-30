//go:build linux

package testrunner

import (
	"context"
	"strings"
	"testing"
)

func TestApprovedTerminalCleanupFailureSurvivesExactBeforeRetry(t *testing.T) {
	f, run, resource, backend, plan := approvedFaultFixture(t, nil)
	run.runtime.Jobs = reviewerFailedRestoreExit{OwnedJobBackend: backend, node: f.node}
	scope := run.Scope()
	if err := scope.ApplyWitnessedFault(context.Background(), plan.Key(), plan); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := scope.ClearFault(context.Background(), plan.Key()); err == nil || !strings.Contains(err.Error(), "fault command failed") {
			t.Fatalf("clear %d erased terminal cleanup failure: %v", attempt, err)
		}
	}
	if !scope.FaultReceipts()[plan.Key()].RestoreNotNeeded {
		t.Fatal("retry did not verify qualified Already-Before")
	}
	resource.mu.Lock()
	state := resource.state
	resource.mu.Unlock()
	if state != "UP" {
		t.Fatalf("fixture did not restore state: %s", state)
	}
	f.node.mu.Lock()
	restores := 0
	for _, spec := range f.node.specs {
		if spec.Restoration {
			restores++
		}
	}
	f.node.mu.Unlock()
	if restores != 1 {
		t.Fatalf("retry issued another restore command: %d", restores)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := run.Close(); err == nil || !strings.Contains(err.Error(), "fault command failed") {
			t.Fatalf("close %d erased terminal cleanup failure: %v", attempt, err)
		}
	}
}
