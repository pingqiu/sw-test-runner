//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
)

func newHTTPFixture(t *testing.T) *cpuFixture {
	f := newCPUFixture(t)
	var d inventory.Document
	if err := json.Unmarshal([]byte(f.plan.Resolved), &d); err != nil {
		t.Fatal(err)
	}
	h := d.Hosts[1]
	h.Roles = append(h.Roles, "client")
	h.RDMA = inventory.RDMA{Status: "not_applicable"}
	h.ResourceID = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/test/providers/Microsoft.Compute/virtualMachines/smoke"
	h.Services = nil
	for _, s := range d.Hosts[1].Services {
		if !strings.HasPrefix(s.Name, "rdma") && s.Name != "dc_control" {
			h.Services = append(h.Services, s)
		}
	}
	d.Hosts = []inventory.Host{h}
	d.Observation.Hosts = d.Observation.Hosts[1:]
	d.Target, d.Transport = "azure", "http"
	d.ManagedResourceIDs = []string{h.ResourceID}
	f.binding.Hosts = f.binding.Hosts[1:]
	f.binding.Transport = "http"
	f.binding.Inventory = f.write(t, "inventory.json", tr.ApprovedInventory{Version: "s0b-inventory-v1", Hosts: f.binding.Hosts})
	f.policy.Deployments = f.policy.Deployments[1:]
	f.binding.Artifacts = f.write(t, "artifacts.json", tr.ApprovedArtifacts{Version: "s0b-artifacts-v1", Deployments: f.policy.Deployments})
	d.ArtifactManifestSHA256 = f.binding.Artifacts.SHA256
	requested := d
	requested.State = "requested"
	requested.Observation = nil
	raw, _ := json.Marshal(requested)
	resolved, err := inventory.Resolve(raw, d)
	if err != nil {
		t.Fatal(err)
	}
	f.plan.Requested, f.plan.Resolved = string(raw), string(resolved)
	keep := func(steps []Step) []Step {
		var out []Step
		for _, s := range steps {
			if s.Host == h.ID {
				out = append(out, s)
			}
		}
		return out
	}
	f.plan.Preflight, f.plan.Ready, f.plan.Collect = keep(f.plan.Preflight), keep(f.plan.Ready), keep(f.plan.Collect)
	f.plan.Workload = []Step{{Host: h.ID, Role: "client", Job: "smoke"}}
	for label, j := range f.policy.Jobs {
		if j.Node != h.ID {
			delete(f.policy.Jobs, label)
		}
	}
	config := SmokeConfig{RunID: d.RunID, Client: h.ID, Transport: "http", FilerGRPC: endpoint(h, "filer_grpc"), FilerHTTP: "http://" + endpoint(h, "filer_http"), Root: "/testops/" + d.RunID + "/" + h.ID, NativeDir: f.plan.Smoke.NativeDir, HelperDir: f.plan.Smoke.HelperDir, Keys: f.plan.Smoke.Keys, Length: f.plan.Smoke.Length}
	c, _ := json.Marshal(config)
	f.policy.Jobs["smoke"] = tr.JobPin{Node: h.ID, Executable: "/opt/seaweed/interpreter", SHA256: strings.Repeat("f", 64), Argv: []string{"-I", "/opt/seaweed/worker/kv_smoke.py", "--config", string(c)}}
	f.publish(t, map[string]string{h.ID: "/service/private-key"})
	return f
}

func TestStrictAzureHTTPProfileUsesSameOwnedFivePhases(t *testing.T) {
	f := newHTTPFixture(t)
	result, _, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, RegisterPack, &f.runtime, nil)
	if err != nil || result == nil || result.Status != tr.StatusPass || len(result.Phases) != 5 || f.node.authorized != 8 || len(f.node.live) != 0 {
		t.Fatalf("result=%+v err=%v authorized=%d live=%d", result, err, f.node.authorized, len(f.node.live))
	}
}

func TestHTTPProfileRefusesAuthorityOrTransportMismatchBeforeEffects(t *testing.T) {
	for _, mode := range []string{"rdma-go", "onprem-http", "cloud-rdma", "missing-workload", "rdma-selector", "helper-unpinned", "unknown-cleanup"} {
		t.Run(mode, func(t *testing.T) {
			f := newHTTPFixture(t)
			if mode == "cloud-rdma" {
				// Keep independently valid DC facts/argv, changing only the target
				// resource declaration. Refusal must be the profile, not bad data.
				f = newCPUFixture(t)
				var observed inventory.Document
				if err := json.Unmarshal([]byte(f.plan.Resolved), &observed); err != nil {
					t.Fatal(err)
				}
				observed.Target = "azure"
				for i := range observed.Hosts {
					id := "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/test/providers/Microsoft.Compute/virtualMachines/" + observed.Hosts[i].ID
					observed.Hosts[i].ResourceID = id
					observed.ManagedResourceIDs = append(observed.ManagedResourceIDs, id)
				}
				requested := observed
				requested.Hosts = append([]inventory.Host{}, observed.Hosts...)
				requested.State = "requested"
				requested.Observation = nil
				for i := range requested.Hosts {
					requested.Hosts[i].RDMA.Status = "requested"
					requested.Hosts[i].RDMA.FabricEvidenceSHA256 = ""
				}
				raw, _ := json.Marshal(requested)
				resolved, err := inventory.Resolve(raw, observed)
				if err != nil {
					t.Fatal(err)
				}
				f.plan.Requested, f.plan.Resolved = string(raw), string(resolved)
			}
			switch mode {
			case "rdma-go":
				f.binding.Transport = "rdma"
			case "onprem-http":
				var requested, observed inventory.Document
				json.Unmarshal([]byte(f.plan.Requested), &requested)
				json.Unmarshal([]byte(f.plan.Resolved), &observed)
				requested.Target = "onprem"
				observed.Target = "onprem"
				requested.ManagedResourceIDs = []string{}
				observed.ManagedResourceIDs = []string{}
				requested.Hosts[0].ResourceID = ""
				observed.Hosts[0].ResourceID = ""
				raw, _ := json.Marshal(requested)
				resolved, err := inventory.Resolve(raw, observed)
				if err != nil {
					t.Fatal(err)
				}
				f.plan.Requested, f.plan.Resolved = string(raw), string(resolved)
			case "missing-workload":
				f.plan.Workload = nil
			case "rdma-selector":
				j := f.policy.Jobs["smoke"]
				j.Argv = append([]string{}, j.Argv...)
				j.Argv[3] = strings.TrimSuffix(j.Argv[3], "}") + `,"gid_index":0}`
				f.policy.Jobs["smoke"] = j
			case "helper-unpinned":
				f.policy.Scripts = f.policy.Scripts[:1]
			case "unknown-cleanup":
				f.node.unknown = true
			}
			if mode == "cloud-rdma" {
				f.publish(t, nil)
			} else {
				f.publish(t, map[string]string{"m02": "/service/private-key"})
			}
			result, _, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, RegisterPack, &f.runtime, nil)
			if err == nil && result != nil && result.Status == tr.StatusPass {
				t.Fatal("mismatched/unknown HTTP profile passed")
			}
			if mode != "unknown-cleanup" && f.node.authorized != 0 {
				t.Fatalf("refusal after effects: %d", f.node.authorized)
			}
		})
	}
}
