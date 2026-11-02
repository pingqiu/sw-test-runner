//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
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
	// Step 3 helpers deployed, read back and pinned (M01 load).
	for i := range f.policy.Deployments {
		dep := &f.policy.Deployments[i]
		names := map[string][]string{"m01": {"soak_load.py", "soak_stats.py", "soak_assess.py"}}[dep.NodeID]
		for _, name := range names {
			body, _ := workerFiles.ReadFile("worker/" + name)
			dep.Files = append(dep.Files, infra.DeploymentFile{Path: "/opt/seaweed/worker/" + name, SHA256: hash(body), Size: int64(len(body)), Roles: []string{"adapter"}})
			if dep.NodeID == "m01" {
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
		Setup: Step{Host: "m02", Job: "soak-setup"}, Evidence: Step{Host: "m02", Job: "soak-evidence"}, Assess: Step{Host: "m01", Job: "soak-assess"}}
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
	load, e := SoakLoadArgs(d, f.plan.Smoke, f.plan.Soak)
	task(f.plan.Workload[0].Job, "m01", load, e)
	evidence, e := ProfileArgs(d, "m02", "soak_collect", "", f.plan.setupSettings(), files("m02"))
	task(f.plan.Soak.Evidence.Job, "m02", evidence, e)
	assess, e := SoakAssessArgs(d, f.plan.Smoke, f.plan.Soak)
	task(f.plan.Soak.Assess.Job, "m01", assess, e)
	f.editScenario = func(s *tr.Scenario) { s.Phases[2].Actions[0].Action = "seaweed_kv_soak" }
	if mutate != nil {
		mutate(f, d)
	}
	f.publish(t, nil)
	return f
}

func runEntry(t *testing.T, f *cpuFixture) string {
	t.Helper()
	result, bundle, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, RegisterPack, &f.runtime, nil)
	if err == nil && result != nil && result.Status == tr.StatusPass {
		t.Fatal("a CPU-fixture soak plan cannot pass: there is no real load")
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
	return text.String()
}

// runSoakEntry expects a binding refusal with zero jobs dispatched.
func runSoakEntry(t *testing.T, f *cpuFixture) string {
	t.Helper()
	out := runEntry(t, f)
	if f.node.authorized != 0 {
		t.Fatalf("refused soak plan dispatched %d jobs", f.node.authorized)
	}
	if !strings.Contains(out, "BINDING_REFUSED") {
		t.Fatalf("no binding refusal recorded: %.2000s", out)
	}
	return out
}

// The gate is gone (step 3b): a complete valid soak plan binds, runs its
// preflight, deploy, READY and layout-setup jobs, and reaches the soak phase,
// whose controller refuses here only because the CPU fixture's M01 boot is
// synthetic (a real M01 run is the separately reviewed parity run).
func TestSoakPlanBindsAndRunsToTheSoakPhase(t *testing.T) {
	f := soakProfile(t, 45*time.Minute, nil, nil)
	out := runEntry(t, f)
	if strings.Contains(out, "BINDING_REFUSED: soak") || f.node.authorized == 0 {
		t.Fatalf("soak plan did not bind/run (jobs %d): %s", f.node.authorized, refusalLines(out))
	}
	if !strings.Contains(out, "not the admitted M01 clock") {
		t.Fatalf("soak phase not reached: %.1500s", out)
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
		"setup grow count drift":        {45 * time.Minute, nil, func(f *cpuFixture, _ *inventory.Document) { f.plan.Soak.GrowCount = 9 }, "profile argv"},
		"collection mismatch":           {45 * time.Minute, func(s *SoakPlan) { s.Layout.Collection = "other" }, nil, "collection coherence"},
		"client kill near a fault":      {45 * time.Minute, func(s *SoakPlan) { s.ClientKillAtS = 950 }, nil, "client kill too close"},
		"setup reuses a ready label":    {45 * time.Minute, nil, func(f *cpuFixture, _ *inventory.Document) { f.plan.Soak.Setup.Job = f.plan.Ready[1].Job }, "setup step not exact"},
		"evidence on the wrong host":    {45 * time.Minute, nil, func(f *cpuFixture, _ *inventory.Document) { f.plan.Soak.Evidence.Host = "m01" }, "evidence runs on M02"},
		"evidence reuses a ready label": {45 * time.Minute, nil, func(f *cpuFixture, _ *inventory.Document) { f.plan.Soak.Evidence.Job = f.plan.Ready[1].Job }, "evidence/assess step not exact"},
		"assessor argv drift": {45 * time.Minute, nil, func(f *cpuFixture, _ *inventory.Document) {
			j := f.policy.Jobs["soak-assess"]
			j.Argv = append(append([]string{}, j.Argv[:3]...), strings.Replace(j.Argv[3], "1320", "1321", 1))
			f.policy.Jobs["soak-assess"] = j
		}, "soak task argv"},
		"pause authority carried": {45 * time.Minute, nil, func(f *cpuFixture, _ *inventory.Document) {
			f.policy.ProcessPause = &tr.ProcessPausePlan{Name: "P1", Victim: filer(f), Deadline: time.Now().Add(10 * time.Minute)}
		}, "no pause authority"},
	} {
		t.Run(name, func(t *testing.T) {
			out := runSoakEntry(t, soakProfile(t, tc.hardIn, tc.pre, tc.mutate))
			if !strings.Contains(out, tc.want) {
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
	if !strings.Contains(out, "phase skipped") {
		t.Fatalf("non-soak plan refused for another reason: %s", refusalLines(out))
	}
}

// R1 positive control: a successful soak phase hands over to collect, which
// runs the M02 evidence and assessor path. Only the soak controller is replaced
// (the real one needs the admitted M01 clock); the transition is the product's.
func TestASuccessfulSoakReachesCollectAndTheVerdict(t *testing.T) {
	ran := 0
	soakPhase = func(r *runtimePack, ctx context.Context, a *tr.ActionContext, p Plan, d *inventory.Document, in tr.PackInputs, s *session) (map[string]string, error) {
		ran++
		return admittedResult("completed", "OK", "CPU stand-in for the M01 soak", "seaweed_kv_soak", d), nil
	}
	t.Cleanup(func() { soakPhase = (*runtimePack).soak })
	out := runEntry(t, soakProfile(t, 45*time.Minute, nil, nil))
	if ran != 1 || strings.Contains(out, "PHASE_ORDER_REFUSED") || !strings.Contains(out, "SOAK_VERDICT_FAIL") {
		t.Fatalf("soak (ran %d) did not hand over to collect and the verdict: %.2500s", ran, out)
	}
}

// R2: the assessor exactly as admitted (python -I <helper> --config) imports its
// pinned siblings from a foreign cwd and emits one row Go decodes for this run.
func TestTheAdmittedAssessorArgvRuns(t *testing.T) {
	helpers, _ := filepath.Abs("worker")
	root := t.TempDir()
	d := &inventory.Document{RunID: "run-1", Hosts: []inventory.Host{{ID: "m01", RunRoot: root}},
		Observation: &inventory.Observation{Hosts: []inventory.HostObservation{{HostID: "m01", BootID: "boot-1"}}}}
	argv, err := SoakAssessArgs(d, SmokeSettings{HelperDir: helpers}, &SoakPlan{DurationS: 1800, ClientKillAtS: 1320, Layout: AssignLayout{Collection: "lmcache"}})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", argv...)
	cmd.Dir = t.TempDir()
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("admitted argv failed: %v", err)
	}
	v, err := decodeVerdict(captured(raw), SoakBinding{SoakVerdictSchema, "run-1", "boot-1", root})
	if err != nil || v.Status != "FAIL" || !strings.Contains(v.Error, "M01 boot") {
		t.Fatalf("want this run's FAIL row (empty evidence, foreign boot), got %+v %v", v, err)
	}
}

// R1 round 2: collect after the real elapsed load + quiescence. The admission
// clock jumps past duration + quiescence inside the soak stand-in; collect must
// still bind (the budget was charged once, at admission) and reach the verdict.
func TestCollectAfterTheElapsedSoakStillBinds(t *testing.T) {
	soakPhase = func(r *runtimePack, ctx context.Context, a *tr.ActionContext, p Plan, d *inventory.Document, in tr.PackInputs, s *session) (map[string]string, error) {
		clock = func() time.Time { return time.Now().Add(31 * time.Minute) } // 1800 s load + 60 s quiescence
		return admittedResult("completed", "OK", "CPU stand-in for the M01 soak", "seaweed_kv_soak", d), nil
	}
	t.Cleanup(func() { soakPhase, clock = (*runtimePack).soak, time.Now })
	out := runEntry(t, soakProfile(t, 45*time.Minute, nil, nil))
	if strings.Contains(out, "does not fit") || !strings.Contains(out, "SOAK_VERDICT_FAIL") {
		t.Fatalf("collect after the elapsed soak did not reach the verdict: %s | %.1500s", refusalLines(out), out)
	}
}
