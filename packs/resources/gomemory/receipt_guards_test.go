package gomemory

// Isolated witnesses adapted from codex01 runner#7 review; retained fixtures
// supply the unrelated passing identity, heap, listener and receipt checks.

import "testing"

func liveMaster(t *testing.T) replayGroup {
	t.Helper()
	for _, g := range fixture(t, "132324") {
		if g.Live && g.ID.Role == "master" {
			return g
		}
	}
	t.Fatal("no live master")
	return replayGroup{}
}

func TestIndependentReceiptGuards(t *testing.T) {
	tests := []struct {
		name   string
		group  func(*testing.T) replayGroup
		mutate func(*replayGroup)
		want   string
	}{
		{"rss-10percent-branch", liveFiler, func(g *replayGroup) {
			g.Evidence.Warm.RSS = f(1024 * MiB)
			g.Evidence.End.RSS = f(1124 * MiB)
		}, "PASS"},
		{"fd-closure-bound", func(t *testing.T) replayGroup { return fixture(t, "132324")[0] }, func(g *replayGroup) {
			g.FDMax = f(*g.FD0 + 17)
		}, "FAIL"},
		{"closure-epoch-floor", func(t *testing.T) replayGroup { return fixture(t, "132324")[0] }, func(g *replayGroup) {
			g.Evidence.Closed.Epoch = f(0)
		}, "FAIL"},
		{"closure-absence", func(t *testing.T) replayGroup { return fixture(t, "132324")[0] }, func(g *replayGroup) {
			g.Evidence.Closed.Absent = false
		}, "FAIL"},
		{"warm-profile-upper", liveMaster, func(g *replayGroup) {
			g.Evidence.Warm.ProfileEnd = f(*g.Evidence.Warm.Anchor + 635.001)
		}, "FAIL"},
		{"warm-lower-effective", liveMaster, func(g *replayGroup) {
			g.Evidence.Warm.RequestStart = f(*g.Evidence.Warm.Anchor + 599)
			g.Evidence.Warm.Age = f(*g.Evidence.Warm.RequestStart - *g.Evidence.Warm.Anchor)
			g.Evidence.Warm.ProfileEnd = f(*g.Evidence.Warm.RequestStart + 0.001)
		}, "FAIL"},
		{"filer-rss-order", liveFiler, func(g *replayGroup) {
			g.Evidence.Warm.RSSEnd = f(*g.Evidence.Warm.RSSStart - 0.001)
		}, "FAIL"},
		{"filer-rss-wall-order", liveFiler, func(g *replayGroup) {
			g.Evidence.Warm.RSSEpochEnd = f(*g.Evidence.Warm.RSSEpochStart - 0.001)
		}, "FAIL"},
		{"profile-order", liveMaster, func(g *replayGroup) {
			g.Evidence.End.RequestStart = g.Evidence.Warm.ProfileEnd
			g.Evidence.End.Age = f(*g.Evidence.End.RequestStart - *g.Evidence.End.Anchor)
			g.Evidence.End.ProfileEnd = f(*g.Evidence.End.RequestStart + 0.001)
		}, "FAIL"},
		{"profile-monotonic-order", liveMaster, func(g *replayGroup) {
			g.Evidence.End.ProfileEnd = f(*g.Evidence.End.RequestStart - 0.001)
		}, "FAIL"},
		{"profile-age-consistency", liveMaster, func(g *replayGroup) {
			g.Evidence.End.Age = f(*g.Evidence.End.Age + 1)
		}, "FAIL"},
		{"phase-archive", func(t *testing.T) replayGroup { return fixture(t, "132324")[0] }, func(g *replayGroup) {
			g.Evidence.Warm.Kind = "quiescent_gc"
		}, "FAIL"},
		{"warm-gc-zero", liveFiler, func(g *replayGroup) {
			i := 1
			g.Evidence.Warm.GCRequests = &i
		}, "FAIL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := tc.group(t)
			baseline := "PASS"
			if g.Evidence.Closed != nil {
				baseline = "CLOSED"
			}
			if got := replay(g); got.Status != baseline {
				t.Fatalf("unrelated baseline invalid: %+v", got)
			}
			tc.mutate(&g)
			if got := replay(g); got.Status != tc.want {
				t.Fatalf("got %+v, want %s", got, tc.want)
			}
		})
	}
}

func TestDirectEvaluatePhaseGuard(t *testing.T) {
	g := liveMaster(t)
	g.Evidence.Warm.Kind = "quiescent_gc"
	if got := Evaluate(g.Baseline, g.FD0, g.FDMax, *g.Evidence.Warm, *g.Evidence.End); got.Status != "FAIL" {
		t.Fatalf("got %+v, want FAIL", got)
	}
}

// At a 1000 MiB baseline, 10% is exactly 100 MiB (>64 MiB), so
// both acceptance and the first rejected byte distinguish this policy branch.
func TestLargeRSSExactThreshold(t *testing.T) {
	for _, role := range []string{"master", "filer"} {
		t.Run(role, func(t *testing.T) {
			g := liveMaster(t)
			if role == "filer" {
				g = liveFiler(t)
			}
			g.Baseline = f(1000 * MiB)
			g.Evidence.Warm.RSS = f(1000 * MiB)
			g.Evidence.End.RSS = f(1100 * MiB)
			if got := replay(g); got.Status != "PASS" || !got.RSSOK {
				t.Fatal(got)
			}
			g.Evidence.End.RSS = f(1100*MiB + 1)
			if got := replay(g); got.Status != "FAIL" || got.RSSOK || !got.HeapOK || !got.FDsOK {
				t.Fatal(got)
			}
		})
	}
}

func TestWarmTimingExactBoundaries(t *testing.T) {
	for _, test := range []struct {
		name       string
		start, end float64
		want       string
	}{
		{"lower_exact", 600, 600.001, "PASS"},
		{"lower_missed", 599.999, 600, "FAIL"},
		{"upper_exact", 634.999, 635, "PASS"},
		{"upper_missed", 634.999, 635.001, "FAIL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Master intentionally avoids the filer's separate RSS window guard.
			g := liveMaster(t)
			g.Evidence.Warm.RequestStart = f(*g.Evidence.Warm.Anchor + test.start)
			g.Evidence.Warm.ProfileEnd = f(*g.Evidence.Warm.Anchor + test.end)
			g.Evidence.Warm.Age = f(*g.Evidence.Warm.RequestStart - *g.Evidence.Warm.Anchor)
			if got := replay(g); got.Status != test.want {
				t.Fatal(got)
			}
		})
	}
}

func TestClosedFDExactThreshold(t *testing.T) {
	g := fixture(t, "132324")[0]
	g.FDMax = f(*g.FD0 + 16)
	if got := replay(g); got.Status != "CLOSED" {
		t.Fatal(got)
	}
	g.FDMax = f(*g.FD0 + 17)
	if got := replay(g); got.Status != "FAIL" {
		t.Fatal(got)
	}
}

func TestIndependentWallClockAndEndPhase(t *testing.T) {
	g := liveMaster(t)
	if got := replay(g); got.Status != "PASS" {
		t.Fatal(got)
	}
	g.Evidence.End.ProfileEpoch = f(*g.Evidence.End.RequestEpoch - .001)
	if got := replay(g); got.Status != "FAIL" {
		t.Fatal(got)
	}
	g = liveMaster(t)
	g.Evidence.End.Kind = "warm"
	if got := Evaluate(g.Baseline, g.FD0, g.FDMax, *g.Evidence.Warm, *g.Evidence.End); got.Status != "FAIL" {
		t.Fatal(got)
	}
}
