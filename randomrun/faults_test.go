package randomrun

import (
	"reflect"
	"regexp"
	"testing"
)

// Process names here are a test product's: p0..p9. Their sorted order is
// the order of the processes the pinned schedule was first derived for.
var testProcess = regexp.MustCompile(`^p[0-9]$`).MatchString

var faultProcs = []string{"p2", "p0", "p1"}

func TestFaultScheduleSameSeedSameScheduleAndPinned(t *testing.T) {
	a, err := NewFaultSchedule(8, 60, faultProcs, 0.08, 20, testProcess)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewFaultSchedule(8, 60, []string{"p0", "p1", "p2"}, 0.08, 20, testProcess)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("the same seed (processes in another order) gave two schedules")
	}
	// Pinned: a change to the derivation changes every recorded run's schedule,
	// so it must come with a new fault-run generator version.
	want := []FaultEvent{
		{24, FaultKill, "p1"}, {29, FaultRestart, "p1"},
		{34, FaultKill, "p1"}, {39, FaultRestart, "p1"},
		{50, FaultKill, "p0"}, {57, FaultRestart, "p0"},
	}
	if !reflect.DeepEqual(a.Events, want) {
		t.Fatalf("schedule for seed 8 changed:\n%#v", a.Events)
	}
	c, _ := NewFaultSchedule(9, 60, faultProcs, 0.08, 20, testProcess)
	if reflect.DeepEqual(a.Events, c.Events) {
		t.Fatal("another seed gave the same schedule")
	}
	if off, err := NewFaultSchedule(7, 60, nil, 0, 20, testProcess); err != nil || len(off.Events) != 0 {
		t.Fatalf("rate 0: %v %v", off, err)
	}
}

func TestFaultScheduleEveryKillRestartedWithinTheBoundOneDownAtATime(t *testing.T) {
	kills := 0
	for seed := uint64(1); seed <= 200; seed++ {
		for _, rate := range []float64{0.01, 0.1, 0.5, 1} {
			for _, within := range []int{1, 5, 20} {
				ops := 20 + int(seed%7)*30
				s, err := NewFaultSchedule(seed, ops, faultProcs, rate, within, testProcess)
				if err != nil {
					t.Fatal(err)
				}
				if err := CheckFaultSchedule(s.Events, ops, within, testProcess); err != nil {
					t.Fatalf("seed %d rate %v within %d: %v\n%v", seed, rate, within, err, s.Events)
				}
				// The same rules, stated directly here, independent of the checker.
				down := map[string]int{}
				for _, e := range s.Events {
					if e.Action == FaultKill {
						if len(down) != 0 {
							t.Fatalf("seed %d: %s killed while %v down", seed, e.Process, down)
						}
						down[e.Process] = e.AfterOp
						kills++
					} else {
						k, ok := down[e.Process]
						if !ok || e.AfterOp <= k || e.AfterOp-k > within || e.AfterOp > ops {
							t.Fatalf("seed %d: restart %+v (killed after %d, ok %v)", seed, e, k, ok)
						}
						delete(down, e.Process)
					}
				}
				if len(down) != 0 {
					t.Fatalf("seed %d: never restarted %v", seed, down)
				}
			}
		}
	}
	if kills < 1000 {
		t.Fatalf("only %d kills over the sweep", kills)
	}
	for name, f := range map[string]func() error{
		"rate above 1":    func() error { _, err := NewFaultSchedule(1, 10, faultProcs, 1.5, 20, testProcess); return err },
		"bound 0":         func() error { _, err := NewFaultSchedule(1, 10, faultProcs, 0.1, 0, testProcess); return err },
		"no processes":    func() error { _, err := NewFaultSchedule(1, 10, nil, 0.1, 20, testProcess); return err },
		"unknown process": func() error { _, err := NewFaultSchedule(1, 10, []string{"q"}, 0.1, 20, testProcess); return err },
		"repeated process": func() error {
			_, err := NewFaultSchedule(1, 10, []string{"p0", "p0"}, 0.1, 20, testProcess)
			return err
		},
	} {
		if f() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Red controls for CheckFaultSchedule: each seeded defect must be caught.
func TestCheckFaultScheduleCatchesEachDefect(t *testing.T) {
	k := func(n int, p string) FaultEvent { return FaultEvent{n, FaultKill, p} }
	r := func(n int, p string) FaultEvent { return FaultEvent{n, FaultRestart, p} }
	good := []FaultEvent{k(2, "p0"), r(5, "p0"), k(5, "p1"), r(25, "p1")}
	if err := CheckFaultSchedule(good, 30, 20, testProcess); err != nil {
		t.Fatalf("green: %v", err)
	}
	for name, ev := range map[string][]FaultEvent{
		"kill never restarted":     {k(2, "p0")},
		"two processes down":       {k(2, "p0"), k(3, "p1"), r(4, "p1"), r(5, "p0")},
		"restart beyond the bound": {k(2, "p0"), r(23, "p0")},
		"restart with no op down":  {k(2, "p0"), r(2, "p0")},
		"restart of another":       {k(2, "p0"), r(4, "p1")},
		"out of order":             {k(9, "p0"), r(12, "p0"), k(3, "p1"), r(4, "p1")},
		"after the last op":        {k(29, "p0"), r(31, "p0")},
		"before the first op":      {k(0, "p0"), r(1, "p0")},
		"unknown process":          {k(2, "px"), r(3, "px")},
		"unknown action":           {{2, "pause", "p0"}},
	} {
		if err := CheckFaultSchedule(ev, 30, 20, testProcess); err == nil {
			t.Errorf("red control %q not caught", name)
		}
	}
}
