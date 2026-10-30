//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/infra"
	"github.com/pingqiu/sw-test-runner/inventory"
)

// Same strict entry and compiled transport fake as the accepted pack controls.
// This proves profile binding only, never actual hardware or service readiness.
func cpuProfile(t *testing.T) *cpuFixture {
	t.Helper()
	f := newCPUFixture(t)
	d, err := inventory.Decode([]byte(f.plan.Resolved))
	if err != nil {
		t.Fatal(err)
	}
	d.Hosts[1].Roles = []string{"master", "filer", "volume"}
	d.Hosts[1].Services = append(d.Hosts[1].Services, inventory.Service{Name: "rdma_push", Port: 18519})
	for i := range d.Hosts {
		d.Hosts[i].RDMA.Device = "mlx5_0"
	}
	for i := range f.policy.Deployments {
		dep := &f.policy.Deployments[i]
		for n := range dep.Files {
			if dep.Files[n].Path == "/opt/seaweed/interpreter" {
				dep.Files[n].Path = "/usr/bin/python3.12"
			}
		}
		for _, name := range []string{"lab_profile.py", "vf_preflight.py"} {
			body, _ := workerFiles.ReadFile("worker/" + name)
			dep.Files = append(dep.Files, infra.DeploymentFile{Path: "/opt/seaweed/worker/" + name, SHA256: hash(body), Size: int64(len(body)), Roles: []string{"adapter"}})
			if i == 0 {
				f.scriptPins = append(f.scriptPins, f.bytes(t, name, body))
			}
		}
		if dep.NodeID == "m02" {
			for _, name := range []string{"weed", "weed-volume"} {
				dep.Files = append(dep.Files, infra.DeploymentFile{Path: "/opt/seaweed/bin/" + name, SHA256: strings.Repeat("d", 64), Size: 1, Roles: []string{"product"}})
			}
		}
	}
	f.policy.Scripts = f.scriptPins
	f.binding.Artifacts = f.write(t, "artifacts.json", tr.ApprovedArtifacts{Version: "s0b-artifacts-v1", Deployments: f.policy.Deployments})
	d.ArtifactManifestSHA256 = f.binding.Artifacts.SHA256
	requested := *d
	requested.State = "requested"
	requested.Observation = nil
	requested.Hosts = append([]inventory.Host{}, d.Hosts...)
	for i := range requested.Hosts {
		requested.Hosts[i].RDMA.Status = "requested"
		requested.Hosts[i].RDMA.FabricEvidenceSHA256 = ""
	}
	raw, _ := json.Marshal(requested)
	resolved, err := inventory.Resolve(raw, *d)
	if err != nil {
		t.Fatal(err)
	}
	f.plan.Requested, f.plan.Resolved, f.plan.Profile = string(raw), string(resolved), "m01-m02-v1"
	f.plan.Deploy = f.plan.Deploy[:3]
	delete(f.policy.Jobs, "deploy-s3")
	for _, group := range []struct {
		steps  []Step
		action string
	}{{f.plan.Preflight, "preflight"}, {f.plan.Deploy, "deploy"}, {f.plan.Ready, "ready"}, {f.plan.Collect, "collect"}} {
		for _, step := range group.steps {
			files := map[string]string{}
			for _, dep := range f.policy.Deployments {
				if dep.NodeID == step.Host {
					for _, file := range dep.Files {
						files[file.Path] = file.SHA256
					}
				}
			}
			args, err := ProfileArgs(d, step.Host, group.action, step.Role, f.plan.Smoke, files)
			if err != nil {
				t.Fatal(err)
			}
			f.policy.Jobs[step.Job] = tr.JobPin{Node: step.Host, Executable: "/usr/bin/python3.12", SHA256: strings.Repeat("f", 64), Argv: args}
		}
	}
	smoke := f.policy.Jobs["smoke"]
	smoke.Executable = "/usr/bin/python3.12"
	f.policy.Jobs["smoke"] = smoke
	f.publish(t, nil)
	return f
}

func TestStrictEntryM01M02ProfileBindsHelpersAndArgs(t *testing.T) {
	f := cpuProfile(t)
	result, bundle, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, RegisterPack, &f.runtime, nil)
	if err != nil || result == nil || result.Status != tr.StatusPass {
		t.Fatalf("result=%+v err=%v bundle=%s", result, err, bundle)
	}
	if f.node.authorized != 10 || len(f.node.live) != 0 {
		t.Fatalf("jobs=%d live=%d", f.node.authorized, len(f.node.live))
	}
}

func TestStrictEntryM01M02ProfileRefusesDriftBeforeJobs(t *testing.T) {
	for _, which := range []string{"argv", "helper", "scan", "interpreter"} {
		t.Run(which, func(t *testing.T) {
			f := cpuProfile(t)
			switch which {
			case "argv":
				j := f.policy.Jobs["preflight-m01"]
				j.Argv[3] = strings.Replace(j.Argv[3], "mlx5_0", "mlx5_1", 1)
				f.policy.Jobs["preflight-m01"] = j
			case "helper":
				for i := range f.policy.Deployments[0].Files {
					p := &f.policy.Deployments[0].Files[i]
					if strings.HasSuffix(p.Path, "lab_profile.py") {
						p.SHA256 = strings.Repeat("a", 64)
					}
				}
				// Keep artifact/inventory authority internally coherent: the
				// compiled profile must refuse these different approved bytes.
				f.binding.Artifacts = f.write(t, "artifacts.json", tr.ApprovedArtifacts{Version: "s0b-artifacts-v1", Deployments: f.policy.Deployments})
				d, e := inventory.Decode([]byte(f.plan.Resolved))
				if e != nil {
					t.Fatal(e)
				}
				d.ArtifactManifestSHA256 = f.binding.Artifacts.SHA256
				var req inventory.Document
				if e = json.Unmarshal([]byte(f.plan.Requested), &req); e != nil {
					t.Fatal(e)
				}
				req.ArtifactManifestSHA256 = f.binding.Artifacts.SHA256
				raw, _ := json.Marshal(req)
				resolved, e := inventory.Resolve(raw, *d)
				if e != nil {
					t.Fatal(e)
				}
				f.plan.Requested, f.plan.Resolved = string(raw), string(resolved)
			case "scan":
				f.policy.Scripts = f.policy.Scripts[:3]
			case "interpreter":
				j := f.policy.Jobs["preflight-m01"]
				j.Executable = "/opt/seaweed/interpreter"
				f.policy.Jobs["preflight-m01"] = j
			}
			f.publish(t, nil)
			result, _, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, RegisterPack, &f.runtime, nil)
			if err == nil && result != nil && result.Status == tr.StatusPass {
				t.Fatal("profile drift passed")
			}
			if f.node.authorized != 0 {
				t.Fatalf("profile drift dispatched %d jobs", f.node.authorized)
			}
		})
	}
}

func TestStrictEntryM01M02ProfileRefusesEarlyFutureWindow(t *testing.T) {
	f := cpuProfile(t)
	d, e := inventory.Decode([]byte(f.plan.Resolved))
	if e != nil {
		t.Fatal(e)
	}
	f.binding.WorkDeadline = time.Now().Add(20 * time.Minute)
	f.binding.HardDeadline = f.binding.WorkDeadline.Add(time.Minute)
	d.Deadlines.Work = f.binding.WorkDeadline.UTC().Format(time.RFC3339Nano)
	d.Deadlines.Hard = f.binding.HardDeadline.UTC().Format(time.RFC3339Nano)
	var req inventory.Document
	if e = json.Unmarshal([]byte(f.plan.Requested), &req); e != nil {
		t.Fatal(e)
	}
	req.Deadlines = d.Deadlines
	raw, _ := json.Marshal(req)
	resolved, e := inventory.Resolve(raw, *d)
	if e != nil {
		t.Fatal(e)
	}
	f.plan.Requested, f.plan.Resolved = string(raw), string(resolved)
	for _, group := range []struct {
		steps  []Step
		action string
	}{{f.plan.Preflight, "preflight"}, {f.plan.Deploy, "deploy"}, {f.plan.Ready, "ready"}, {f.plan.Collect, "collect"}} {
		for _, step := range group.steps {
			files := map[string]string{}
			for _, dep := range f.policy.Deployments {
				if dep.NodeID == step.Host {
					for _, file := range dep.Files {
						files[file.Path] = file.SHA256
					}
				}
			}
			args, e := ProfileArgs(d, step.Host, group.action, step.Role, f.plan.Smoke, files)
			if e != nil {
				t.Fatal(e)
			}
			job := f.policy.Jobs[step.Job]
			job.Argv = args
			f.policy.Jobs[step.Job] = job
		}
	}
	f.publish(t, nil)
	result, _, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, RegisterPack, &f.runtime, nil)
	if err == nil && result != nil && result.Status == tr.StatusPass {
		t.Fatal("future window admitted more than ten minutes of remaining custody")
	}
	if f.node.authorized != 0 {
		t.Fatalf("early window dispatched %d jobs", f.node.authorized)
	}
}
