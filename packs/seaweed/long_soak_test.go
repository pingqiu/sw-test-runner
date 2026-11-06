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

// directSoakBudget refuses, before any deploy, an 8 h soak whose work deadline or --timeout can't hold it,
// including its whole scaled assessor step (codex01 P2 on fe027da4).
func TestDirectSoakBudgetSizesTheTimeoutAndTheWorkDeadline(t *testing.T) {
	p, _ := soakTestPlan()
	p.Soak.DurationS = 28800
	now := time.Unix(1_800_000_000, 0)
	// The corrected 8 h sizing: deploy 300 + witness 10 + load 28800 + quiescence 60 + exit grace 30
	// + trailing (collect 120 + assessor 15 x 16^2 = 3840) = 33160 s of work; the timeout adds cleanup 120.
	need := 33160 * time.Second
	if got := DeployReserve + WitnessWait + 8*time.Hour + QuiescenceReserve + LoadExitGrace + trailingReserve(28800, true); got != need {
		t.Fatalf("8 h need = %v, want %v", got, need)
	}
	if err := directSoakBudget(p.Soak, now, now.Add(need), now.Add(need+CleanupReserve)); err != nil {
		t.Fatalf("the exact budget was refused: %v", err)
	}
	if err := directSoakBudget(p.Soak, now, now.Add(need-time.Second), now.Add(9*time.Hour)); err == nil || !strings.Contains(err.Error(), "work deadline") {
		t.Fatalf("a short work deadline passed: %v", err)
	}
	if err := directSoakBudget(p.Soak, now, now.Add(need), now.Add(need+CleanupReserve-time.Second)); err == nil || !strings.Contains(err.Error(), "--timeout") {
		t.Fatalf("a short --timeout passed: %v", err)
	}
	// Composed control: the fixed-CollectReserve budget (fe027da4's) no longer admits 8 h, because the
	// assessor alone may use 64 min.
	legacy := DeployReserve + WitnessWait + 8*time.Hour + QuiescenceReserve + LoadExitGrace + CollectReserve
	if err := directSoakBudget(p.Soak, now, now.Add(legacy), now.Add(legacy+CleanupReserve)); err == nil {
		t.Fatal("8 h admitted without reserving its scaled assessor")
	}
	if err := directSoakBudget(p.Soak, now, now.Add(70*time.Minute), now.Add(70*time.Minute)); err == nil {
		t.Fatal("an 8 h soak passed with today's 70 min timeout")
	}
	p.Soak.DurationS = 32401
	if err := directSoakBudget(p.Soak, now, now.Add(10*time.Hour), now.Add(10*time.Hour)); err == nil {
		t.Fatal("a soak past the direct horizon passed the budget")
	}
}

// One trailing reserve: CollectReserve on the approved path and for any 30-minute soak (unchanged);
// plus the whole scaled assessor step for a longer direct soak.
func TestTrailingReserveCoversTheScaledAssessorOnlyForLongDirectSoaks(t *testing.T) {
	for _, c := range []struct {
		d      int
		direct bool
		want   time.Duration
	}{{1800, false, CollectReserve}, {7200, false, CollectReserve}, {1800, true, CollectReserve},
		{1801, true, CollectReserve + 240*time.Second}, {7200, true, CollectReserve + 480*time.Second},
		{28800, true, CollectReserve + 3840*time.Second}} {
		if got := trailingReserve(c.d, c.direct); got != c.want {
			t.Errorf("trailingReserve(%d, %v) = %v, want %v", c.d, c.direct, got, c.want)
		}
	}
}

// The assessor step covers its measured runtime with a 2x margin at both measured durations (#1002: offline on
// tp01 with the real evidence and a clock bracket, 2 h 91 s, 8 h 1552 s; the miss join is ~quadratic), and keeps
// the 30-minute and 2 h steps unchanged.
func TestAssessTimeoutCoversTheMeasuredMissJoinWithMargin(t *testing.T) {
	for _, c := range []struct {
		d               int
		measured, exact time.Duration
	}{{1800, 0, 120 * time.Second}, {7200, 91 * time.Second, 480 * time.Second}, {28800, 1552 * time.Second, 3840 * time.Second}} {
		if got := assessTimeout(c.d); got != c.exact || got < 2*c.measured {
			t.Errorf("assessTimeout(%d) = %v, want %v and at least 2 x %v", c.d, got, c.exact, c.measured)
		}
	}
}

// runSoak admits the load with the same trailing reserve: a 2 h direct soak whose work deadline only
// holds the fixed CollectReserve is refused before any effect; a 30-minute direct soak keeps today's
// admission exactly (the 45 min fixture deadline still passes, with both faults).
func TestRunSoakReservesTheScaledAssessorAtLoadAdmission(t *testing.T) {
	f := newFakeSoak(t)
	f.exitAt = f.start + 7200 + 60.01
	legacy := WitnessWait + 2*time.Hour + QuiescenceReserve + LoadExitGrace + CollectReserve
	f.deadline = f.wall.Add(legacy + time.Minute) // enough for fe027da4's admission, not for the 480 s assessor
	p, in := soakTestPlan()
	p.Soak.DurationS = 7200
	if _, _, err := runSoak(context.Background(), directHorizon{f}, p, in, map[string]tr.JobHandle{}); err == nil || !strings.Contains(err.Error(), "not enough work time left to admit the load") || f.started != 0 {
		t.Fatalf("a 2 h direct soak was admitted without its assessor reserve: err=%v started=%d", err, f.started)
	}
	f = newFakeSoak(t) // 30 min, 45 min deadline: as on the approved path
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	defer cancel()
	p, in = soakTestPlan()
	records, _, err := runSoak(ctx, directHorizon{f}, p, in, map[string]tr.JobHandle{})
	if err != nil || strings.Join(f.restarts, ",") != "E1,E2" || len(records) != 2 {
		t.Fatalf("a 30-min direct soak changed: err=%v restarts=%v", err, f.restarts)
	}
}

func TestSoakScaleCountsThirtyMinuteSpans(t *testing.T) {
	for d, want := range map[int]int{0: 1, 1: 1, 1800: 1, 1801: 2, 7200: 4, 28800: 16, 32400: 18} {
		if got := soakScale(d); got != want {
			t.Errorf("soakScale(%d) = %d, want %d", d, got, want)
		}
	}
}

// codex01 R2 on ab54b6d4: runSoak validates the plan before any arithmetic on it. A nil plan and
// out-of-range plans return the range error with no effect, on both executors, instead of panicking.
func TestRunSoakRefusesNilAndOutOfRangePlansBeforeAnyArithmeticOrEffect(t *testing.T) {
	for _, direct := range []bool{false, true} {
		for name, soak := range map[string]*SoakPlan{"nil": nil, "zero": {DurationS: 0}, "negative": {DurationS: -1},
			"past the direct horizon": {DurationS: 32401}} {
			f := newFakeSoak(t)
			var ops soakOps = f
			if direct {
				ops = directHorizon{f}
			}
			p, in := soakTestPlan()
			p.Soak = soak
			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("direct=%v %s: runSoak panicked: %v", direct, name, r)
					}
				}()
				_, _, err = runSoak(context.Background(), ops, p, in, map[string]tr.JobHandle{})
			}()
			if err == nil || !strings.Contains(err.Error(), "duration out of range") || f.started != 0 || len(f.restarts) != 0 {
				t.Errorf("direct=%v %s: err=%v started=%d restarts=%v", direct, name, err, f.started, f.restarts)
			}
		}
	}
}
