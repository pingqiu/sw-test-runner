//go:build linux

package seaweed

import (
	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
	"math"
	"strings"
	"testing"
	"time"
)

func TestSoakPlanBindRefusals(t *testing.T) {
	type mut = func(*bindingFixture, *inventory.Document)
	filer := func(f *bindingFixture) string {
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
		"two volume faults": {45 * time.Minute, nil, func(f *bindingFixture, _ *inventory.Document) { f.plan.Soak.Faults[1].Role = "volume" }, "one volume and one master"},
		"extra episode": {45 * time.Minute, nil, func(f *bindingFixture, _ *inventory.Document) {
			j := f.policy.Jobs[filer(f)]
			j.Argv = append(append([]string{}, j.Argv...), "--replacement-filer")
			f.policy.Jobs["replacement-filer"] = j
			f.policy.GenerationRestarts["E3"] = tr.GenerationRestartPlan{Victim: filer(f), Replacement: "replacement-filer"}
		}, "exactly the scheduled faults"},
		"replacement reuses a ready label": {45 * time.Minute, nil, func(f *bindingFixture, _ *inventory.Document) {
			e := f.policy.GenerationRestarts["E1"]
			e.Replacement = f.plan.Ready[1].Job
			f.policy.GenerationRestarts["E1"] = e
		}, "not the exact private helper"},
		"at_s overflow":    {45 * time.Minute, nil, func(f *bindingFixture, _ *inventory.Document) { f.plan.Soak.Faults[1].AtS = math.MaxInt64 }, "within the load"},
		"faults too close": {45 * time.Minute, nil, func(f *bindingFixture, _ *inventory.Document) { f.plan.Soak.Faults[1].AtS = 480 + 100 }, "fault budget plus recovery apart"},
		"faults out of order": {45 * time.Minute, nil, func(f *bindingFixture, _ *inventory.Document) {
			f.plan.Soak.Faults[0].AtS, f.plan.Soak.Faults[1].AtS = 900, 480
		}, "fault budget plus recovery apart"},
		"last fault leaves no quiescence":         {45 * time.Minute, nil, func(f *bindingFixture, _ *inventory.Document) { f.plan.Soak.Faults[1].AtS = 1700 }, "no recovery and quiescence"},
		"master fault without ready_assign":       {45 * time.Minute, nil, func(f *bindingFixture, _ *inventory.Document) { f.plan.Soak.ReadyAssign = nil }, "ready_assign layout"},
		"beyond the 60 min horizon":               {70 * time.Minute, nil, nil, "60 min soak horizon"},
		"does not fit the work deadline":          {20 * time.Minute, nil, nil, "does not fit the admitted work deadline"},
		"pacing below range":                      {45 * time.Minute, func(s *SoakPlan) { s.PacingS = 0.1 }, nil, "duration/pacing"},
		"rotation product-invalid":                {45 * time.Minute, func(s *SoakPlan) { s.Rotation.MinRemainingVolumes = 0 }, nil, "rotation bounds"},
		"soak and restart together":               {45 * time.Minute, nil, func(f *bindingFixture, _ *inventory.Document) { f.plan.Restart = &ReadRestart{Episode: "E1"} }, "exclusive"},
		"soak plan with the plain workload phase": {45 * time.Minute, nil, func(f *bindingFixture, _ *inventory.Document) { f.editScenario = nil }, "phase skipped"},
		"fault ready without the Assign probe": {45 * time.Minute, nil, func(f *bindingFixture, _ *inventory.Document) {
			j := f.policy.Jobs["fault-ready-m02"]
			j.Argv = f.policy.Jobs[f.plan.Ready[1].Job].Argv
			f.policy.Jobs["fault-ready-m02"] = j
		}, "profile argv"},
		"load argv drift": {45 * time.Minute, nil, func(f *bindingFixture, _ *inventory.Document) {
			j := f.policy.Jobs[f.plan.Workload[0].Job]
			j.Argv = append(append([]string{}, j.Argv[:3]...), strings.Replace(j.Argv[3], `"pacing_s":2`, `"pacing_s":3`, 1))
			f.policy.Jobs[f.plan.Workload[0].Job] = j
		}, "soak task argv"},
		"setup grow count drift":        {45 * time.Minute, nil, func(f *bindingFixture, _ *inventory.Document) { f.plan.Soak.GrowCount = 9 }, "profile argv"},
		"collection mismatch":           {45 * time.Minute, func(s *SoakPlan) { s.Layout.Collection = "other" }, nil, "collection coherence"},
		"client kill near a fault":      {45 * time.Minute, func(s *SoakPlan) { s.ClientKillAtS = 950 }, nil, "client kill too close"},
		"setup reuses a ready label":    {45 * time.Minute, nil, func(f *bindingFixture, _ *inventory.Document) { f.plan.Soak.Setup.Job = f.plan.Ready[1].Job }, "setup step not exact"},
		"evidence on the wrong host":    {45 * time.Minute, nil, func(f *bindingFixture, _ *inventory.Document) { f.plan.Soak.Evidence.Host = "m01" }, "evidence runs on M02"},
		"evidence reuses a ready label": {45 * time.Minute, nil, func(f *bindingFixture, _ *inventory.Document) { f.plan.Soak.Evidence.Job = f.plan.Ready[1].Job }, "evidence/assess step not exact"},
		"assessor argv drift": {45 * time.Minute, nil, func(f *bindingFixture, _ *inventory.Document) {
			j := f.policy.Jobs["soak-assess-m01"]
			j.Argv = append(append([]string{}, j.Argv[:3]...), strings.Replace(j.Argv[3], "1320", "1321", 1))
			f.policy.Jobs["soak-assess-m01"] = j
		}, "soak task argv"},
		"pause authority carried": {45 * time.Minute, nil, func(f *bindingFixture, _ *inventory.Document) {
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
