package seaweed

import (
	"math"
	"strings"
	"testing"
)

// The default LMCache-like pattern: 32 clients, write once, then read 600 s.
func lmcacheLike() *LoadPattern {
	return &LoadPattern{Clients: 32, Transport: "dc", Keys: 4096, Distribution: "zipf", ZipfS: 1.1, ObjectBytes: 64 << 10,
		RatePerClient: 10, Phases: []LoadPhase{{Kind: "write_once"}, {Kind: "read", DurationS: 600}}}
}

func TestLoadPatternBounds(t *testing.T) {
	if err := lmcacheLike().check(1800); err != nil {
		t.Fatalf("the default pattern was refused: %v", err)
	}
	for name, mutate := range map[string]func(*LoadPattern){
		"no clients":            func(p *LoadPattern) { p.Clients = 0 },
		"65 clients":            func(p *LoadPattern) { p.Clients = 65 },
		"transport":             func(p *LoadPattern) { p.Transport = "rdma" },
		"no keys":               func(p *LoadPattern) { p.Keys = 0 },
		"object past the limit": func(p *LoadPattern) { p.ObjectBytes = 32 << 20 },
		"zero rate":             func(p *LoadPattern) { p.RatePerClient = 0 },
		"NaN rate":              func(p *LoadPattern) { p.RatePerClient = math.NaN() },
		"rate over 100":         func(p *LoadPattern) { p.RatePerClient = 101 },
		"uniform with s":        func(p *LoadPattern) { p.Distribution = "uniform" },
		"zipf s 0":              func(p *LoadPattern) { p.ZipfS = 0 },
		"zipf s 3.5":            func(p *LoadPattern) { p.ZipfS = 3.5 },
		"other distribution":    func(p *LoadPattern) { p.Distribution, p.ZipfS = "hot", 0 },
		"reads first":           func(p *LoadPattern) { p.Phases[0] = LoadPhase{Kind: "read", DurationS: 60} },
		"write_once timed":      func(p *LoadPattern) { p.Phases[0].DurationS = 60 },
		"one phase":             func(p *LoadPattern) { p.Phases = p.Phases[:1] },
		"five phases":           func(p *LoadPattern) { p.Phases = append(p.Phases, p.Phases[1], p.Phases[1], p.Phases[1]) },
		"read without time":     func(p *LoadPattern) { p.Phases[1].DurationS = 0 },
		"read with a ratio":     func(p *LoadPattern) { p.Phases[1].ReadRatio = 0.5 },
		"mixed ratio 1":         func(p *LoadPattern) { p.Phases[1] = LoadPhase{Kind: "mixed", DurationS: 60, ReadRatio: 1} },
		"phases overrun":        func(p *LoadPattern) { p.Phases[1].DurationS = 1790 }, // 4096/(32*10) = 12.8 s of writes first
	} {
		p := lmcacheLike()
		mutate(p)
		if err := p.check(1800); err == nil || !strings.Contains(err.Error(), "kv_load pattern") {
			t.Errorf("%s: err %v, want a kv_load pattern refusal", name, err)
		}
	}
	mixed := lmcacheLike()
	mixed.Distribution, mixed.ZipfS = "uniform", 0
	mixed.Phases = append(mixed.Phases, LoadPhase{Kind: "mixed", DurationS: 300, ReadRatio: 0.9})
	if err := mixed.check(1800); err != nil {
		t.Fatalf("uniform with a mixed phase was refused: %v", err)
	}
}

// The soak's own schedule check runs the pattern's.
func TestCheckScheduleChecksThePattern(t *testing.T) {
	p, _ := soakTestPlan()
	if err := p.Soak.checkSchedule(); err != nil {
		t.Fatal(err)
	}
	p.Soak.Pattern = lmcacheLike()
	p.Soak.Pattern.Clients = 0
	if err := p.Soak.checkSchedule(); err == nil || !strings.Contains(err.Error(), "kv_load pattern") {
		t.Fatalf("checkSchedule passed a bad pattern: %v", err)
	}
}
