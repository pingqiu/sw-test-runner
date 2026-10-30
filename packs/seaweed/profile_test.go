package seaweed

import (
	"encoding/json"
	"github.com/pingqiu/sw-test-runner/inventory"
	"os"
	"strings"
	"testing"
)

func profileFixture(t *testing.T) *inventory.Document {
	t.Helper()
	raw, err := os.ReadFile("../../examples/inventory/m01-m02.resolved.json")
	if err != nil {
		t.Fatal(err)
	}
	d, err := inventory.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	// Fixture only: real profile deliberately excludes the example S3 role.
	d.Hosts[1].Roles = []string{"master", "filer", "volume"}
	d.Hosts[1].Services = append(d.Hosts[1].Services, inventory.Service{Name: "rdma_push", Port: 18519})
	return d
}

func TestProfileArgsBindExactInventory(t *testing.T) {
	d := profileFixture(t)
	s := SmokeSettings{HelperDir: "/opt/profile/worker", NativeDir: "/opt/profile/native", Keys: 1, Length: 64}
	args, err := ProfileArgs(d, "m01", "preflight", "", s, map[string]string{"/opt/profile/bin/weed": "a", "/opt/profile/bin/weed-volume": "b"})
	if err != nil {
		t.Fatal(err)
	}
	var c ProfileConfig
	if err = json.Unmarshal([]byte(args[3]), &c); err != nil {
		t.Fatal(err)
	}
	if c.PCI != "/sys/devices/pci0000:00/0000:00:01.0/0000:01:00.1" || c.PhysicalPCI != "/sys/devices/pci0000:00/0000:00:01.0/0000:01:00.0" || c.Firmware != "16.33.1048" {
		t.Fatal("missing pinned fabric identity")
	}
	if c.Host != "m01" || c.BootID != d.Observation.Hosts[0].BootID || c.GIDIndex != *d.Hosts[0].RDMA.GIDIndex || c.Root != d.Hosts[0].RunRoot || c.Deadline != d.Deadlines.Work {
		t.Fatal("profile not derived from inventory")
	}
}

func TestProfileArgsRefuseForeignRoleOrMissingPins(t *testing.T) {
	d := profileFixture(t)
	s := SmokeSettings{HelperDir: "/opt/profile/worker", NativeDir: "/opt/profile/native"}
	for _, tc := range []struct{ host, action, role string }{{"foreign", "preflight", ""}, {"m01", "deploy", "master"}, {"m02", "deploy", "s3"}, {"m02", "collect", "volume"}, {"m02", "arbitrary", ""}} {
		if _, err := ProfileArgs(d, tc.host, tc.action, tc.role, s, nil); err == nil {
			t.Fatalf("admitted %+v", tc)
		}
	}
	if _, err := ProfileArgs(d, "m02", "deploy", "volume", s, nil); err == nil {
		t.Fatal("missing product pin admitted")
	}
	d.Hosts[0].ID = "foreign"
	if _, err := ProfileArgs(d, "foreign", "preflight", "", s, nil); err == nil {
		t.Fatal("foreign named host admitted to M01/M02 profile")
	}
}

func TestProfileBundleIsPinnedDataOnly(t *testing.T) {
	d := profileFixture(t)
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
	in := ProfileInput{Requested: string(raw), Resolved: string(resolved), Smoke: SmokeSettings{HelperDir: "/opt/profile/worker", NativeDir: "/opt/profile/native", Keys: 4, Length: 1048576}, Files: map[string]map[string]string{}}
	for _, host := range []string{"m01", "m02"} {
		in.Files[host] = map[string]string{"/usr/bin/python3.12": strings.Repeat("a", 64), "/opt/profile/bin/weed": strings.Repeat("b", 64), "/opt/profile/bin/weed-volume": strings.Repeat("c", 64)}
		for _, name := range []string{"lab_profile.py", "vf_preflight.py", "kv_smoke.py", "soak_native_worker.py", "soak_oracle.py"} {
			body, _ := workerFiles.ReadFile("worker/" + name)
			in.Files[host]["/opt/profile/worker/"+name] = hash(body)
		}
	}
	bundle, err := BuildProfile(in)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.ExecutionAuthorized || len(bundle.Jobs) != 10 || len(bundle.Plan.Deploy) != 3 || len(bundle.Plan.Workload) != 1 || !strings.Contains(bundle.ScenarioYAML, "always: true") {
		t.Fatalf("bad data-only profile %+v", bundle)
	}
	plan, _ := MarshalPlan(bundle.Plan)
	if result, err := PlanPhase(string(plan), "seaweed_workload"); err != nil || result.ExecutionAuthorized || result.Code != "S0B_REQUIRED" {
		t.Fatalf("plan=%+v %v", result, err)
	}
	in.Files["m01"]["/opt/profile/worker/lab_profile.py"] = strings.Repeat("d", 64)
	if _, err := BuildProfile(in); err == nil {
		t.Fatal("different helper emitted")
	}
}
