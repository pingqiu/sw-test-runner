//go:build linux

package seaweed

import (
	"context"
	"testing"
	"time"
)

func TestMaintenanceAdmissionCannotResetExpiredOrShortBudget(t *testing.T) {
	now := time.Now()
	m := &maintenanceRuntime{budget: maintenanceBudget{work: now.Add(1200 * time.Second), hard: now.Add(1500 * time.Second)}}
	for _, remaining := range []time.Duration{-time.Second, 284 * time.Second, 300 * time.Second} {
		ctx, cancel := context.WithDeadline(context.Background(), now.Add(remaining))
		if err := m.admitCalibrationBudget(ctx, 30*time.Second); err == nil {
			t.Fatalf("admitted short original phase: %s", remaining)
		}
		cancel()
	}
	ctx, cancel := context.WithDeadline(context.Background(), now.Add(500*time.Second))
	defer cancel()
	if err := m.admitCalibrationBudget(ctx, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	m.budget.work = now.Add(700 * time.Second)
	if err := m.admitCalibrationBudget(ctx, 0); err == nil {
		t.Fatal("admitted without 240s readback before original work stop")
	}
	if m.budget.hard != now.Add(1500*time.Second) {
		t.Fatal("cleanup hard stop changed")
	}
}

func TestMaintenanceAdmissionUnfaultedLoadHasNoErrorExcusal(t *testing.T) {
	ops := []chaosOp{{Op: 0, Kind: "kv_set", Started: 1, Ended: 2, Exact: true}, {Op: 1, Kind: "s3_put", Started: 3, Ended: 4, Exact: true}}
	if err := checkMaintenanceUnfaultedOps(ops, chaosWindows{}); err != nil {
		t.Fatal(err)
	}
	ops[1].Exact = false
	if err := checkMaintenanceUnfaultedOps(ops, chaosWindows{Marks: []float64{2}}); err == nil {
		t.Fatal("non-admitted fault excused a failed load op")
	}
}
