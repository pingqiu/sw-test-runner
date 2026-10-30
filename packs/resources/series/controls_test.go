package series

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"testing"
)

func f(v float64) *float64 { return &v }
func simple() Input {
	in := Input{Policy: Replacement9b5}
	ages := []float64{600.1, 630.1, 660.1, 689.9}
	for a := 720.; a <= 1050; a += 30 {
		ages = append(ages, a)
	}
	for _, a := range ages {
		p := Process{Identity: Identity{Role: "client-0", PID: 123, Start: 10000}, RSS: f(100), FDs: f(10), Pin: f(0), Lock: f(0), Capture: &Capture{Host: "host", Boot: "boot", Namespace: "time:[1]", Domain: "CLOCK_BOOTTIME", HZ: 100, Start: f(100 + a), End: f(100 + a + .001)}}
		s := Sample{Body: f(100 + a), BodyBracket: []float64{100 + a, 100 + a + .01}, Clients: []Process{p}, Server: Server{Data: map[string]*float64{}, VolumeDir: f(0), Auxiliary: f(0), Budgets: []Budget{{Bytes: f(1)}}, Gauges: map[string]*float64{pinned: f(0)}}}
		in.Samples = append(in.Samples, s)
	}
	p := in.Samples[len(in.Samples)-1].Clients[0]
	p.Capture = &Capture{Host: "host", Boot: "boot", Namespace: "time:[1]", Domain: "CLOCK_BOOTTIME", HZ: 100, Start: f(1200), End: f(1200.001)}
	in.Final.Clients = []Process{p}
	zero := func() map[string]*float64 {
		m := map[string]*float64{}
		for _, k := range append(append([]string{}, retained...), pools...) {
			m[k] = f(0)
		}
		return m
	}
	in.Final.RDMA = zero()
	in.Final.Baseline.RDMA = zero()
	victim := 0
	in.Faults = []Fault{{Kind: "client_kill", Body: f(100), Started: f(100), Finished: f(101), Victim: &victim, Replacement: &Identity{PID: 123, Start: 10000}}}
	return in
}
func TestReplacementWitnessAndExactTerminal(t *testing.T) {
	in := simple()
	got := Assess(in)
	if got.Status != "PASS" {
		t.Fatal(got)
	}
	in.Final.Clients[0].Pin = f(1)
	if got := Assess(in); got.Checks["fixed_pinning_returns_to_baseline"].Status != "FAIL" {
		t.Fatal(got)
	}
}
func TestFDLimitStartsAtThirdWarmSample(t *testing.T) {
	for _, i := range []int{2, 3, 4} {
		in := simple()
		in.Samples[i].Clients[0].FDs = f(27)
		if got := Assess(in); got.Checks["rss_fd_growth"].Status != "FAIL" {
			t.Fatalf("sample %d accepted: %+v", i, got)
		}
	}
}
func TestInvalidReplacementWitnessNeverPasses(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Input)
	}{
		{"missing capture", func(in *Input) { in.Samples[0].Clients[0].Capture = nil }},
		{"foreign boot", func(in *Input) { in.Samples[5].Clients[0].Capture.Boot = "other" }},
		{"foreign host", func(in *Input) { in.Samples[5].Clients[0].Capture.Host = "other" }},
		{"foreign namespace", func(in *Input) { in.Samples[5].Clients[0].Capture.Namespace = "other" }},
		{"changed tick rate", func(in *Input) { in.Samples[5].Clients[0].Capture.HZ++ }},
		{"wrong clock", func(in *Input) { in.Samples[5].Clients[0].Capture.Domain = "CLOCK_MONOTONIC" }},
		{"nonfinite capture", func(in *Input) { in.Samples[5].Clients[0].Capture.Start = f(math.NaN()) }},
		{"duplicate body bracket", func(in *Input) {
			in.Samples = append(in.Samples[:8], append([]Sample{in.Samples[7]}, in.Samples[8:]...)...)
		}},
		{"terminal missing", func(in *Input) { in.Final.Clients = nil }},
		{"terminal boot changed", func(in *Input) { in.Final.Clients[0].Capture.Boot = "other" }},
		{"terminal capture reversed", func(in *Input) { in.Final.Clients[0].Capture.End = f(1199) }},
		{"replacement identity missing", func(in *Input) { in.Faults[0].Replacement = nil }},
		{"replacement identity malformed", func(in *Input) { in.Faults[0].Replacement.Start = 0 }},
		{"victim missing", func(in *Input) { in.Faults[0].Victim = nil }},
		{"five post samples", func(in *Input) { in.Samples = in.Samples[:9] }},
		{"post span too short", func(in *Input) { in.Samples = in.Samples[:10] }},
		{"controller span cannot substitute", func(in *Input) {
			for i := 4; i < len(in.Samples); i++ {
				c := in.Samples[i].Clients[0].Capture
				c.Start = f(820 + float64(i-4)*20)
				c.End = f(*c.Start + .001)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := simple()
			tc.mutate(&in)
			if got := Assess(in); got.Checks["rss_fd_growth"].Status != "NOT_OBSERVED" {
				t.Fatalf("invalid replacement not refused: %+v", got)
			}
		})
	}
}

func TestStartupRSSSlopeAndLevelRemainIndependent(t *testing.T) {
	for _, kind := range []string{"slope", "level"} {
		in := simple()
		in.Policy = Startup54d
		in.Faults = nil
		in.Samples = in.Samples[:6]
		for i := range in.Samples {
			if kind == "slope" {
				in.Samples[i].Body = f(300 + float64(i)*5)
				in.Samples[i].Clients[0].RSS = f(float64(i) * 1048576)
			} else {
				in.Samples[i].Body = f(300 + float64(i)*1800)
				rss := 100.
				if i >= 3 {
					rss = 300
				}
				in.Samples[i].Clients[0].RSS = f(rss * 1048576)
			}
		}
		if got := Assess(in); got.Checks["rss_fd_growth"].Status != "FAIL" {
			t.Fatalf("%s accepted %+v", kind, got)
		}
	}
}

func TestNativeExactThresholds(t *testing.T) {
	in := simple()
	in.Policy = Startup54d
	in.Faults = nil
	in.Samples = in.Samples[:6]
	for i := range in.Samples {
		in.Samples[i].Body = f(300 + float64(i)*60)
		in.Samples[i].Clients[0].RSS = f(float64(i) * 2 * 1048576)
	}
	if r := Assess(in); r.Checks["rss_fd_growth"].Status != "PASS" {
		t.Fatal(r)
	}
	in.Samples[5].Clients[0].RSS = f(*in.Samples[5].Clients[0].RSS + 1)
	if r := Assess(in); r.Checks["rss_fd_growth"].Status != "FAIL" {
		t.Fatal(r)
	}
	in = simple()
	in.Samples[2].Clients[0].FDs = f(26)
	if r := Assess(in); r.Checks["rss_fd_growth"].Status != "PASS" {
		t.Fatal(r)
	}
	in.Final.Clients[0].Lock = f(1)
	if r := Assess(in); r.Checks["fixed_pinning_returns_to_baseline"].Status != "FAIL" {
		t.Fatal(r)
	}
}

func TestEveryFrozenDiskBound(t *testing.T) {
	for _, kind := range []string{"cache count", "all count", "all bytes", "directory bytes", "auxiliary bytes"} {
		in := simple()
		s := &in.Samples[0].Server
		switch kind {
		case "cache count":
			for i := 0; i < 9; i++ {
				s.Data[fmt.Sprint("lmcache_", i, ".dat")] = f(1)
			}
		case "all count":
			for i := 0; i < 25; i++ {
				s.Data[fmt.Sprint(i, ".dat")] = f(1)
			}
		case "all bytes":
			s.Data["1.dat"] = f(768*1048576 + 1)
		case "directory bytes":
			s.VolumeDir = f(960*1048576 + 1)
		case "auxiliary bytes":
			s.Auxiliary = f(128*1048576 + 1)
		}
		if r := Assess(in); r.Checks["server_storage_bounds"].Status != "FAIL" {
			t.Fatalf("%s accepted %+v", kind, r)
		}
	}
}
func TestChronologyBeforeFaultOrStartupExclusion(t *testing.T) {
	for _, body := range []float64{299, 500} {
		in := simple()
		r := &in.Samples[8]
		r.Body = f(body)
		r.BodyBracket = []float64{body, body + .01}
		r.Clients[0].FDs = f(27)
		if body == 500 {
			in.Faults = append(in.Faults, Fault{Kind: "freeze_restart", Body: f(500), Started: f(1), Finished: f(2)})
		}
		if got := Assess(in); got.Checks["rss_fd_growth"].Status != "NOT_OBSERVED" {
			t.Fatal(got)
		}
	}
}
func TestFullCaptureWarmAndFaultBrackets(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Input)
	}{
		{"early", func(in *Input) { in.Samples[0].Clients[0].Capture.Start = f(699.999) }},
		{"late", func(in *Input) { in.Samples[2].Clients[0].Capture.End = f(790.001) }},
		{"tick uncertainty", func(in *Input) { in.Samples[0].Clients[0].Capture.Start = f(700.005) }},
		{"fault crosses capture", func(in *Input) {
			in.Samples[0].BodyBracket[1] += 2
			in.Faults = append(in.Faults, Fault{Kind: "freeze_restart", Body: f(*in.Samples[0].Body + 1), Started: f(1), Finished: f(2)})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := simple()
			in.Samples = append(in.Samples[:3], in.Samples[4:]...)
			tc.mutate(&in)
			if got := Assess(in); got.Checks["rss_fd_growth"].Status != "NOT_OBSERVED" {
				t.Fatal(got)
			}
		})
	}
}
func TestOriginalAndGonePathsRemainSeparate(t *testing.T) {
	in := simple()
	in.Faults = nil
	for i := range in.Samples {
		in.Samples[i].Clients[0].Capture = nil
	}
	in.Final.Clients[0].Capture = nil
	if got := Assess(in); got.Status != "PASS" {
		t.Fatal(got)
	}
	in = simple()
	in.Final.Clients[0].Status = "GONE"
	in.Final.Clients[0].Capture = nil
	if got := Assess(in); got.Checks["rss_fd_growth"].Status != "PASS" {
		t.Fatal(got)
	}
}
func TestMissingRequiredGenerationCannotDisappear(t *testing.T) {
	in := simple()
	in.Faults = append(in.Faults, Fault{Kind: "vkill", Body: f(1), Started: f(1), Finished: f(2), Result: &Restart{New: &Generation{Identity: Identity{PID: 999, Start: 10000}}}})
	r := Assess(in)
	if r.Checks["rss_fd_growth"].Status != "NOT_OBSERVED" || r.Checks["fixed_pinning_returns_to_baseline"].Status != "NOT_OBSERVED" {
		t.Fatal(r)
	}
}
func TestHardBoundsAndMissingMetrics(t *testing.T) {
	cases := []struct {
		name, check, status string
		mutate              func(*Input)
	}{
		{"RSS grows", "rss_fd_growth", "FAIL", func(in *Input) {
			for i := len(in.Samples) - 2; i < len(in.Samples); i++ {
				in.Samples[i].Clients[0].RSS = f(1024 * 1024 * 1024)
			}
		}},
		{"pin above budget", "reported_pin_budget", "FAIL", func(in *Input) { in.Samples[0].Server.Gauges[pinned] = f(2) }},
		{"pin missing", "reported_pin_budget", "NOT_OBSERVED", func(in *Input) { delete(in.Samples[0].Server.Gauges, pinned) }},
		{"budget missing", "reported_pin_budget", "NOT_OBSERVED", func(in *Input) { in.Samples[0].Server.Budgets = nil }},
		{"retained source", "quiescence_baseline_return", "FAIL", func(in *Input) { in.Final.RDMA[retained[1]] = f(1) }},
		{"pool changed", "quiescence_baseline_return", "FAIL", func(in *Input) { in.Final.RDMA[pools[1]] = f(1) }},
		{"gauge missing", "quiescence_baseline_return", "FAIL", func(in *Input) { delete(in.Final.RDMA, pools[2]) }},
		{"disk exceeds bound", "server_storage_bounds", "FAIL", func(in *Input) { in.Samples[0].Server.Data["lmcache_1.dat"] = f(256*1024*1024 + 1) }},
		{"disk field missing", "server_storage_bounds", "FAIL", func(in *Input) { in.Samples[0].Server.Auxiliary = nil }},
		{"FD missing", "rss_fd_growth", "NOT_OBSERVED", func(in *Input) { in.Samples[5].Clients[0].FDs = nil }},
		{"terminal lock missing", "fixed_pinning_returns_to_baseline", "NOT_OBSERVED", func(in *Input) { in.Final.Clients[0].Lock = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := simple()
			tc.mutate(&in)
			if got := Assess(in); got.Checks[tc.check].Status != tc.status {
				t.Fatal(got)
			}
		})
	}
}
func TestPolicyIsExplicitAndInputIsUnchanged(t *testing.T) {
	in := simple()
	before, _ := json.Marshal(in)
	if got := Assess(in); got.Status != "PASS" {
		t.Fatal(got)
	}
	after, _ := json.Marshal(in)
	if string(before) != string(after) {
		t.Fatal("assessor modified retained receipts")
	}
	in.Policy = ""
	if got := Assess(in); got.Status != "FAIL" {
		t.Fatal(got)
	}
}

func TestRetainedInputsAndPoliciesAreFrozen(t *testing.T) {
	for name, want := range map[string]string{
		"132324.json.gz": "c95a408e0fa5def45aeff4f4ef0ae17ac94dc9ef384a28359e22249d086b3a81",
		"095251.json.gz": "13a799e1834499b7d56754f6dda9b69b38cf3383e127506d65c1f1c77997e516",
	} {
		b, e := os.ReadFile("testdata/" + name)
		if e != nil {
			t.Fatal(e)
		}
		s := sha256.Sum256(b)
		if hex.EncodeToString(s[:]) != want {
			t.Fatal("frozen input changed", name)
		}
	}
}
