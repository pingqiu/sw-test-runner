//go:build linux

package seaweed

import (
	"encoding/json"
	"fmt"
	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
	"gopkg.in/yaml.v3"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A declared-data fixture only: no protected files, policy bootstrap, GO, lease,
// fake owned executor or RunApprovedRequest. Direct/profile binding uses these leaves.
type bindingFixture struct {
	plan         Plan
	policy       tr.ApprovedRunPolicy
	input        ProfileInput
	in           tr.PackInputs
	editScenario func(*tr.Scenario)
	err          error
}

func soakInput(t *testing.T, hardIn time.Duration, pre func(*SoakPlan), docs ...func(*inventory.Document)) ProfileInput {
	t.Helper()
	d := profileFixture(t)
	for _, f := range docs {
		f(d)
	}
	d.Deadlines = inventory.Deadlines{Work: time.Now().Add(hardIn - 3*time.Minute).UTC().Format(time.RFC3339Nano), Hard: time.Now().Add(hardIn).UTC().Format(time.RFC3339Nano), CleanupReserveMS: 60000}
	req := *d
	req.State = "requested"
	req.Observation = nil
	req.Hosts = append([]inventory.Host{}, d.Hosts...)
	for i := range req.Hosts {
		req.Hosts[i].RDMA.Status = "requested"
		req.Hosts[i].RDMA.FabricEvidenceSHA256 = ""
	}
	raw, _ := json.Marshal(req)
	resolved, err := inventory.Resolve(raw, *d)
	if err != nil {
		t.Fatal(err)
	}
	x := &SoakPlan{DurationS: 1800, PacingS: 2, Rotation: &Rotation{Collections: "lmcache", HighWatermark: .3, LowWatermark: .15, MaxVolumesPerCycle: 1, MinRemainingVolumes: 4, CheckIntervalSeconds: 2}, ReadyAssign: &AssignLayout{Collection: "lmcache", Replication: "000", TTL: "2h"}, Faults: []SoakFault{{AtS: 480, Role: "volume", Episode: "E1"}, {AtS: 900, Role: "master", Episode: "E2"}}, Layout: AssignLayout{Collection: "lmcache", Replication: "000", TTL: "2h"}, GrowCount: 8, ClientKillAtS: 1320}
	if pre != nil {
		pre(x)
	}
	in := ProfileInput{Requested: string(raw), Resolved: string(resolved), Smoke: SmokeSettings{HelperDir: "/opt/profile/worker", NativeDir: "/opt/profile/native", Keys: 4, Length: 1 << 20}, Files: map[string]map[string]string{}, Soak: x}
	for _, h := range d.Hosts {
		files := map[string]string{"/usr/bin/python3.12": strings.Repeat("a", 64), "/opt/profile/bin/weed": strings.Repeat("b", 64), "/opt/profile/bin/weed-volume": strings.Repeat("c", 64)}
		for _, name := range []string{"lab_profile.py", "vf_preflight.py", "kv_smoke.py", "soak_native_worker.py", "soak_oracle.py", "soak_load.py", "soak_stats.py", "soak_assess.py"} {
			body, _ := workerFiles.ReadFile("worker/" + name)
			files["/opt/profile/worker/"+name] = hash(body)
		}
		files["/opt/profile/native/seaweedkv_native/_native.so"] = strings.Repeat("d", 64)
		in.Files[h.ID] = files
	}
	return in
}

func soakProfile(t *testing.T, hardIn time.Duration, pre func(*SoakPlan), mutate func(*bindingFixture, *inventory.Document)) *bindingFixture {
	t.Helper()
	input := soakInput(t, hardIn, pre)
	bundle, err := BuildProfile(input)
	f := &bindingFixture{input: input, plan: bundle.Plan, err: err}
	if err != nil {
		return f
	}
	d, err := inventory.Join([]byte(input.Requested), []byte(input.Resolved))
	if err != nil {
		t.Fatal(err)
	}
	work, _ := time.Parse(time.RFC3339Nano, d.Deadlines.Work)
	hard, _ := time.Parse(time.RFC3339Nano, d.Deadlines.Hard)
	binding := tr.RunBinding{RunID: d.RunID, Workload: "kv", Transport: "rdma", Artifacts: tr.FilePin{SHA256: d.ArtifactManifestSHA256}, WorkDeadline: work, HardDeadline: hard, CleanupReserveMS: d.Deadlines.CleanupReserveMS}
	boots := map[string]string{}
	for _, o := range d.Observation.Hosts {
		boots[o.HostID] = o.BootID
	}
	for _, h := range d.Hosts {
		binding.OutputDir = h.RunRoot
		binding.Hosts = append(binding.Hosts, tr.ApprovedHost{Node: h.ID, Host: net.JoinHostPort(h.Management.Address, strconv.Itoa(h.Management.Port)), User: h.Management.User, HostKey: *h.Management.HostKey, Root: h.RunRoot})
	}
	scripts := map[string][]byte{}
	for _, name := range []string{"lab_profile.py", "vf_preflight.py", "kv_smoke.py", "soak_native_worker.py", "soak_oracle.py", "soak_load.py", "soak_stats.py", "soak_assess.py"} {
		scripts[name], _ = workerFiles.ReadFile("worker/" + name)
	}
	f.policy = tr.ApprovedRunPolicy{Jobs: bundle.Jobs, GenerationRestarts: bundle.Restarts}
	f.in = tr.PackInputs{Binding: binding, Jobs: bundle.Jobs, Restarts: bundle.Restarts, Boots: boots, Scripts: scripts, Files: input.Files}
	f.editScenario = func(s *tr.Scenario) { s.Phases[2].Actions[0].Action = tr.SoakAction }
	if mutate != nil {
		mutate(f, d)
	}
	return f
}

func runSoakEntry(t *testing.T, f *bindingFixture) string {
	t.Helper()
	if f.err != nil {
		return f.err.Error()
	}
	s := tr.Scenario{Name: "data-only-bind"}
	for i, name := range phases {
		s.Phases = append(s.Phases, tr.Phase{Name: name, Always: i == 4, Actions: []tr.Action{{Action: name}}})
	}
	if f.editScenario != nil {
		f.editScenario(&s)
	}
	f.in.Scenario, _ = yaml.Marshal(s)
	f.in.Jobs = f.policy.Jobs
	f.in.Restarts = f.policy.GenerationRestarts
	f.in.Pause = f.policy.ProcessPause
	d, err := inventory.Join([]byte(f.plan.Requested), []byte(f.plan.Resolved))
	if err == nil {
		err = bind(f.plan, d, f.in, time.Now())
	}
	if err != nil {
		return err.Error()
	}
	return "BOUND_DATA_ONLY"
}
func refusalLines(s string) string { return fmt.Sprint(s) }

func TestSoakDeclaredDataBinds(t *testing.T) {
	if s := runSoakEntry(t, soakProfile(t, 45*time.Minute, nil, nil)); s != "BOUND_DATA_ONLY" {
		t.Fatal(s)
	}
}
