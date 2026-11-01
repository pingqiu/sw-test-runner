//go:build linux

package testrunner

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/infra"
	"golang.org/x/crypto/ssh"
)

type approvedFixture struct {
	dir, request, profile string
	binding               RunBinding
	job                   JobPin
	policy                ApprovedRunPolicy
	node                  *fakeOwnedNode
	runtime               ApprovedRuntime
}

// privateTestDir is a fresh 0700 directory under the real home. Trusted
// records refuse any group- or world-writable ancestor (snapshotRecord), so a
// test that places them must not use t.TempDir(): its root, /tmp, is 1777.
func privateTestDir(t *testing.T, pattern string) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(home, pattern)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func newApprovedFixture(t *testing.T) *approvedFixture {
	t.Helper()
	dir := privateTestDir(t, ".s0b-run-")
	var err error
	f := &approvedFixture{dir: dir, node: &fakeOwnedNode{ids: map[string]ProcessIdentity{}, specs: map[string]JobSpec{}}}
	write := func(name string, v any) FilePin {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, name)
		if err = os.WriteFile(p, raw, 0600); err != nil {
			t.Fatal(err)
		}
		return FilePin{p, fmt.Sprintf("%x", sha256.Sum256(raw))}
	}
	pub, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(make([]byte, 32)).Public())
	if err != nil {
		t.Fatal(err)
	}
	host := ApprovedHost{Node: "n1", Host: "127.0.0.1:2222", User: "worker", HostKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), Root: "/run/approved"}
	files := []infra.DeploymentFile{}
	for i, role := range []string{"runner", "adapter", "config", "product", "native"} {
		files = append(files, infra.DeploymentFile{Path: "/opt/approved/" + role, SHA256: strings.Repeat(string(rune('a'+i)), 64), Size: 1, Roles: []string{role}})
	}
	f.job = JobPin{Node: "n1", Executable: "/opt/approved/product", SHA256: strings.Repeat("d", 64), Argv: []string{"", "literal"}}
	f.policy = ApprovedRunPolicy{Version: "s0b-run-policy-v1", Runner: "/opt/approved/runner", Deployments: []infra.DeploymentPolicy{{NodeID: "n1", Host: host.Host, User: host.User, HostKey: host.HostKey, InstallRoot: "/opt/approved", ExpectedBootID: "boot", WorkloadUID: 1000, RequireNative: true, Files: files}}, Jobs: map[string]JobPin{"probe": f.job}}
	policyPin := write("run-policy.json", f.policy)
	inventory := write("inventory.json", ApprovedInventory{Version: "s0b-inventory-v1", Hosts: []ApprovedHost{host}})
	artifacts := write("artifacts.json", ApprovedArtifacts{Version: "s0b-artifacts-v1", Deployments: f.policy.Deployments})
	scenario := []byte("name: approved\nphases:\n  - name: first\n    actions:\n      - action: probe\n  - name: second\n    actions:\n      - action: probe\n")
	scenarioPath := filepath.Join(dir, "scenario.yaml")
	if err = os.WriteFile(scenarioPath, scenario, 0600); err != nil {
		t.Fatal(err)
	}
	ledger, running := filepath.Join(dir, "ledger"), filepath.Join(dir, "running")
	for _, p := range []string{ledger, running} {
		if err = os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	id := "0123456789abcdef0123456789abcdef"
	stamp := float64(time.Now().UnixNano()) / 1e9
	raw, _ := json.Marshal(map[string]any{"lease_id": id, "agent": "codex03", "started": stamp - 10, "host": "broker-only"})
	if err = os.WriteFile(filepath.Join(ledger, id+"-acquire.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	f.binding = RunBinding{RequestID: "r1", RunID: "run1", Workload: "kv", Transport: "rdma", Inventory: inventory, Scenario: FilePin{scenarioPath, fmt.Sprintf("%x", sha256.Sum256(scenario))}, Policy: policyPin, Artifacts: artifacts, Hosts: []ApprovedHost{host}, LeaseID: id, LeaseOwner: "codex03", LeaseResource: "explicit-test-resource", OutputDir: host.Root, WorkDeadline: time.Now().Add(30 * time.Second), HardDeadline: time.Now().Add(40 * time.Second), CleanupReserveMS: 5000}
	request := ClaimedRequest{Version: "s0b-request-v1", Binding: f.binding}
	raw, _ = json.Marshal(request)
	f.request = filepath.Join(running, "r1.env")
	if err = os.WriteFile(f.request, raw, 0600); err != nil {
		t.Fatal(err)
	}
	approval := ApprovedGO{Version: "s0b-go-v1", SourceID: "0123456789abcdef01234567", RequestSHA256: fmt.Sprintf("%x", sha256.Sum256(raw)), Binding: f.binding}
	goPin := write("go.json", approval)
	f.profile = write("profile.json", PolicyBootstrap{Version: "s0b-bootstrap-v1", Approval: goPin, Policy: policyPin, RunningDir: running, LedgerDir: ledger, CancelFile: filepath.Join(dir, "cancel.json"), KeyFiles: map[string]string{"n1": "/service/private-key"}, ResultsDir: filepath.Join(dir, "results")}).Path
	// A fake transport is a pre-agreed CPU seam. It makes no real deployment
	// claim; PR2's encrypted-SSH real-file readback controls remain separate.
	f.runtime = ApprovedRuntime{Jobs: f.node, Readback: func(_ context.Context, p infra.DeploymentPolicy) (infra.DeploymentReceipt, error) {
		r := infra.DeploymentReceipt{NodeID: "n1", Host: host.Host, User: host.User, HostKey: host.HostKey, BootID: "boot", WorkloadUID: 1000, ReadStart: time.Now(), ReadEnd: time.Now(), Files: map[string]string{}}
		for _, file := range files {
			r.Files[file.Path] = file.SHA256
		}
		encoded, _ := json.Marshal(f.policy.Deployments[0])
		r.PolicySHA256 = fmt.Sprintf("%x", sha256.Sum256(encoded))
		return r, nil
	}}
	return f
}

func TestApprovedRunSharesOneScopeAcrossPhases(t *testing.T) {
	f := newApprovedFixture(t)
	policy, err := ReadTrustedPolicy(f.profile, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenApprovedRun(context.Background(), f.request, policy, &f.runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	r := NewRegistry()
	var first *RunScope
	seen := 0
	r.RegisterFunc("probe", TierCore, func(ctx context.Context, a *ActionContext, _ Action) (map[string]string, error) {
		if first == nil {
			first = a.Safety
		}
		if a.Safety != first || a.Safety != run.Scope() {
			return nil, fmt.Errorf("scope changed between phases")
		}
		seen++
		h, err := a.Safety.Start(JobSpec{Node: f.job.Node, Executable: f.job.Executable, SHA256: f.job.SHA256, Argv: f.job.Argv})
		if err != nil {
			return nil, err
		}
		_, err = a.Safety.Wait(ctx, h)
		return nil, err
	})
	result := run.Execute(context.Background(), NewEngine(r, nil), &ActionContext{})
	if result.Status != StatusPass || seen != 2 || f.node.authorized != 2 {
		t.Fatalf("single-scope dispatch: result=%#v seen=%d auth=%d", result, seen, f.node.authorized)
	}
	if err = run.Close(); err != nil {
		t.Fatal(err)
	}
}
