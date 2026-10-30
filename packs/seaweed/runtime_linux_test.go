//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/infra"
	"github.com/pingqiu/sw-test-runner/inventory"
	"gopkg.in/yaml.v3"
)

// Fake only at the transport boundary: no SSH, native import, host or network.
// The strict records, lease, GO, readback qualification and Engine are real.
type cpuNode struct {
	mu             sync.Mutex
	ids            map[string]tr.ProcessIdentity
	specs          map[string]tr.JobSpec
	boots          map[string]string
	live           map[string]bool
	cleanupStopped map[string]bool
	authorized     int
	unknown        bool
	lostReply      bool
	onAuthorize    func()
	dropOutput     bool
}

func (n *cpuNode) Enroll(_ context.Context, id string, a tr.RunAdmission, s tr.JobSpec) (tr.ProcessIdentity, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	i := tr.ProcessIdentity{Node: s.Node, BootID: n.boots[s.Node], RunID: a.RunID, ExecutableSHA256: s.SHA256, PID: 100 + len(n.ids), PGID: 100 + len(n.ids), StartTime: 42}
	n.ids[id] = i
	n.specs[id] = s
	n.live[id] = true
	if n.lostReply && len(n.ids) == 3 {
		return tr.ProcessIdentity{}, errors.New("CPU lost enrollment reply after POSSIBLE")
	}
	return i, nil
}
func (n *cpuNode) Authorize(_ context.Context, id string, i tr.ProcessIdentity) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ids[id] != i {
		return errors.New("foreign identity")
	}
	n.authorized++
	if n.onAuthorize != nil {
		n.onAuthorize()
	}
	return nil
}
func (n *cpuNode) Settle(_ context.Context, id string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.unknown {
		return errors.New("CPU UNKNOWN settlement")
	}
	if n.cleanupStopped == nil {
		n.cleanupStopped = map[string]bool{}
	}
	n.cleanupStopped[id] = n.live[id]
	delete(n.live, id)
	return nil
}
func (n *cpuNode) Wait(_ context.Context, id string) (tr.ExitInfo, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	s := n.specs[id]
	delete(n.live, id)
	exit := tr.ExitInfo{Identity: n.ids[id], Code: 0, Settled: true, StdoutPath: s.StdoutPath, StderrPath: s.StderrPath}
	if s.CaptureOutput && !n.dropOutput {
		exit.Output = tr.CapturedOutput{Complete: true, StdoutBase64: "e30K", StderrBase64: "", StdoutSHA256: hash([]byte("{}\n")), StderrSHA256: hash(nil), ReadStart: time.Now(), ReadEnd: time.Now()}
	}
	if n.cleanupStopped[id] {
		exit.Code, exit.Signal, exit.CleanupStopped = -1, 9, true
	}
	return exit, nil
}
func (n *cpuNode) Kill(context.Context, string) (tr.KillWitness, error) {
	return tr.KillWitness{}, errors.New("unexpected kill")
}

type cpuFixture struct {
	editScenario          func(*tr.Scenario)
	dir, request, profile string
	binding               tr.RunBinding
	policy                tr.ApprovedRunPolicy
	plan                  Plan
	runtime               tr.ApprovedRuntime
	node                  *cpuNode
	scriptPins            []tr.FilePin
}

func (f *cpuFixture) write(t *testing.T, name string, v any) tr.FilePin {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return f.bytes(t, name, raw)
}
func (f *cpuFixture) bytes(t *testing.T, name string, raw []byte) tr.FilePin {
	t.Helper()
	p := filepath.Join(f.dir, name)
	if err := os.WriteFile(p, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return tr.FilePin{Path: p, SHA256: hash(raw)}
}
func newCPUFixture(t *testing.T) *cpuFixture {
	t.Helper()
	home, _ := os.UserHomeDir()
	dir, err := os.MkdirTemp(home, ".codex02-pack-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &cpuFixture{dir: dir, node: &cpuNode{ids: map[string]tr.ProcessIdentity{}, specs: map[string]tr.JobSpec{}, boots: map[string]string{}, live: map[string]bool{}}}
	raw, err := os.ReadFile("../../examples/inventory/dc.resolved.json")
	if err != nil {
		t.Fatal(err)
	}
	d, err := inventory.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	d.RunID = "pack-cpu"
	d.Deadlines = inventory.Deadlines{Work: time.Now().Add(90 * time.Second).UTC().Format(time.RFC3339Nano), Hard: time.Now().Add(100 * time.Second).UTC().Format(time.RFC3339Nano), CleanupReserveMS: 5000}
	server := d.Hosts[0]
	server.ID = "m02"
	server.Roles = []string{"master", "filer", "volume", "s3"}
	server.RunRoot = "/run/pack-cpu"
	server.Management.Address = "192.0.2.2"
	server.Data.Address = "192.0.2.2"
	server.RDMA.GID = "00000000000000000000ffffc0000202"
	client := server
	client.ID = "m01"
	client.Roles = []string{"client"}
	client.Management.Address = "192.0.2.1"
	client.Data.Address = "192.0.2.1"
	client.RDMA.GID = "00000000000000000000ffffc0000201"
	client.Services = []inventory.Service{}
	client.Disks = []inventory.Disk{}
	d.Hosts = []inventory.Host{client, server}
	d.Observation.Hosts = []inventory.HostObservation{}
	f.policy = tr.ApprovedRunPolicy{Version: "s0b-run-policy-v1", Runner: "/opt/seaweed/runner", Jobs: map[string]tr.JobPin{}}
	hosts := []tr.ApprovedHost{}
	keys := map[string]string{}
	for _, h := range d.Hosts {
		boot := "11111111-1111-1111-1111-111111111111"
		f.node.boots[h.ID] = boot
		hosts = append(hosts, tr.ApprovedHost{Node: h.ID, Host: h.Management.Address + ":22", User: h.Management.User, HostKey: *h.Management.HostKey, Root: h.RunRoot})
		keys[h.ID] = "/service/private-key"
		d.Observation.Hosts = append(d.Observation.Hosts, inventory.HostObservation{HostID: h.ID, BootID: boot, CapturedAt: time.Now().UTC().Format(time.RFC3339Nano), EvidenceSHA256: strings.Repeat("c", 64)})
		files := []infra.DeploymentFile{}
		for i, role := range []string{"runner", "adapter", "config", "product", "native", "interpreter"} {
			path := "/opt/seaweed/" + role
			if role == "native" {
				path = "/opt/seaweed/native/seaweedkv_native/_native.so"
			}
			files = append(files, infra.DeploymentFile{Path: path, SHA256: strings.Repeat(string(rune('a'+i)), 64), Size: 1, Roles: []string{role}})
		}
		for _, name := range []string{"kv_smoke.py", "soak_native_worker.py", "soak_oracle.py"} {
			body, _ := workerFiles.ReadFile("worker/" + name)
			files = append(files, infra.DeploymentFile{Path: "/opt/seaweed/worker/" + name, SHA256: hash(body), Size: int64(len(body)), Roles: []string{"adapter"}})
			if h.ID == "m01" {
				f.scriptPins = append(f.scriptPins, f.bytes(t, name, body))
			}
		}
		f.policy.Deployments = append(f.policy.Deployments, infra.DeploymentPolicy{NodeID: h.ID, Host: h.Management.Address + ":22", User: h.Management.User, HostKey: *h.Management.HostKey, InstallRoot: "/opt/seaweed", ExpectedBootID: boot, WorkloadUID: 1000, RequireNative: true, RequireInterpreter: true, Files: files})
	}
	f.policy.Scripts = f.scriptPins
	artifacts := f.write(t, "artifacts.json", tr.ApprovedArtifacts{Version: "s0b-artifacts-v1", Deployments: f.policy.Deployments})
	d.ArtifactManifestSHA256 = artifacts.SHA256
	requested := *d
	requested.Hosts = append([]inventory.Host(nil), d.Hosts...)
	for i := range requested.Hosts {
		requested.Hosts[i].RDMA.Status = "requested"
		requested.Hosts[i].RDMA.FabricEvidenceSHA256 = ""
	}
	requested.State = "requested"
	requested.Observation = nil
	req, _ := json.Marshal(requested)
	resolved, err := inventory.Resolve(req, *d)
	if err != nil {
		t.Fatal(err)
	}
	f.plan = Plan{Version: "seaweed.pack-plan/v1", Requested: string(req), Resolved: string(resolved), Smoke: SmokeSettings{NativeDir: "/opt/seaweed/native", HelperDir: "/opt/seaweed/worker", Keys: 2, Length: 4096}}
	add := func(host, role, label string) Step {
		f.policy.Jobs[label] = tr.JobPin{Node: host, Executable: "/opt/seaweed/product", SHA256: strings.Repeat("d", 64), Argv: []string{label}}
		return Step{Host: host, Role: role, Job: label}
	}
	for _, h := range d.Hosts {
		f.plan.Preflight = append(f.plan.Preflight, add(h.ID, "", "preflight-"+h.ID))
		f.plan.Ready = append(f.plan.Ready, add(h.ID, "", "ready-"+h.ID))
		f.plan.Collect = append(f.plan.Collect, add(h.ID, "", "collect-"+h.ID))
		for _, role := range h.Roles {
			if role != "client" {
				f.plan.Deploy = append(f.plan.Deploy, add(h.ID, role, "deploy-"+role))
			}
		}
	}
	f.plan.Workload = []Step{add("m01", "client", "smoke")}
	args, err := SmokeArgs(d, "m01", f.plan.Smoke)
	if err != nil {
		t.Fatal(err)
	}
	f.policy.Jobs["smoke"] = tr.JobPin{Node: "m01", Executable: "/opt/seaweed/interpreter", SHA256: strings.Repeat("f", 64), Argv: args}
	for _, name := range []string{"running", "ledger", "results"} {
		if err = os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	lease := "0123456789abcdef0123456789abcdef"
	f.write(t, "ledger/"+lease+"-acquire.json", map[string]any{"lease_id": lease, "agent": "codex02", "host": "broker", "started": float64(time.Now().UnixNano())/1e9 - 1})
	work, _ := time.Parse(time.RFC3339Nano, d.Deadlines.Work)
	hard, _ := time.Parse(time.RFC3339Nano, d.Deadlines.Hard)
	f.binding = tr.RunBinding{RequestID: "pack-request", RunID: d.RunID, Workload: "kv", Transport: "rdma", Inventory: f.write(t, "inventory.json", tr.ApprovedInventory{Version: "s0b-inventory-v1", Hosts: hosts}), Artifacts: artifacts, Hosts: hosts, LeaseID: lease, LeaseOwner: "codex02", LeaseResource: "m01,m02", OutputDir: "/run/pack-cpu", WorkDeadline: work, HardDeadline: hard, CleanupReserveMS: 5000}
	f.runtime = tr.ApprovedRuntime{Jobs: f.node, Readback: func(_ context.Context, p infra.DeploymentPolicy) (infra.DeploymentReceipt, error) {
		r := infra.DeploymentReceipt{NodeID: p.NodeID, Host: p.Host, User: p.User, HostKey: p.HostKey, BootID: p.ExpectedBootID, WorkloadUID: p.WorkloadUID, ReadStart: time.Now(), ReadEnd: time.Now(), Files: map[string]string{}}
		b, _ := json.Marshal(p)
		r.PolicySHA256 = hash(b)
		for _, file := range p.Files {
			r.Files[file.Path] = file.SHA256
		}
		return r, nil
	}}
	f.publish(t, keys)
	return f
}
func (f *cpuFixture) publish(t *testing.T, keys map[string]string) {
	t.Helper()
	if keys == nil {
		keys = map[string]string{"m01": "/service/private-key", "m02": "/service/private-key"}
	}
	planRaw, err := MarshalPlan(f.plan)
	if err != nil {
		t.Fatal(err)
	}
	scenario := tr.Scenario{Name: "seaweed-cpu", Metadata: map[string]string{"seaweed_plan": string(planRaw)}}
	for i, name := range phases {
		scenario.Phases = append(scenario.Phases, tr.Phase{Name: name, Always: i == 4, Actions: []tr.Action{{Action: name}}})
	}
	if f.editScenario != nil {
		f.editScenario(&scenario)
	}
	raw, err := yaml.Marshal(scenario)
	if err != nil {
		t.Fatal(err)
	}
	f.binding.Scenario = f.bytes(t, "scenario.yaml", raw)
	f.binding.Policy = f.write(t, "policy.json", f.policy)
	f.request = f.write(t, "running/pack-request.env", tr.ClaimedRequest{Version: "s0b-request-v1", Binding: f.binding}).Path
	req, _ := os.ReadFile(f.request)
	goPin := f.write(t, "go.json", tr.ApprovedGO{Version: "s0b-go-v1", SourceID: "0123456789abcdef01234567", RequestSHA256: hash(req), Binding: f.binding, AllowMutating: true})
	f.profile = f.write(t, "profile.json", tr.PolicyBootstrap{Version: "s0b-bootstrap-v1", Approval: goPin, Policy: f.binding.Policy, RunningDir: filepath.Join(f.dir, "running"), LedgerDir: filepath.Join(f.dir, "ledger"), CancelFile: filepath.Join(f.dir, "cancel.json"), KeyFiles: keys, ResultsDir: filepath.Join(f.dir, "results")}).Path
}
func TestStrictEntryRunsAllFiveSeaweedPhases(t *testing.T) {
	f := newCPUFixture(t)
	result, bundle, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, RegisterPack, &f.runtime, nil)
	if err != nil || result == nil || result.Status != tr.StatusPass {
		t.Fatalf("result=%+v err=%v bundle=%s", result, err, bundle)
	}
	if len(result.Phases) != 5 || len(f.node.live) != 0 || f.node.authorized != 11 {
		t.Fatalf("phases=%d remaining=%d jobs=%d", len(result.Phases), len(f.node.live), f.node.authorized)
	}
	raw, err := os.ReadFile(filepath.Join(bundle, "artifacts", "authority.json"))
	if err != nil || !strings.Contains(string(raw), "pack-request") {
		t.Fatalf("authority evidence %s %v", raw, err)
	}
	var evidence struct {
		Jobs map[string]tr.ApprovedJobReceipt
	}
	if err := json.Unmarshal(raw, &evidence); err != nil {
		t.Fatal(err)
	}
	background := 0
	for _, receipt := range evidence.Jobs {
		if len(receipt.Spec.Argv) > 0 && strings.HasPrefix(receipt.Spec.Argv[0], "deploy-") {
			background++
			if receipt.State != "EXIT_SETTLED" || receipt.Exit == nil || !receipt.Exit.CleanupStopped || receipt.Exit.Code != -1 || receipt.Exit.Signal != 9 {
				t.Fatalf("background exit not retained: %+v", receipt)
			}
		}
	}
	if background != 4 {
		t.Fatalf("background receipts=%d", background)
	}
}

func TestStrictEntryMissingOwnedOutputCannotPass(t *testing.T) {
	f := newCPUFixture(t)
	f.node.dropOutput = true
	result, _, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, RegisterPack, &f.runtime, nil)
	if err == nil && result != nil && result.Status == tr.StatusPass {
		t.Fatal("missing hardware/collector export still passed actual strict entry")
	}
}

func TestStrictEntryRefusesForeignPackBindingsBeforeJobs(t *testing.T) {
	for _, mode := range []string{"foreign-go", "released-lease", "stale-lease", "deadline", "artifact", "empty-workload", "missing-host-phase", "wrong-workload-selector", "helper-not-scanned", "reordered"} {
		t.Run(mode, func(t *testing.T) {
			f := newCPUFixture(t)
			switch mode {
			case "foreign-go":
				f.binding.Transport = "http"
				f.write(t, "running/pack-request.env", tr.ClaimedRequest{Version: "s0b-request-v1", Binding: f.binding})
			case "released-lease":
				f.bytes(t, "ledger/"+f.binding.LeaseID+"-release.json", []byte("{}"))
			case "stale-lease":
				f.write(t, "ledger/"+f.binding.LeaseID+"-acquire.json", map[string]any{"lease_id": f.binding.LeaseID, "agent": "codex02", "host": "broker", "started": float64(time.Now().Unix() - 601)})
			case "deadline", "artifact":
				var req, obs inventory.Document
				json.Unmarshal([]byte(f.plan.Requested), &req)
				json.Unmarshal([]byte(f.plan.Resolved), &obs)
				if mode == "deadline" {
					req.Deadlines.Work = f.binding.WorkDeadline.Add(-time.Second).UTC().Format(time.RFC3339Nano)
					obs.Deadlines = req.Deadlines
				} else {
					req.ArtifactManifestSHA256 = strings.Repeat("1", 64)
					obs.ArtifactManifestSHA256 = req.ArtifactManifestSHA256
				}
				raw, _ := json.Marshal(req)
				out, err := inventory.Resolve(raw, obs)
				if err != nil {
					t.Fatal(err)
				}
				f.plan.Requested = string(raw)
				f.plan.Resolved = string(out)
				f.publish(t, nil)
			case "empty-workload":
				f.plan.Workload = nil
				f.publish(t, nil)
			case "missing-host-phase":
				f.plan.Collect = f.plan.Collect[:1]
				f.publish(t, nil)
			case "wrong-workload-selector":
				j := f.policy.Jobs["smoke"]
				j.Argv = append([]string(nil), j.Argv...)
				j.Argv[len(j.Argv)-1] = strings.Replace(j.Argv[len(j.Argv)-1], `"transport":"dc"`, `"transport":"http"`, 1)
				f.policy.Jobs["smoke"] = j
				f.publish(t, nil)
			case "helper-not-scanned":
				f.policy.Scripts = f.policy.Scripts[:1]
				f.publish(t, nil)
			case "reordered":
				f.plan.Preflight, f.plan.Ready = f.plan.Ready, f.plan.Preflight
				f.publish(t, nil)
			}
			// A differently approved preflight/readiness program is not identifiable by
			// label text alone: independent policy owns those program semantics. Treat
			// their exchange as a valid authority change, not a fabricated refusal.
			wantFail := mode != "reordered"
			result, _, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, RegisterPack, &f.runtime, nil)
			failed := err != nil || result == nil || result.Status != tr.StatusPass
			if failed != wantFail || wantFail && f.node.authorized != 0 {
				t.Fatalf("mode=%s result=%+v err=%v authorized=%d", mode, result, err, f.node.authorized)
			}
		})
	}
}

func TestStrictEntryRetainsCleanupUnknown(t *testing.T) {
	f := newCPUFixture(t)
	f.node.unknown = true
	result, _, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, RegisterPack, &f.runtime, nil)
	if err != nil || result == nil || result.Status != tr.StatusFail || !strings.Contains(result.Error, "UNKNOWN") || len(f.node.live) != 4 {
		t.Fatalf("cleanup failure lost: %+v err=%v live=%d", result, err, len(f.node.live))
	}
	last := result.Phases[len(result.Phases)-1]
	if last.Status != tr.StatusFail || len(last.Actions) != 1 || !strings.Contains(last.Actions[0].Output, "CLEANUP_UNPROVEN") {
		t.Fatalf("cleanup JSON failure lost: %+v", last)
	}
}

func TestLeaseLossBetweenPhasesNeverAuthorizesNextJob(t *testing.T) {
	f := newCPUFixture(t)
	f.node.onAuthorize = func() {
		if f.node.authorized == 2 {
			if err := os.WriteFile(filepath.Join(f.dir, "ledger", f.binding.LeaseID+"-release.json"), []byte("{}"), 0600); err != nil {
				panic(err)
			}
		}
	}
	result, _, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, RegisterPack, &f.runtime, nil)
	if err != nil || result == nil || result.Status != tr.StatusFail || f.node.authorized != 2 || len(f.node.live) != 0 {
		t.Fatalf("lease bypass: %+v %v jobs=%d live=%d", result, err, f.node.authorized, len(f.node.live))
	}
}

func TestCallerCreatedScopeIsNotApprovedPackAuthority(t *testing.T) {
	f := newCPUFixture(t)
	scope, err := tr.NewRunScope(context.Background(), tr.RunAdmission{RunID: "fake", Workload: "kv", RequestID: "r", ManifestSHA256: strings.Repeat("a", 64), OutputDir: "/run/pack-cpu", WorkDeadline: time.Now().Add(time.Minute), HardDeadline: time.Now().Add(2 * time.Minute), CleanupReserve: time.Second}, f.node, func(context.Context, tr.RunAdmission) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	r := tr.NewRegistry()
	r.AllowMutating = true
	RegisterPack(r)
	for _, name := range phases {
		handler, _ := r.Get(name)
		out, err := handler.Execute(context.Background(), &tr.ActionContext{Safety: scope, Vars: map[string]string{"s0b_admitted": "true"}}, tr.Action{Action: name})
		if !errors.Is(err, ErrS0bUnavailable) || f.node.authorized != 0 {
			t.Fatalf("caller scope bypass: %s %+v %v", name, out, err)
		}
	}
}

func TestUnknownEnrollmentStillSettlesPossibleIssuer(t *testing.T) {
	f := newCPUFixture(t)
	f.node.lostReply = true
	result, _, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, RegisterPack, &f.runtime, nil)
	if err != nil || result == nil || result.Status != tr.StatusFail || f.node.authorized != 2 || len(f.node.live) != 0 || !strings.Contains(result.Error, "UNKNOWN") {
		t.Fatalf("POSSIBLE lost: result=%+v err=%v authorized=%d live=%d", result, err, f.node.authorized, len(f.node.live))
	}
}

func TestScenarioCannotSkipSmokeOrIgnorePackRefusal(t *testing.T) {
	for _, mode := range []string{"cleanup-only", "ignored-errors", "missing-workload", "reordered-actions"} {
		t.Run(mode, func(t *testing.T) {
			f := newCPUFixture(t)
			f.editScenario = func(s *tr.Scenario) {
				switch mode {
				case "cleanup-only":
					s.Phases = s.Phases[4:]
				case "missing-workload":
					s.Phases = append(s.Phases[:2], s.Phases[3:]...)
				case "reordered-actions":
					s.Phases[0].Actions[0].Action = "seaweed_deploy"
				case "ignored-errors":
					for i := range s.Phases {
						s.Phases[i].Always = false
						s.Phases[i].Actions[0].IgnoreError = true
					}
				}
			}
			f.publish(t, nil)
			result, _, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, RegisterPack, &f.runtime, nil)
			if err != nil || result == nil || result.Status != tr.StatusFail || f.node.authorized != 0 {
				t.Fatalf("incomplete/ignored qualification: result=%+v err=%v jobs=%d", result, err, f.node.authorized)
			}
		})
	}
}
