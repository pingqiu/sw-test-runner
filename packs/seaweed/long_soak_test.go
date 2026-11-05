package seaweed

import (
	"context"
	"strings"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

// #1002: the 2 h and 8 h gates run under seaweed-cloud direct only. The approved path, and the
// worker lease envelope it shares with core, keep the 60-minute horizon.
func TestDirectSoakHorizonAdmitsTheLongGatesOnlyOnDirect(t *testing.T) {
	if SoakHorizon != 60*time.Minute || tr.SoakHorizon != 60*time.Minute {
		t.Fatalf("the approved horizon moved: %v %v", SoakHorizon, tr.SoakHorizon)
	}
	p, _ := soakTestPlan()
	for _, d := range []int{7200, 28800, 32400} {
		p.Soak.DurationS = d
		if err := p.Soak.checkSchedule(); err == nil || !strings.Contains(err.Error(), "duration out of range") {
			t.Errorf("approved path admitted %d s: %v", d, err)
		}
		if err := p.Soak.checkScheduleWithin(DirectSoakHorizon); err != nil {
			t.Errorf("direct path refused %d s: %v", d, err)
		}
	}
	p.Soak.DurationS = 32401 // past 9 h
	if err := p.Soak.checkScheduleWithin(DirectSoakHorizon); err == nil {
		t.Fatal("direct path admitted a soak past its horizon")
	}
	if _, err := checkedSpanWithin(7200, DirectSoakHorizon); err != nil {
		t.Fatalf("a 2 h wait refused under the direct horizon: %v", err)
	}
	if _, err := checkedSpan(7200); err == nil {
		t.Fatal("a 2 h wait admitted under the approved horizon")
	}
}

// directHorizon is a fake executor that declares the direct path's horizon.
type directHorizon struct{ *fakeSoak }

func (directHorizon) Horizon() time.Duration { return DirectSoakHorizon }

// runSoak takes its horizon from the executor: a 2 h plan runs every fault and the whole load on
// an executor that declares DirectSoakHorizon, and is refused before any effect on one that doesn't.
func TestRunSoakRunsATwoHourPlanOnlyUnderTheExecutorsHorizon(t *testing.T) {
	two := func() (*fakeSoak, Plan, tr.PackInputs) {
		f := newFakeSoak(t)
		f.exitAt = f.start + 7200 + 60.01
		f.deadline = f.wall.Add(2*time.Hour + 15*time.Minute)
		p, in := soakTestPlan()
		p.Soak.DurationS = 7200
		return f, p, in
	}
	f, p, in := two()
	if _, _, err := runSoak(context.Background(), f, p, in, map[string]tr.JobHandle{}); err == nil || !strings.Contains(err.Error(), "duration out of range") || f.started != 0 {
		t.Fatalf("an approved executor ran a 2 h plan: err=%v started=%d", err, f.started)
	}
	f, p, in = two()
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	defer cancel()
	records, exit, err := runSoak(ctx, directHorizon{f}, p, in, map[string]tr.JobHandle{})
	if err != nil {
		t.Fatalf("2 h soak under the direct horizon: %v", err)
	}
	if strings.Join(f.restarts, ",") != "E1,E2" || len(records) != 2 || exit.Code != 0 {
		t.Fatalf("restarts=%v records=%d exit=%+v", f.restarts, len(records), exit)
	}
	if f.mono < f.start+7200 {
		t.Fatalf("the load ended at %v, before its 2 h end %v", f.mono, f.start+7200)
	}
}

// directSoakBudget refuses, before any deploy, an 8 h soak whose work deadline or --timeout can't hold it.
func TestDirectSoakBudgetSizesTheTimeoutAndTheWorkDeadline(t *testing.T) {
	p, _ := soakTestPlan()
	p.Soak.DurationS = 28800
	now := time.Unix(1_800_000_000, 0)
	need := DeployReserve + WitnessWait + 8*time.Hour + QuiescenceReserve + LoadExitGrace + CollectReserve
	if err := directSoakBudget(p.Soak, now, now.Add(need), now.Add(need+CleanupReserve)); err != nil {
		t.Fatalf("the exact budget was refused: %v", err)
	}
	if err := directSoakBudget(p.Soak, now, now.Add(need-time.Second), now.Add(9*time.Hour)); err == nil || !strings.Contains(err.Error(), "work deadline") {
		t.Fatalf("a short work deadline passed: %v", err)
	}
	if err := directSoakBudget(p.Soak, now, now.Add(need), now.Add(need+CleanupReserve-time.Second)); err == nil || !strings.Contains(err.Error(), "--timeout") {
		t.Fatalf("a short --timeout passed: %v", err)
	}
	if err := directSoakBudget(p.Soak, now, now.Add(70*time.Minute), now.Add(70*time.Minute)); err == nil {
		t.Fatal("an 8 h soak passed with today's 70 min timeout")
	}
	p.Soak.DurationS = 32401
	if err := directSoakBudget(p.Soak, now, now.Add(10*time.Hour), now.Add(10*time.Hour)); err == nil {
		t.Fatal("a soak past the direct horizon passed the budget")
	}
}

func TestSoakScaleCountsThirtyMinuteSpans(t *testing.T) {
	for d, want := range map[int]int{0: 1, 1: 1, 1800: 1, 1801: 2, 7200: 4, 28800: 16, 32400: 18} {
		if got := soakScale(d); got != want {
			t.Errorf("soakScale(%d) = %d, want %d", d, got, want)
		}
	}
}
