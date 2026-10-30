//go:build linux

package smoke

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/infra"
	"github.com/pingqiu/sw-test-runner/inventory"
	"github.com/pingqiu/sw-test-runner/packs/seaweed"
)

type freshJobs struct {
	n       int
	specs   map[string]tr.JobSpec
	ids     map[string]tr.ProcessIdentity
	stopped map[string]bool
	boot    string
}

func (j *freshJobs) Enroll(_ context.Context, id string, a tr.RunAdmission, s tr.JobSpec) (tr.ProcessIdentity, error) {
	i := tr.ProcessIdentity{Node: s.Node, BootID: j.boot, RunID: a.RunID, ExecutableSHA256: s.SHA256, PID: 100 + len(j.ids), PGID: 100 + len(j.ids), StartTime: 42}
	j.ids[id] = i
	j.specs[id] = s
	return i, nil
}
func (j *freshJobs) Authorize(context.Context, string, tr.ProcessIdentity) error { j.n++; return nil }
func (j *freshJobs) Settle(_ context.Context, id string) error                   { j.stopped[id] = true; return nil }
func (j *freshJobs) Wait(_ context.Context, id string) (tr.ExitInfo, error) {
	e := tr.ExitInfo{Identity: j.ids[id], Settled: true, StdoutPath: j.specs[id].StdoutPath, StderrPath: j.specs[id].StderrPath}
	if j.stopped[id] {
		e.CleanupStopped = true
		e.Signal = 9
		e.Code = -1
	}
	return e, nil
}
func (j *freshJobs) Kill(context.Context, string) (tr.KillWitness, error) {
	return tr.KillWitness{}, errors.New("unexpected kill")
}

func freshFixture(t *testing.T) (Config, *freshAuthority, []byte, *freshJobs) {
	t.Helper()
	c := fixture(t)
	dir := filepath.Dir(c.Output)
	for _, n := range []string{"running", "ledger", "results", "docs"} {
		if e := os.Mkdir(filepath.Join(dir, n), 0700); e != nil {
			t.Fatal(e)
		}
	}
	c.Request = filepath.Join(dir, "running", "fresh-request.env")
	c.Policy = filepath.Join(dir, "docs", "bootstrap.json")
	var decl declaration
	b, _ := os.ReadFile(c.Variables)
	json.Unmarshal(b, &decl)
	b, e := os.ReadFile("../../examples/inventory/http.requested.json")
	if e != nil {
		t.Fatal(e)
	}
	d, e := inventory.Decode(b)
	if e != nil {
		t.Fatal(e)
	}
	d.Target = "azure"
	d.RunID = "fresh-run"
	d.Deadlines = inventory.Deadlines{Work: decl.Run.Work.Format(time.RFC3339Nano), Hard: decl.Run.Hard.Format(time.RFC3339Nano), CleanupReserveMS: 5000}
	h := &d.Hosts[0]
	h.RunRoot = "/data/fresh-run"

	h.ResourceID = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/dedicated-test/providers/Microsoft.Compute/virtualMachines/fresh-node1"
	diskID := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/dedicated-test/providers/Microsoft.Compute/disks/fresh-cache"
	d.ManagedResourceIDs = []string{h.ResourceID, diskID}
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEB"
	host := tr.ApprovedHost{Node: h.ID, Host: "192.0.2.10:22", User: h.Management.User, HostKey: key, Root: h.RunRoot}
	files := []infra.DeploymentFile{}
	for i, role := range []string{"runner", "adapter", "config", "product", "native", "interpreter"} {
		p := "/opt/seaweed/" + role
		if role == "native" {
			p = "/opt/seaweed/native/seaweedkv_native/_native.so"
		}
		files = append(files, infra.DeploymentFile{Path: p, SHA256: strings.Repeat(fmt.Sprint(i+1), 64), Size: 1, Roles: []string{role}})
	}
	policy := tr.ApprovedRunPolicy{Version: "s0b-run-policy-v1", Runner: "/opt/seaweed/runner", Jobs: map[string]tr.JobPin{}, Deployments: []infra.DeploymentPolicy{{NodeID: h.ID, Host: host.Host, User: host.User, HostKey: key, InstallRoot: "/opt/seaweed", WorkloadUID: 2001, RequireNative: true, RequireInterpreter: true, Files: files}}}
	for _, name := range []string{"kv_smoke.py", "soak_native_worker.py", "soak_oracle.py"} {
		b, e := os.ReadFile("../../packs/seaweed/worker/" + name)
		if e != nil {
			t.Fatal(e)
		}
		p := filepath.Join(dir, "docs", name)
		os.WriteFile(p, b, 0600)
		policy.Scripts = append(policy.Scripts, tr.FilePin{Path: p, SHA256: digest(b)})
		policy.Deployments[0].Files = append(policy.Deployments[0].Files, infra.DeploymentFile{Path: "/opt/seaweed/worker/" + name, SHA256: digest(b), Size: int64(len(b)), Roles: []string{"adapter"}})
	}
	payload := seaweed.PayloadManifest{Version: "seaweed.payload/v1"}
	for _, f := range files {
		if f.Roles[0] == "product" || f.Roles[0] == "native" {
			payload.Files = append(payload.Files, seaweed.PayloadFile{Node: h.ID, Path: f.Path, SHA256: f.SHA256})
		}
	}
	pb, _ := json.Marshal(payload)
	d.ArtifactManifestSHA256 = digest(pb)
	plan := seaweed.Plan{Version: "seaweed.pack-plan/v1", Payload: string(pb), Smoke: seaweed.SmokeSettings{NativeDir: "/opt/seaweed/native", HelperDir: "/opt/seaweed/worker", Keys: 2, Length: 4096}}
	add := func(role, label string) seaweed.Step {
		policy.Jobs[label] = tr.JobPin{Node: h.ID, Executable: "/opt/seaweed/product", SHA256: strings.Repeat("4", 64), Argv: []string{label}}
		return seaweed.Step{Host: h.ID, Role: role, Job: label}
	}
	plan.Preflight = []seaweed.Step{add("", "probe")}
	plan.Ready = []seaweed.Step{add("", "ready")}
	plan.Collect = []seaweed.Step{add("", "collect")}
	for _, r := range []string{"master", "filer", "volume", "s3"} {
		plan.Deploy = append(plan.Deploy, add(r, r))
	}
	plan.Workload = []seaweed.Step{add("client", "smoke")}
	config := seaweed.SmokeConfig{RunID: d.RunID, Client: h.ID, Transport: "http", FilerGRPC: h.Data.Address + ":18888", FilerHTTP: "http://" + h.Data.Address + ":8888", Root: "/testops/" + d.RunID + "/" + h.ID, NativeDir: plan.Smoke.NativeDir, HelperDir: plan.Smoke.HelperDir, Keys: 2, Length: 4096}
	sb, _ := json.Marshal(config)
	policy.Jobs["smoke"] = tr.JobPin{Node: h.ID, Executable: "/opt/seaweed/interpreter", SHA256: strings.Repeat("6", 64), Argv: []string{"-I", "/opt/seaweed/worker/kv_smoke.py", "--config", string(sb)}}
	s := tr.Scenario{Name: "fresh-cpu", Metadata: map[string]string{"seaweed_plan": ""}}
	for i, n := range []string{"seaweed_preflight", "seaweed_deploy", "seaweed_workload", "seaweed_collect", "seaweed_cleanup"} {
		s.Phases = append(s.Phases, tr.Phase{Name: n, Always: i == 4, Actions: []tr.Action{{Action: n}}})
	}
	pin := func(name string) tr.FilePin { return tr.FilePin{Path: filepath.Join(dir, "docs", name)} }
	lease := "0123456789abcdef0123456789abcdef"
	binding := tr.RunBinding{RequestID: "fresh-request", RunID: d.RunID, Workload: "kv", Transport: "http", Inventory: pin("inventory.json"), Scenario: pin("scenario.yaml"), Policy: pin("policy.json"), Artifacts: pin("artifacts.json"), Hosts: []tr.ApprovedHost{host}, LeaseID: lease, LeaseOwner: "codex02", LeaseResource: "azure-dedicated-test", OutputDir: h.RunRoot, WorkDeadline: decl.Run.Work, HardDeadline: decl.Run.Hard, CleanupReserveMS: 5000}
	bootstrap := tr.PolicyBootstrap{Version: "s0b-bootstrap-v1", Approval: pin("go.json"), Policy: binding.Policy, RunningDir: filepath.Join(dir, "running"), LedgerDir: filepath.Join(dir, "ledger"), CancelFile: filepath.Join(dir, "cancel"), KeyFiles: map[string]string{h.ID: "/root/private-key"}, ResultsDir: filepath.Join(dir, "results")}
	template := freshTemplate{Version: "seaweed.fresh-template/v1", Config: c, Requested: *d, MinMTU: 1500, MaxMTU: 1500, Disks: []diskSlot{{ID: "cache", ResourceID: diskID, LUN: 0}}, Binding: binding, Policy: policy, Plan: plan, Scenario: s, GO: tr.ApprovedGO{Version: "s0b-go-v1", SourceID: "0123456789abcdef01234567", Binding: binding, AllowMutating: true}, Bootstrap: bootstrap}
	j := &freshJobs{specs: map[string]tr.JobSpec{}, ids: map[string]tr.ProcessIdentity{}, stopped: map[string]bool{}, boot: "11111111-1111-1111-1111-111111111111"}
	a := &freshAuthority{template: template}
	approvedBytes, _ := json.Marshal(template)
	a.verify = func() error {
		current, _ := json.Marshal(a.template)
		if digest(current) != digest(approvedBytes) {
			return errors.New("approved template changed")
		}
		return nil
	}
	a.setup = func(_ context.Context, _ Config, raw []byte) ([]byte, error) {
		f := freshFacts{ExportSHA256: digest(raw), Boot: j.boot, BootAfter: j.boot, DataAddress: h.Data.Address, NIC: "eth0", MTU: 1500, Disks: []diskFact{{ID: "cache", ResourceID: diskID, LUN: 0, DeviceID: "/dev/disk/by-id/fresh-cache"}}, Start: time.Now(), End: time.Now()}
		return json.Marshal(f)
	}
	a.readback = func(_ context.Context, p infra.DeploymentPolicy, _ string) (infra.DeploymentReceipt, error) {
		r := infra.DeploymentReceipt{NodeID: p.NodeID, Host: p.Host, HostKey: p.HostKey, User: p.User, BootID: p.ExpectedBootID, WorkloadUID: p.WorkloadUID, Files: map[string]string{}, ReadStart: time.Now(), ReadEnd: time.Now()}
		b, _ := json.Marshal(p)
		r.PolicySHA256 = digest(b)
		for _, f := range p.Files {
			r.Files[f.Path] = f.SHA256
		}
		return r, nil
	}
	leaseRaw, _ := json.Marshal(map[string]any{"lease_id": lease, "agent": "codex02", "host": "broker", "started": float64(time.Now().UnixNano())/1e9 - 1})
	os.WriteFile(filepath.Join(bootstrap.LedgerDir, lease+"-acquire.json"), leaseRaw, 0600)
	raw, _ := json.Marshal(d)
	return c, a, raw, j
}

func TestFreshControllerMaterializesIntoUnchangedStrictEntry(t *testing.T) {
	c, a, raw, j := freshFixture(t)
	f := &fake{}
	command := func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
		b, e := f.command(ctx, dir, tool, args...)
		if tool == "terraform" && args[0] == "output" {
			return raw, e
		}
		return b, e
	}
	pack := func(ctx context.Context, request, policy string, requested []byte) (string, error) {
		if string(requested) != string(raw) {
			return "", errors.New("requested bytes changed")
		}
		runtime := tr.ApprovedRuntime{Jobs: j, Readback: func(ctx context.Context, p infra.DeploymentPolicy) (infra.DeploymentReceipt, error) {
			return a.readback(ctx, p, "")
		}}
		r, b, e := tr.RunApprovedRequest(ctx, request, policy, seaweed.RegisterPack, &runtime, nil)
		if e == nil && (r == nil || r.Status != tr.StatusPass) {
			return b, fmt.Errorf("strict pack failed: %+v", r)
		}
		return b, e
	}
	r := (Controller{Command: command, Pack: pack, fresh: a}).Execute(context.Background(), "run", c)
	if r.Code != "COMPLETE" || j.n != 8 {
		t.Fatalf("%+v jobs=%d", r, j.n)
	}
	for _, name := range []string{"derivation.json", "observation.json"} {
		if _, e := os.Stat(filepath.Join(c.Output, name)); e != nil {
			t.Fatal(e)
		}
	}
	if e := a.publish(context.Background(), c, raw); e == nil {
		t.Fatal("second materialization allowed")
	}
}

func TestFreshRefusalsCrossControllerAndStrictEntry(t *testing.T) {
	for _, mode := range []string{"role", "argv", "file", "uid", "deadline", "lease", "port", "budget", "key", "resource", "node", "ip-provenance", "probe-extra", "capture", "cancel", "cancel-context", "setup-failed", "boot", "readback-file", "shadow", "disk", "reserialized", "go", "bootstrap", "request"} {
		t.Run(mode, func(t *testing.T) {
			c, a, raw, j := freshFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := &fake{}
			// Independent approval is fixed before any candidate mutation.
			switch mode {
			case "role":
				a.template.Requested.Hosts[0].Roles = []string{"client"}
			case "argv":
				p := a.template.Policy.Jobs["master"]
				p.Argv = append(p.Argv, "unlisted")
				a.template.Policy.Jobs["master"] = p
			case "file":
				a.template.Policy.Deployments[0].Files[0].SHA256 = strings.Repeat("f", 64)
			case "uid":
				a.template.Policy.Deployments[0].WorkloadUID++
			case "deadline":
				a.template.Binding.WorkDeadline = a.template.Binding.WorkDeadline.Add(time.Minute)
			case "lease":
				a.template.Binding.LeaseID = strings.Repeat("2", 32)
			case "port":
				a.template.Requested.Hosts[0].Services[0].Port++
			case "budget":
				a.template.Config.EstimatedUSD++
			}
			var doc inventory.Document
			_ = json.Unmarshal(raw, &doc)
			if mode == "resource" {
				doc.ManagedResourceIDs = append(doc.ManagedResourceIDs, doc.Hosts[0].ResourceID+"-foreign")
				raw, _ = json.Marshal(doc)
			}
			if mode == "node" {
				doc.Hosts[0].ID = "foreign"
				raw, _ = json.Marshal(doc)
			}
			if mode == "reserialized" {
				raw = append(raw, '\n')
			}
			setup := a.setup
			a.setup = func(ctx context.Context, c Config, raw []byte) ([]byte, error) {
				b, e := setup(ctx, c, raw)
				var v freshFacts
				_ = json.Unmarshal(b, &v)
				switch mode {
				case "ip-provenance":
					v.ExportSHA256 = strings.Repeat("f", 64)
				case "probe-extra":
					var m map[string]any
					json.Unmarshal(b, &m)
					m["management_ip"] = "198.51.100.9"
					return json.Marshal(m)
				case "capture":
					v.End = time.Now().Add(time.Hour)
				case "cancel":
					os.WriteFile(a.template.Bootstrap.CancelFile, []byte("cancel"), 0600)
				case "cancel-context":
					cancel()
				case "setup-failed":
					return b, errors.New("failed probe with positive stdout")
				case "disk":
					v.Disks[0].ResourceID += "-foreign"
				case "reserialized":
					v.ExportSHA256 = digest([]byte(strings.TrimSpace(string(raw))))
				}
				if e != nil {
					return nil, e
				}
				return json.Marshal(v)
			}
			read := a.readback
			a.readback = func(ctx context.Context, p infra.DeploymentPolicy, k string) (infra.DeploymentReceipt, error) {
				r, e := read(ctx, p, k)
				switch mode {
				case "key":
					r.HostKey = "foreign"
				case "boot":
					r.BootID = "22222222-2222-2222-2222-222222222222"
				case "readback-file":
					r.Files[p.Files[0].Path] = strings.Repeat("f", 64)
				case "shadow":
					r.Files["/opt/seaweed/worker/soak_oracle/__init__.py"] = strings.Repeat("f", 64)
				}
				return r, e
			}
			command := func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
				b, e := f.command(ctx, dir, tool, args...)
				if tool == "terraform" && args[0] == "output" {
					return raw, e
				}
				return b, e
			}
			pack := func(ctx context.Context, request, policy string, _ []byte) (string, error) {
				if mode == "go" {
					os.WriteFile(a.template.Bootstrap.Approval.Path, []byte(`{}`), 0600)
				}
				if mode == "bootstrap" {
					os.WriteFile(c.Policy, []byte(`{}`), 0600)
				}
				if mode == "request" {
					os.WriteFile(c.Request, []byte(`{}`), 0600)
				}
				runtime := tr.ApprovedRuntime{Jobs: j, Readback: func(ctx context.Context, p infra.DeploymentPolicy) (infra.DeploymentReceipt, error) {
					return read(ctx, p, "")
				}}
				r, b, e := tr.RunApprovedRequest(ctx, request, policy, seaweed.RegisterPack, &runtime, nil)
				if e == nil && (r == nil || r.Status != tr.StatusPass) {
					return b, errors.New("pack FAIL")
				}
				return b, e
			}
			r := (Controller{Command: command, Pack: pack, fresh: a}).Execute(ctx, "run", c)
			if r.Code == "COMPLETE" || j.n != 0 {
				t.Fatalf("mode=%s result=%+v jobs=%d", mode, r, j.n)
			}
			if len(f.events) > 0 && (len(f.events) < 2 || strings.Join(f.events[len(f.events)-2:], ",") != "terraform destroy,az resource") {
				t.Fatalf("cleanup absent: %v", f.events)
			}
		})
	}
}

func TestFreshProtectedPublicationDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "pin.json")
	if e := publishFresh(name, []byte("approved")); e != nil {
		t.Fatal(e)
	}
	if e := publishFresh(name, []byte("replacement")); e == nil {
		t.Fatal("publication replaced existing bytes")
	}
	b, e := protectedFresh(name)
	if e != nil || string(b) != "approved" {
		t.Fatalf("read %q %v", b, e)
	}
	link := filepath.Join(dir, "link")
	if e := os.Symlink(name, link); e != nil {
		t.Fatal(e)
	}
	if _, e := protectedFresh(link); e == nil {
		t.Fatal("symlink accepted")
	}
	if e := os.Chmod(name, 0666); e != nil {
		t.Fatal(e)
	}
	if _, e := protectedFresh(name); e == nil {
		t.Fatal("writable authority accepted")
	}
}

func TestFreshTemplateCanonicalAndPrivateTranscripts(t *testing.T) {
	c, a, raw, _ := freshFixture(t)
	b, _ := json.Marshal(a.template)
	var out freshTemplate
	if e := strictFreshTemplate(b, &out); e != nil {
		t.Fatal(e)
	}
	for _, bad := range []string{string(b) + " ", strings.Replace(string(b), `"WorkloadUID"`, `"workloaduid"`, 1), strings.Replace(string(b), `"version":`, `"version":"x","version":`, 1)} {
		if strictFreshTemplate([]byte(bad), &out) == nil {
			t.Fatal("noncanonical template accepted")
		}
	}
	f := &fake{}
	command := func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
		b, e := f.command(ctx, dir, tool, args...)
		if tool == "terraform" && args[0] == "output" {
			return raw, e
		}
		if tool == "terraform" {
			return []byte("HOST_PRIVATE_SENTINEL"), e
		}
		return b, e
	}
	r := (Controller{Command: command, Pack: f.pack, fresh: a}).Execute(context.Background(), "run", c)
	// The deliberately different fake pack refuses; teardown still runs.
	if r.Code == "COMPLETE" {
		t.Fatal("fake pack unexpectedly passed")
	}
	paths, _ := filepath.Glob(filepath.Join(c.Output, "*-output"))
	for _, p := range paths {
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), "HOST_PRIVATE_SENTINEL") {
			t.Fatal("private terraform stdout leaked")
		}
	}
}
