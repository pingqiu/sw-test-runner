package seaweed

import (
	"encoding/json"
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
		"no clients":             func(p *LoadPattern) { p.Clients = 0 },
		"65 clients":             func(p *LoadPattern) { p.Clients = 65 },
		"transport":              func(p *LoadPattern) { p.Transport = "rdma" },
		"http only":              func(p *LoadPattern) { p.Transport = "http" }, // the mandatory client kill needs a DC worker
		"no keys":                func(p *LoadPattern) { p.Keys = 0 },
		"object past the limit":  func(p *LoadPattern) { p.ObjectBytes = 32 << 20 },
		"zero rate":              func(p *LoadPattern) { p.RatePerClient = 0 },
		"NaN rate":               func(p *LoadPattern) { p.RatePerClient = math.NaN() },
		"rate over 100":          func(p *LoadPattern) { p.RatePerClient = 101 },
		"uniform with s":         func(p *LoadPattern) { p.Distribution = "uniform" },
		"zipf s 0":               func(p *LoadPattern) { p.ZipfS = 0 },
		"zipf s 3.5":             func(p *LoadPattern) { p.ZipfS = 3.5 },
		"other distribution":     func(p *LoadPattern) { p.Distribution, p.ZipfS = "hot", 0 },
		"reads first":            func(p *LoadPattern) { p.Phases[0] = LoadPhase{Kind: "read", DurationS: 60} },
		"write_once timed":       func(p *LoadPattern) { p.Phases[0].DurationS = 60 },
		"one phase":              func(p *LoadPattern) { p.Phases = p.Phases[:1] },
		"five phases":            func(p *LoadPattern) { p.Phases = append(p.Phases, p.Phases[1], p.Phases[1], p.Phases[1]) },
		"read without time":      func(p *LoadPattern) { p.Phases[1].DurationS = 0 },
		"read with a ratio":      func(p *LoadPattern) { p.Phases[1].ReadRatio = 0.5 },
		"mixed ratio 1":          func(p *LoadPattern) { p.Phases[1] = LoadPhase{Kind: "mixed", DurationS: 60, ReadRatio: 1} },
		"phases overrun":         func(p *LoadPattern) { p.Phases[1].DurationS = 1790 },
		"writes and their reads": func(p *LoadPattern) { p.Phases[1].DurationS = 1780 }, // 2*4096/(32*10) = 25.6 s before the reads
		"nofile under 1024":      func(p *LoadPattern) { p.VolumeNofile = 1023 },
		"nofile over product":    func(p *LoadPattern) { p.VolumeNofile = 1048577 },
		"negative nofile":        func(p *LoadPattern) { p.VolumeNofile = -1 },
		"pipes over 8":           func(p *LoadPattern) { p.PushPipes = 9 },
		"negative pipes":         func(p *LoadPattern) { p.PushPipes = -1 },
	} {
		p := lmcacheLike()
		mutate(p)
		if err := p.check(1800); err == nil || !strings.Contains(err.Error(), "kv_load pattern") {
			t.Errorf("%s: err %v, want a kv_load pattern refusal", name, err)
		}
	}
	knobs := lmcacheLike()
	knobs.VolumeNofile, knobs.PushPipes = 4096, 2 // run 2's capacity
	if err := knobs.check(1800); err != nil {
		t.Fatalf("nofile 4096 and 2 pipes were refused: %v", err)
	}
	mixed := lmcacheLike()
	mixed.Distribution, mixed.ZipfS = "uniform", 0
	mixed.Phases = append(mixed.Phases, LoadPhase{Kind: "mixed", DurationS: 300, ReadRatio: 0.9})
	if err := mixed.check(1800); err != nil {
		t.Fatalf("uniform with a mixed phase was refused: %v", err)
	}
}

// A present null capacity knob refuses on both decode paths (profile input
// and plan) instead of decoding to 0 and vanishing; a number decodes, and an
// unknown pattern field still refuses inside the custom decoder.
func TestLoadPatternRefusesANullKnob(t *testing.T) {
	input := func(pattern string) error {
		_, err := DecodeProfileInput([]byte(`{"soak":{"pattern":` + pattern + `}}`))
		return err
	}
	plan := func(pattern string) error {
		_, _, err := decodePlan(`{"soak":{"pattern":` + pattern + `}}`)
		return err
	}
	for _, knob := range []string{"volume_nofile", "push_pipes", "PUSH_PIPES"} {
		null := `{"clients":32,"` + knob + `":null}`
		for name, decode := range map[string]func(string) error{"profile input": input, "plan": plan} {
			// A case alias is already refused by uniqueJSON ("noncanonical").
			if err := decode(null); err == nil || !strings.Contains(err.Error(), "not null") && !strings.Contains(err.Error(), "noncanonical") {
				t.Errorf("%s, %s null: %v", name, knob, err)
			}
		}
	}
	if err := input(`{"clients":32,"push_pipes":2,"volume_nofile":4096}`); err != nil {
		t.Fatalf("numbers refused: %v", err)
	}
	var p SoakPlan
	if err := json.Unmarshal([]byte(`{"pattern":{"push_pipes":2,"volume_nofile":4096}}`), &p); err != nil || p.Pattern.PushPipes != 2 || p.Pattern.VolumeNofile != 4096 {
		t.Fatalf("knobs decoded as %+v: %v", p.Pattern, err)
	}
	if err := input(`{"clients":32,"pipes":2}`); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("an unknown pattern field passed: %v", err)
	}
}

// The preflight bound covers the pattern's full declared writes: write_once
// plus every mixed phase at its rate, not just one copy of the keys.
func TestDatasetBytesCoverEveryDeclaredWrite(t *testing.T) {
	p := lmcacheLike() // write_once then read: the keys only
	if got, want := p.datasetBytes(), int64(2*4096*(64<<10)); got != want {
		t.Fatalf("write_once + read: %d, want %d", got, want)
	}
	// codex01's case: a mixed phase writing 10% of 32 x 10/s for 1500 s adds
	// 48000 writes, about 3 GiB of payload after the 4096 keys.
	p.Phases = append(p.Phases, LoadPhase{Kind: "mixed", DurationS: 1500, ReadRatio: 0.9})
	if got, want := p.datasetBytes(), int64(2*(4096+48000)*(64<<10)); got != want {
		t.Fatalf("with a mixed phase: %d, want %d", got, want)
	}
}

// The soak's own schedule check runs the pattern's.
func TestCheckScheduleChecksThePattern(t *testing.T) {
	p, _ := soakTestPlan()
	if err := p.Soak.checkSchedule(); err != nil {
		t.Fatal(err)
	}
	p.Soak.Pattern = lmcacheLike()
	p.Soak.Pattern.Clients, p.Soak.Pattern.RatePerClient = 1, 10 // write_once takes at least 2*4096/10 = 819.2 s
	if p.Soak.ClientKillAtS = 600; p.Soak.checkSchedule() == nil || !strings.Contains(p.Soak.checkSchedule().Error(), "client kill must come after write_once") {
		t.Fatalf("a client kill at 600 s, before write_once's end, passed: %v", p.Soak.checkSchedule())
	}
	p.Soak.ClientKillAtS = 1320
	p.Soak.Pattern = lmcacheLike()
	p.Soak.Pattern.Clients = 0
	if err := p.Soak.checkSchedule(); err == nil || !strings.Contains(err.Error(), "kv_load pattern") {
		t.Fatalf("checkSchedule passed a bad pattern: %v", err)
	}
}
