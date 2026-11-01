//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/infra"
	"github.com/pingqiu/sw-test-runner/inventory"
)

// soakProfile is the strict-entry CPU profile (same fake transport) carrying a
// complete, internally consistent soak plan. mutate runs before publish.
func soakProfile(t *testing.T, hardIn time.Duration, pre func(*SoakPlan), mutate func(*cpuFixture, *inventory.Document)) *cpuFixture {
	t.Helper()
	f := cpuProfile(t)
	d, err := inventory.Decode([]byte(f.plan.Resolved))
	if err != nil {
		t.Fatal(err)
	}
	// Step 3 helpers deployed, read back and pinned (M01 load, M02 sampler).
	for i := range f.policy.Deployments {
		dep := &f.policy.Deployments[i]
		names := map[string][]string{"m01": {"soak_load.py", "soak_stats.py"}, "m02": {"soak_sampler.py", "soak_stats.py"}}[dep.NodeID]
		for _, name := range names {
			body, _ := workerFiles.ReadFile("worker/" + name)
			dep.Files = append(dep.Files, infra.DeploymentFile{Path: "/opt/seaweed/worker/" + name, SHA256: hash(body), Size: int64(len(body)), Roles: []string{"adapter"}})
			if dep.NodeID == "m01" || name == "soak_sampler.py" {
				f.scriptPins = append(f.scriptPins, f.bytes(t, name, body))
			}
		}
	}
	f.policy.Scripts = f.scriptPins
	f.binding.Artifacts = f.write(t, "artifacts.json", tr.ApprovedArtifacts{Version: "s0b-artifacts-v1", Deployments: f.policy.Deployments})
	d.ArtifactManifestSHA256 = f.binding.Artifacts.SHA256
	f.binding.WorkDeadline = time.Now().Add(hardIn - 3*time.Minute)
	f.binding.HardDeadline = time.Now().Add(hardIn)
	d.Deadlines.Work = f.binding.WorkDeadline.UTC().Format(time.RFC3339Nano)
	d.Deadlines.Hard = f.binding.HardDeadline.UTC().Format(time.RFC3339Nano)
	var req inventory.Document
	if err = json.Unmarshal([]byte(f.plan.Requested), &req); err != nil {
		t.Fatal(err)
	}
	req.Deadlines, req.ArtifactManifestSHA256 = d.Deadlines, d.ArtifactManifestSHA256
	raw, _ := json.Marshal(req)
	resolved, err := inventory.Resolve(raw, *d)
	if err != nil {
		t.Fatal(err)
	}
	f.plan.Requested, f.plan.Resolved = string(raw), string(resolved)
	f.plan.Soak = &SoakPlan{DurationS: 1800, PacingS: 2,
		Rotation:    &Rotation{Collections: "lmcache", HighWatermark: 0.3, LowWatermark: 0.15, MaxVolumesPerCycle: 1, MinRemainingVolumes: 4, CheckIntervalSeconds: 2},
		ReadyAssign: &AssignLayout{Collection: "lmcache", Replication: "000", TTL: "2h"},
		FaultReady:  []Step{{Host: "m01", Job: "fault-ready-m01"}, {Host: "m02", Job: "fault-ready-m02"}},
		Faults:      []SoakFault{{AtS: 480, Role: "volume", Episode: "E1"}, {AtS: 900, Role: "master", Episode: "E2"}},
		Layout:      AssignLayout{Collection: "lmcache", Replication: "000", TTL: "2h"}, GrowCount: 8, ClientKillAtS: 1320,
		Setup: Step{Host: "m02", Job: "soak-setup"}, Sampler: Step{Host: "m02", Job: "soak-sampler"}}
	if pre != nil { // before the pins, so every job stays consistent with the plan
		pre(f.plan.Soak)
	}
	files := func(host string) map[string]string {
		out := map[string]string{}
		for _, dep := range f.policy.Deployments {
			if dep.NodeID == host {
				for _, file := range dep.Files {
					out[file.Path] = file.SHA256
				}
			}
		}
		return out
	}
	pin := func(host, action, role string, faultReady bool) []string {
		args, e := ProfileArgs(d, host, action, role, f.plan.profileSettings(faultReady), files(host))
		if e != nil {
			t.Fatal(e)
		}
		return args
	}
	for _, group := range []struct {
		steps  []Step
		action string
	}{{f.plan.Preflight, "preflight"}, {f.plan.Deploy, "deploy"}, {f.plan.Ready, "ready"}, {f.plan.Collect, "collect"}} {
		for _, step := range group.steps {
			j := f.policy.Jobs[step.Job]
			j.Argv = pin(step.Host, group.action, step.Role, false)
			f.policy.Jobs[step.Job] = j
		}
	}
	f.policy.GenerationRestarts = map[string]tr.GenerationRestartPlan{}
	for _, fault := range f.plan.Soak.Faults {
		victim := ""
		for _, s := range f.plan.Deploy {
			if s.Host == "m02" && s.Role == fault.Role {
				victim = s.Job
			}
		}
		j := f.policy.Jobs[victim]
		j.Argv = append(pin("m02", "deploy", fault.Role, false), "--replacement-"+fault.Role)
		f.policy.Jobs["replacement-"+fault.Role] = j
		f.policy.GenerationRestarts[fault.Episode] = tr.GenerationRestartPlan{Victim: victim, Replacement: "replacement-" + fault.Role}
	}
	for _, s := range f.plan.Soak.FaultReady {
		var ready Step
		for _, r := range f.plan.Ready {
			if r.Host == s.Host {
				ready = r
			}
		}
		j := f.policy.Jobs[ready.Job]
		j.Argv = pin(s.Host, "ready", "", true)
		f.policy.Jobs[s.Job] = j
	}
	task := func(label, host string, args []string, e error) {
		if e != nil {
			t.Fatal(e)
		}
		j := f.policy.Jobs[f.plan.Ready[0].Job] // the pinned python3.12 interpreter
		j.Node, j.Argv = host, args
		f.policy.Jobs[label] = j
	}
	setup, e := ProfileArgs(d, "m02", "soak_setup", "", f.plan.setupSettings(), files("m02"))
	task(f.plan.Soak.Setup.Job, "m02", setup, e)
	sampler, e := SoakSamplerArgs(d, f.plan.Smoke, f.plan.Soak)
	task(f.plan.Soak.Sampler.Job, "m02", sampler, e)
	load, e := SoakLoadArgs(d, f.plan.Smoke, f.plan.Soak)
	task(f.plan.Workload[0].Job, "m01", load, e)
	f.editScenario = func(s *tr.Scenario) { s.Phases[2].Actions[0].Action = "seaweed_kv_soak" }
	if mutate != nil {
		mutate(f, d)
	}
	f.publish(t, nil)
	return f
}

func runSoakEntry(t *testing.T, f *cpuFixture) string {
	t.Helper()
	result, bundle, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, RegisterPack, &f.runtime, nil)
	if err == nil && result != nil && result.Status == tr.StatusPass {
		t.Fatal("soak plan passed while its worker is unadmitted")
	}
	if f.node.authorized != 0 {
		t.Fatalf("soak plan dispatched %d jobs", f.node.authorized)
	}
	// The refusal text lives in the result files, not in the bundle path.
	var text strings.Builder
	fmt.Fprint(&text, err, " ", result)
	_ = filepath.WalkDir(string(bundle), func(p string, e fs.DirEntry, werr error) error {
		if werr == nil && !e.IsDir() {
			if b, rerr := os.ReadFile(p); rerr == nil {
				text.Write(b)
			}
		}
		return nil
	})
	if !strings.Contains(text.String(), "BINDING_REFUSED") {
		t.Fatalf("no binding refusal recorded: %.2000s", text.String())
	}
	return text.String()
}

// A complete, valid soak plan binds every rule, then refuses at the staging
// gate: SOAK_WORKER_UNADMITTED, and zero jobs of any phase are dispatched.
func TestSoakPlanIsInertUntilWorkerAdmitted(t *testing.T) {
	out := runSoakEntry(t, soakProfile(t, 45*time.Minute, nil, nil))
	if !strings.Contains(out, "SOAK_WORKER_UNADMITTED") {
		t.Fatalf("valid soak plan refused for another reason: %s", refusalLines(out))
	}
}

func refusalLines(s string) string {
	return strings.Join(regexp.MustCompile(`BINDING_REFUSED[^"\\]{0,200}`).FindAllString(s, 3), " | ")
}

// Each rule refuses for its own named reason (never at the staging gate),
// with zero jobs dispatched. Fixtures stay valid for the core's own policy
// checks, so the refusal comes from the pack's bind.
func TestSoakPlanBindRefusals(t *testing.T) {
	type mut = func(*cpuFixture, *inventory.Document)
	filer := func(f *cpuFixture) string {
		for _, s := range f.plan.Deploy {
			if s.Role == "filer" {
				return s.Job
			}
		}
		t.Fatal("no filer deploy step")
		return ""
	}
	for name, tc := range map[string]struct {
		hardIn time.Duration
		pre    func(*SoakPlan)
		mutate mut
		want   string
	}{
		"two volume faults": {45 * time.Minute, nil, func(f *cpuFixture, _ *inventory.Document) { f.plan.Soak.Faults[1].Role = "volume" }, "one volume and one master"},
		"extra episode": {45 * time.Minute, nil, func(f *cpuFixture, _ *inventory.Document) {
			j := f.policy.Jobs[filer(f)]
			j.Argv = append(append([]string{}, j.Argv...), "--replacement-filer")
			f.policy.Jobs["replacement-filer"] = j
			f.policy.GenerationRestarts["E3"] = tr.GenerationRestartPlan{Victim: filer(f), Replacement: "replacement-filer"}
		}, "exactly the scheduled faults"},
		"replacement reuses a ready label": {45 * time.Minute, nil, func(f *cpuFixture, _ *inventory.Document) {
			e := f.policy.GenerationRestarts["E1"]
			e.Replacement = f.plan.Ready[1].Job
			f.policy.GenerationRestarts["E1"] = e
		}, "not the exact private helper"},
		"at_s overflow":    {45 * time.Minute, nil, func(f *cpuFixture, _ *inventory.Document) { f.plan.Soak.Faults[1].AtS = math.MaxInt64 }, "within the load"},
		"faults too close": {45 * time.Minute, nil, func(f *cpuFixture, _ *inventory.Document) { f.plan.Soak.Faults[1].AtS = 480 + 100 }, "fault budget plus recovery apart"},
		"faults out of order": {45 * time.Minute, nil, func(f *cpuFixture, _ *inventory.Document) {
			f.plan.Soak.Faults[0].AtS, f.plan.Soak.Faults[1].AtS = 900, 480
		}, "fault budget plus recovery apart"},
		"last fault leaves no quiescence":         {45 * time.Minute, nil, func(f *cpuFixture, _ *inventory.Document) { f.plan.Soak.Faults[1].AtS = 1700 }, "no recovery and quiescence"},
		"master fault without ready_assign":       {45 * time.Minute, nil, func(f *cpuFixture, _ *inventory.Document) { f.plan.Soak.ReadyAssign = nil }, "ready_assign layout"},
		"beyond the 60 min horizon":               {70 * time.Minute, nil, nil, "60 min soak horizon"},
		"does not fit the work deadline":          {20 * time.Minute, nil, nil, "does not fit the admitted work deadline"},
		"pacing below range":                      {45 * time.Minute, func(s *SoakPlan) { s.PacingS = 0.1 }, nil, "duration/pacing"},
		"rotation product-invalid":                {45 * time.Minute, func(s *SoakPlan) { s.Rotation.MinRemainingVolumes = 0 }, nil, "rotation bounds"},
		"soak and restart together":               {45 * time.Minute, nil, func(f *cpuFixture, _ *inventory.Document) { f.plan.Restart = &ReadRestart{Episode: "E1"} }, "exclusive"},
		"soak plan with the plain workload phase": {45 * time.Minute, nil, func(f *cpuFixture, _ *inventory.Document) { f.editScenario = nil }, "phase skipped"},
		"fault ready without the Assign probe": {45 * time.Minute, nil, func(f *cpuFixture, _ *inventory.Document) {
			j := f.policy.Jobs["fault-ready-m02"]
			j.Argv = f.policy.Jobs[f.plan.Ready[1].Job].Argv
			f.policy.Jobs["fault-ready-m02"] = j
		}, "profile argv"},
		"load argv drift": {45 * time.Minute, nil, func(f *cpuFixture, _ *inventory.Document) {
			j := f.policy.Jobs[f.plan.Workload[0].Job]
			j.Argv = append(append([]string{}, j.Argv[:3]...), strings.Replace(j.Argv[3], `"pacing_s":2`, `"pacing_s":3`, 1))
			f.policy.Jobs[f.plan.Workload[0].Job] = j
		}, "soak task argv"},
		"sampler horizon drift": {45 * time.Minute, nil, func(f *cpuFixture, _ *inventory.Document) {
			j := f.policy.Jobs["soak-sampler"]
			j.Argv = append(append([]string{}, j.Argv[:3]...), strings.Replace(j.Argv[3], "1860", "1900", 1))
			f.policy.Jobs["soak-sampler"] = j
		}, "soak task argv"},
		"setup grow count drift":       {45 * time.Minute, nil, func(f *cpuFixture, _ *inventory.Document) { f.plan.Soak.GrowCount = 9 }, "profile argv"},
		"collection mismatch":          {45 * time.Minute, func(s *SoakPlan) { s.Layout.Collection = "other" }, nil, "collection coherence"},
		"client kill near a fault":     {45 * time.Minute, func(s *SoakPlan) { s.ClientKillAtS = 950 }, nil, "client kill too close"},
		"sampler reuses a ready label": {45 * time.Minute, nil, func(f *cpuFixture, _ *inventory.Document) { f.plan.Soak.Sampler.Job = f.plan.Ready[1].Job }, "setup/sampler step not exact"},
		"pause authority carried": {45 * time.Minute, nil, func(f *cpuFixture, _ *inventory.Document) {
			f.policy.ProcessPause = &tr.ProcessPausePlan{Name: "P1", Victim: filer(f), Deadline: time.Now().Add(10 * time.Minute)}
		}, "no pause authority"},
	} {
		t.Run(name, func(t *testing.T) {
			out := runSoakEntry(t, soakProfile(t, tc.hardIn, tc.pre, tc.mutate))
			if strings.Contains(out, "SOAK_WORKER_UNADMITTED") || !strings.Contains(out, tc.want) {
				t.Fatalf("want refusal %q, got: %s", tc.want, refusalLines(out))
			}
		})
	}
}

// Non-soak plans are unchanged in both directions: a seaweed_kv_soak phase in
// a plan without Soak is refused, with zero jobs.
func TestNonSoakPlanRefusesTheSoakPhase(t *testing.T) {
	f := cpuProfile(t)
	f.editScenario = func(s *tr.Scenario) { s.Phases[2].Actions[0].Action = "seaweed_kv_soak" }
	f.publish(t, nil)
	out := runSoakEntry(t, f)
	if strings.Contains(out, "SOAK_WORKER_UNADMITTED") {
		t.Fatalf("non-soak plan reached the soak gate: %s", out)
	}
}
