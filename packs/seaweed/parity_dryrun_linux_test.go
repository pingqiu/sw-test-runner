//go:build linux

package seaweed

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
	"gopkg.in/yaml.v3"
)

// TestRealPlanDryRun binds a real plan bundle without any host. Set
// SEAWEED_PLAN_DIR to a directory holding input.json (a ProfileInput) and
// bundle.json (`seaweed-pack profile input.json`). The bundle is rebuilt and
// must equal the CLI's, then every phase binds with the PackInputs a strict-entry
// run carries for it. Lab facts stay in that directory, never in this repo.
func TestRealPlanDryRun(t *testing.T) {
	dir := os.Getenv("SEAWEED_PLAN_DIR")
	if dir == "" {
		t.Skip("SEAWEED_PLAN_DIR not set")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "input.json"))
	if err != nil {
		t.Fatal(err)
	}
	input, err := DecodeProfileInput(raw)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := BuildProfile(input)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := os.ReadFile(filepath.Join(dir, "bundle.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fromCLI ProfileBundle
	if err = json.Unmarshal(cli, &fromCLI); err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(bundle)
	b, _ := json.Marshal(fromCLI)
	if string(a) != string(b) {
		t.Fatal("the CLI bundle differs from the rebuilt one")
	}
	d, err := inventory.Join([]byte(input.Requested), []byte(input.Resolved))
	if err != nil {
		t.Fatal(err)
	}
	work, _ := time.Parse(time.RFC3339Nano, d.Deadlines.Work)
	hard, _ := time.Parse(time.RFC3339Nano, d.Deadlines.Hard)
	transport := map[string]string{"onprem": "rdma", "azure": "http"}[d.Target]
	binding := tr.RunBinding{RunID: d.RunID, Workload: "kv", Transport: transport, Artifacts: tr.FilePin{SHA256: d.ArtifactManifestSHA256},
		WorkDeadline: work, HardDeadline: hard, CleanupReserveMS: d.Deadlines.CleanupReserveMS}
	boots := map[string]string{}
	for _, o := range d.Observation.Hosts {
		boots[o.HostID] = o.BootID
	}
	for _, h := range d.Hosts {
		binding.OutputDir = h.RunRoot
		binding.Hosts = append(binding.Hosts, tr.ApprovedHost{Node: h.ID, Host: net.JoinHostPort(h.Management.Address, strconv.Itoa(h.Management.Port)),
			User: h.Management.User, HostKey: *h.Management.HostKey, Root: h.RunRoot})
	}
	scripts := map[string][]byte{}
	for _, name := range []string{"lab_profile.py", "vf_preflight.py", "kv_smoke.py", "soak_native_worker.py", "soak_oracle.py", "soak_load.py", "soak_stats.py", "soak_assess.py"} {
		if scripts[name], err = workerFiles.ReadFile("worker/" + name); err != nil {
			t.Fatal(err)
		}
	}
	in := tr.PackInputs{Scenario: []byte(bundle.ScenarioYAML), Binding: binding, Jobs: bundle.Jobs, Restarts: bundle.Restarts,
		Boots: boots, Scripts: scripts, Files: input.Files}
	var sealed tr.Scenario
	if err = yaml.Unmarshal(in.Scenario, &sealed); err != nil {
		t.Fatal(err)
	}
	p, pd, err := decodePlan(sealed.Metadata["seaweed_plan"])
	if err != nil {
		t.Fatal(err)
	}
	if err = bind(p, pd, in, time.Now()); err != nil {
		t.Fatalf("real plan does not bind: %v", err)
	}
	var phases []string
	for _, ph := range sealed.Phases {
		phases = append(phases, ph.Actions[0].Action+"/"+ph.Actions[0].Timeout)
	}
	t.Logf("BOUND run %s: %d jobs, %d restarts, phases %v, work left %s", d.RunID, len(bundle.Jobs), len(bundle.Restarts), phases, time.Until(work).Round(time.Second))
}
