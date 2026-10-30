//go:build linux

package seaweed

// Regression witnesses supplied with codex01's a16669af review. The background
// witness additionally requires every terminal exit, even on a failed run.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/infra"
	"github.com/pingqiu/sw-test-runner/inventory"
)

func TestReviewerNoClientCannotQualifyKVSmoke(t *testing.T) {
	f := newCPUFixture(t)
	// Keep the declared service host, but independently approve no client at all.
	var observed inventory.Document
	if err := json.Unmarshal([]byte(f.plan.Resolved), &observed); err != nil {
		t.Fatal(err)
	}
	observed.Hosts = observed.Hosts[1:]
	observed.Observation.Hosts = observed.Observation.Hosts[1:]
	f.binding.Hosts = f.binding.Hosts[1:]
	f.binding.Inventory = f.write(t, "inventory.json", tr.ApprovedInventory{Version: "s0b-inventory-v1", Hosts: f.binding.Hosts})
	f.policy.Deployments = f.policy.Deployments[1:]
	f.binding.Artifacts = f.write(t, "artifacts.json", tr.ApprovedArtifacts{Version: "s0b-artifacts-v1", Deployments: f.policy.Deployments})
	observed.ArtifactManifestSHA256 = f.binding.Artifacts.SHA256
	requested := observed
	requested.Hosts = append([]inventory.Host(nil), observed.Hosts...)
	requested.State, requested.Observation = "requested", nil
	for i := range requested.Hosts {
		requested.Hosts[i].RDMA.Status = "requested"
		requested.Hosts[i].RDMA.FabricEvidenceSHA256 = ""
	}
	raw, err := json.Marshal(requested)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := inventory.Resolve(raw, observed)
	if err != nil {
		t.Fatal(err)
	}
	f.plan.Requested, f.plan.Resolved = string(raw), string(resolved)
	keep := func(in []Step) []Step {
		var out []Step
		for _, s := range in {
			if s.Host == "m02" {
				out = append(out, s)
			}
		}
		return out
	}
	f.plan.Preflight, f.plan.Ready, f.plan.Collect = keep(f.plan.Preflight), keep(f.plan.Ready), keep(f.plan.Collect)
	f.plan.Workload = nil
	for label, job := range f.policy.Jobs {
		if job.Node != "m02" {
			delete(f.policy.Jobs, label)
		}
	}
	f.publish(t, map[string]string{"m02": "/service/private-key"})
	result, _, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, RegisterPack, &f.runtime, nil)
	t.Logf("no client, no workload: result=%+v err=%v authorized=%d", result, err, f.node.authorized)
	if err == nil && result != nil && result.Status == tr.StatusPass {
		t.Fatal("KV qualification PASS without any client, SET, GET or transport witness")
	}
	if f.node.authorized != 0 {
		t.Fatalf("missing workload should refuse before effects, got %d authorizations", f.node.authorized)
	}
}

type reviewerSlowPhase struct {
	*cpuNode
	mu2   sync.Mutex
	first time.Time
	late  int
}

func (n *reviewerSlowPhase) Enroll(ctx context.Context, id string, a tr.RunAdmission, s tr.JobSpec) (tr.ProcessIdentity, error) {
	if len(s.Argv) > 0 && strings.HasPrefix(s.Argv[0], "deploy-") {
		n.mu2.Lock()
		first := n.first.IsZero()
		if first {
			n.first = time.Now()
		}
		n.mu2.Unlock()
		if first {
			time.Sleep(150 * time.Millisecond)
		}
	}
	return n.cpuNode.Enroll(ctx, id, a, s)
}
func (n *reviewerSlowPhase) Authorize(ctx context.Context, id string, i tr.ProcessIdentity) error {
	n.cpuNode.mu.Lock()
	spec := n.cpuNode.specs[id]
	n.cpuNode.mu.Unlock()
	n.mu2.Lock()
	if len(spec.Argv) > 0 && strings.HasPrefix(spec.Argv[0], "deploy-") && time.Since(n.first) > 50*time.Millisecond {
		n.late++
	}
	n.mu2.Unlock()
	return n.cpuNode.Authorize(ctx, id, i)
}
func TestReviewerPhaseDeadlinePreventsLateServiceAuthorization(t *testing.T) {
	f := newCPUFixture(t)
	f.editScenario = func(s *tr.Scenario) { s.Phases[1].Actions[0].Timeout = "50ms" }
	f.publish(t, nil)
	backend := &reviewerSlowPhase{cpuNode: f.node}
	f.runtime.Jobs = backend
	result, _, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, RegisterPack, &f.runtime, nil)
	backend.mu2.Lock()
	late := backend.late
	backend.mu2.Unlock()
	t.Logf("50ms deploy action, 150ms enrollment: late_service_authorizations=%d result=%+v err=%v", late, result, err)
	if late != 0 {
		t.Fatalf("phase timeout allowed %d later service authorizations", late)
	}
}

func TestReviewerCompetingHelperPackageMustBeRefused(t *testing.T) {
	f := newCPUFixture(t)
	body := []byte("ORIGIN = 'competing package, not the pinned source'\n")
	f.policy.Deployments[0].Files = append(f.policy.Deployments[0].Files, infra.DeploymentFile{Path: "/opt/seaweed/worker/soak_oracle/__init__.py", SHA256: hash(body), Size: int64(len(body)), Roles: []string{"adapter"}})
	f.binding.Artifacts = f.write(t, "artifacts.json", tr.ApprovedArtifacts{Version: "s0b-artifacts-v1", Deployments: f.policy.Deployments})
	var req, obs inventory.Document
	if err := json.Unmarshal([]byte(f.plan.Requested), &req); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(f.plan.Resolved), &obs); err != nil {
		t.Fatal(err)
	}
	req.ArtifactManifestSHA256, obs.ArtifactManifestSHA256 = f.binding.Artifacts.SHA256, f.binding.Artifacts.SHA256
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := inventory.Resolve(raw, obs)
	if err != nil {
		t.Fatal(err)
	}
	f.plan.Requested, f.plan.Resolved = string(raw), string(resolved)
	f.publish(t, nil)
	result, _, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, RegisterPack, &f.runtime, nil)
	t.Logf("protected competing unscanned helper package: status=%v err=%v", result, err)
	if err == nil && result != nil && result.Status == tr.StatusPass {
		t.Fatal("pack passed with a competing import package despite exact .py hash claims")
	}
}

type reviewerBackgroundExit struct {
	*cpuNode
	waited int
}

func (n *reviewerBackgroundExit) Wait(ctx context.Context, id string) (tr.ExitInfo, error) {
	n.cpuNode.mu.Lock()
	spec := n.cpuNode.specs[id]
	n.cpuNode.mu.Unlock()
	info, err := n.cpuNode.Wait(ctx, id)
	if len(spec.Argv) > 0 && strings.HasPrefix(spec.Argv[0], "deploy-") {
		n.waited++
		info.Code = 9
		info.Signal, info.CleanupStopped = 0, false
	}
	return info, err
}
func TestReviewerBackgroundServiceExitMustHaveTerminalReceipt(t *testing.T) {
	f := newCPUFixture(t)
	backend := &reviewerBackgroundExit{cpuNode: f.node}
	f.runtime.Jobs = backend
	// A started service has already exited after workload, before teardown.
	f.node.onAuthorize = func() {
		if f.node.authorized == 11 {
			for id, spec := range f.node.specs {
				if len(spec.Argv) > 0 && spec.Argv[0] == "deploy-volume" {
					delete(f.node.live, id)
				}
			}
		}
	}
	result, bundle, err := tr.RunApprovedRequest(context.Background(), f.request, f.profile, RegisterPack, &f.runtime, nil)
	raw, readErr := os.ReadFile(filepath.Join(bundle, "artifacts", "authority.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	var evidence struct {
		Jobs map[string]tr.ApprovedJobReceipt
	}
	if e := json.Unmarshal(raw, &evidence); e != nil {
		t.Fatal(e)
	}
	missing := 0
	for _, receipt := range evidence.Jobs {
		if len(receipt.Spec.Argv) > 0 && strings.HasPrefix(receipt.Spec.Argv[0], "deploy-") && receipt.Exit == nil {
			missing++
		}
	}
	t.Logf("background Wait calls=%d missing_background_exit_receipts=%d status=%v err=%v", backend.waited, missing, result, err)
	if missing != 0 || backend.waited != 4 {
		t.Fatalf("background terminal outcomes lost: missing=%d waited=%d", missing, backend.waited)
	}
	if err == nil && result != nil && result.Status == tr.StatusPass {
		t.Fatal("unexpected background exit qualified as PASS")
	}
}
