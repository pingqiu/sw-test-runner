package seaweed

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// loadConfig is the --config JSON of the fixture's admitted load job.
func loadConfig(t *testing.T, f *bindingFixture) map[string]json.RawMessage {
	t.Helper()
	argv := f.policy.Jobs[f.plan.Workload[0].Job].Argv
	var c map[string]json.RawMessage
	if len(argv) < 2 || argv[len(argv)-2] != "--config" || json.Unmarshal([]byte(argv[len(argv)-1]), &c) != nil {
		t.Fatalf("load argv %q", argv)
	}
	return c
}

// A pattern reaches the admitted load argv and the plan still binds and runs to
// the soak phase; without one, the soak's argv carries no pattern key at all.
func TestKVLoadPatternReachesTheAdmittedLoadArgv(t *testing.T) {
	if _, ok := loadConfig(t, soakProfile(t, 45*time.Minute, nil, nil))["pattern"]; ok {
		t.Fatal("the soak's load argv carries a pattern")
	}
	f := soakProfile(t, 45*time.Minute, func(x *SoakPlan) { x.Pattern = lmcacheLike() }, nil)
	var got LoadPattern
	if err := json.Unmarshal(loadConfig(t, f)["pattern"], &got); err != nil || got.Clients != 32 || got.ZipfS != 1.1 || got.Phases[1].DurationS != 600 {
		t.Fatalf("pattern in the load argv: %+v, %v", got, err)
	}
	if out := runSoakEntry(t, f); out != "BOUND_DATA_ONLY" {
		t.Fatalf("kv_load data did not bind: %s", out)
	}
}

// The normal builder doesn't panic on an empty pattern: it builds profile args
// (datasetBytes) before anything checks the pattern, and bind's schedule check
// then refuses the plan it built.
func TestBuildProfileRefusesAnEmptyPatternWithoutPanic(t *testing.T) {
	var in ProfileInput
	soakProfile(t, 45*time.Minute, func(x *SoakPlan) { x.Pattern = &LoadPattern{} }, func(f *cpuFixture, d *inventory.Document) {
		settings := *f.plan.Soak
		settings.Setup, settings.Evidence, settings.Assess, settings.FaultReady = Step{}, Step{}, Step{}, nil
		in = ProfileInput{Requested: f.plan.Requested, Resolved: f.plan.Resolved, Smoke: f.plan.Smoke, Files: map[string]map[string]string{}, Soak: &settings}
		for _, dep := range f.policy.Deployments {
			in.Files[dep.NodeID] = map[string]string{}
			for _, file := range dep.Files {
				in.Files[dep.NodeID][file.Path] = file.SHA256
			}
		}
	})
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("BuildProfile panicked on pattern {}: %v", r)
		}
	}()
	bundle, err := BuildProfile(in)
	if err != nil {
		return // refused while building: also fine
	}
	if err := bundle.Plan.Soak.checkSchedule(); err == nil || !strings.Contains(err.Error(), "kv_load pattern") {
		t.Fatalf("the plan built from pattern {} passes the schedule check: %v", err)
	}
}

// kv_load runs only a profile with a pattern, and kv_soak only one without,
// refused before any host is reached.
func TestKVLoadSelectorMatchesTheProfile(t *testing.T) {
	var in ProfileInput
	for _, withPattern := range []bool{false, true} {
		in = soakInput(t, 45*time.Minute, func(x *SoakPlan) {
			if withPattern {
				x.Pattern = lmcacheLike()
			}
		})
		raw, _ := json.Marshal(in)
		profile := filepath.Join(t.TempDir(), "profile.json")
		if err := os.WriteFile(profile, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		c := CloudConfig{KVSoak: withPattern, KVLoad: !withPattern, SSHKey: "unused", Output: t.TempDir(), SoakRelease: t.TempDir(), SoakProfile: profile}
		if _, err := RunCloudHTTP(soakCtx(t), []byte(in.Requested), c); err == nil || !strings.Contains(err.Error(), "kv_load needs a profile with a load pattern") {
			t.Errorf("kv_load=%v on a profile with pattern=%v: err %v", c.KVLoad, withPattern, err)
		}
	}
	both := CloudConfig{KVSoak: true, KVLoad: true, SSHKey: "unused", Output: t.TempDir(), SoakRelease: t.TempDir()}
	if _, err := RunCloudHTTP(soakCtx(t), []byte(in.Requested), both); err == nil || !strings.Contains(err.Error(), "kv_soak requires") {
		t.Errorf("kv_soak and kv_load together: %v", err)
	}
}
