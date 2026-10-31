//go:build linux

package testrunner

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pingqiu/sw-test-runner/infra"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type generationNode struct {
	*fakeOwnedNode
	next       int
	events     []string
	cleanup    map[string]bool
	killMode   string
	afterKill  func()
	beforeKill func()
}

func (n *generationNode) Enroll(ctx context.Context, id string, a RunAdmission, s JobSpec) (ProcessIdentity, error) {
	i, err := n.fakeOwnedNode.Enroll(ctx, id, a, s)
	if err != nil {
		return i, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.next++
	i.PID = 100 + n.next
	i.PGID = i.PID
	i.StartTime = uint64(n.next)
	n.ids[id] = i
	n.events = append(n.events, "enroll")
	return i, nil
}
func (n *generationNode) Wait(ctx context.Context, id string) (ExitInfo, error) {
	n.mu.Lock()
	n.events = append(n.events, "wait")
	n.mu.Unlock()
	e, err := n.fakeOwnedNode.Wait(ctx, id)
	n.mu.Lock()
	e.CleanupStopped = n.cleanup[id]
	n.mu.Unlock()
	return e, err
}
func (n *generationNode) Kill(ctx context.Context, id string) (KillWitness, error) {
	if n.beforeKill != nil {
		n.beforeKill()
	}
	n.mu.Lock()
	n.events = append(n.events, "kill")
	n.mu.Unlock()
	w, err := n.fakeOwnedNode.Kill(ctx, id)
	if n.afterKill != nil {
		n.afterKill()
	}
	switch n.killMode {
	case "pid":
		w.Identity.PID++
	case "start":
		w.Identity.StartTime++
	case "not-alive":
		w.AliveBefore = false
	case "lost":
		err = errors.New("lost kill reply")
	}
	return w, err
}
func (n *generationNode) Settle(ctx context.Context, id string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if n.cleanup == nil {
		n.cleanup = map[string]bool{}
	}
	n.cleanup[id] = true
	return nil
}
func generationFixture(t *testing.T) (*approvedFixture, *generationNode) {
	t.Helper()
	f := newApprovedFixture(t)
	n := &generationNode{fakeOwnedNode: f.node}
	f.runtime.Jobs = n
	f.node.exitCode = -1
	f.node.exitSignal = 9
	f.policy.Jobs = map[string]JobPin{"volume0": f.job, "volume1": f.job}
	f.policy.GenerationRestarts = map[string]GenerationRestartPlan{"volume-once": {Victim: "volume0", Replacement: "volume1"}}
	f.publish(t, nil)
	write := func(name string, v any) string {
		raw, _ := json.Marshal(v)
		if err := os.WriteFile(name, raw, 0600); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%x", sha256.Sum256(raw))
	}
	var approval ApprovedGO
	raw, _ := os.ReadFile(filepath.Join(f.dir, "go.json"))
	if err := json.Unmarshal(raw, &approval); err != nil {
		t.Fatal(err)
	}
	approval.AllowMutating = true
	hash := write(filepath.Join(f.dir, "go.json"), approval)
	var profile PolicyBootstrap
	raw, _ = os.ReadFile(f.profile)
	if err := json.Unmarshal(raw, &profile); err != nil {
		t.Fatal(err)
	}
	profile.Approval.SHA256 = hash
	write(f.profile, profile)
	return f, n
}
func runGenerationCase(t *testing.T, f *approvedFixture, action func(context.Context, *ActionContext) error, core ...bool) (*ApprovedRun, *ScenarioResult) {
	t.Helper()
	policy, err := ReadTrustedPolicy(f.profile, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenApprovedRun(context.Background(), f.request, policy, &f.runtime)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	r := NewRegistry()
	calls := 0
	handler := func(ctx context.Context, a *ActionContext, _ Action) (map[string]string, error) {
		calls++
		if calls > 1 {
			return nil, nil
		}
		return nil, action(ctx, a)
	}
	if len(core) > 0 && core[0] {
		r.RegisterFunc("probe", TierCore, handler)
	} else {
		r.RegisterMutatingFunc("probe", TierChaos, handler)
	}
	return run, run.Execute(context.Background(), NewEngine(r, nil), &ActionContext{})
}
func TestGenerationRestartSettlesOldBeforeReplacement(t *testing.T) {
	f, n := generationFixture(t)
	run, result := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
		old, _, err := a.RunApprovedPackJob(ctx, "volume0", true)
		if err != nil {
			return err
		}
		n.beforeKill = func() {
			intent := old.job.exitIntent
			if intent == nil || intent.Episode != "volume-once" || intent.Identity != old.job.identity || intent.BoundAt.IsZero() || old.job.expectedKill != nil {
				t.Error("exact exit intent was not bound before signal dispatch, or witness pre-existed")
			}
			receipt := a.approved.GenerationRestartReceipts()["volume-once"]
			if intent != nil && receipt.Intent != *intent {
				t.Error("owned generation receipt does not carry the pre-dispatch intent")
			}
		}
		next, r, err := a.RestartApprovedPackGeneration(ctx, "volume-once", old)
		if err != nil {
			return err
		}
		if next.scope == nil || r.State != "REPLACEMENT_STARTED" || !r.Kill.AliveBefore || !r.OldExit.Settled || r.Kill.Identity != r.OldExit.Identity || r.Replacement.PID == r.OldExit.Identity.PID {
			return fmt.Errorf("bad generation proof: %+v", r)
		}
		return nil
	})
	if result.Status != StatusPass {
		t.Fatalf("restart: %+v", result)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	want := []string{"enroll", "kill", "wait", "enroll", "wait"}
	if fmt.Sprint(n.events) != fmt.Sprint(want) {
		t.Fatalf("replacement before old settlement: %v", n.events)
	}
}

func TestGenerationRestartRefusesUnqualifiedExitAndAuthority(t *testing.T) {
	for _, mode := range []string{"pid", "start", "not-alive", "lost", "wrong-exit", "unknown-exit", "unplanned", "foreign-label", "revoked", "lost-replacement"} {
		t.Run(mode, func(t *testing.T) {
			f, n := generationFixture(t)
			n.killMode = mode
			if mode == "wrong-exit" {
				n.exitCode = 2
				n.exitSignal = 0
			}
			if mode == "unknown-exit" {
				n.waitUnsettled = true
			}
			if mode == "lost-replacement" {
				n.afterKill = func() { n.lostReply = true }
			}
			if mode == "revoked" {
				n.afterKill = func() {
					if err := os.WriteFile(filepath.Join(f.dir, "go.json"), []byte("{}"), 0600); err != nil {
						t.Error(err)
					}
				}
			}
			_, result := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
				label := "volume0"
				if mode == "foreign-label" {
					label = "volume1"
				}
				old, _, err := a.RunApprovedPackJob(ctx, label, true)
				if err != nil {
					return err
				}
				name := "volume-once"
				if mode == "unplanned" {
					name = "not-approved"
				}
				_, _, err = a.RestartApprovedPackGeneration(ctx, name, old)
				if err == nil {
					t.Error("unproven restart accepted")
				}
				return nil // swallowing the error must not permit Engine PASS
			})
			if result.Status != StatusFail {
				t.Fatalf("ignored provider failure became PASS: %+v", result)
			}
			n.mu.Lock()
			defer n.mu.Unlock()
			if n.authorized != 1 {
				t.Fatalf("replacement authorized after %s: %d", mode, n.authorized)
			}
			if (mode == "unplanned" || mode == "foreign-label") && n.killed != 0 {
				t.Fatal("unapproved target killed")
			}
		})
	}
}
func TestGenerationRestartDoesNotExcuseReplacementExit(t *testing.T) {
	f, n := generationFixture(t)
	_, result := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
		old, _, err := a.RunApprovedPackJob(ctx, "volume0", true)
		if err != nil {
			return err
		}
		other, _, err := a.RestartApprovedPackGeneration(ctx, "volume-once", old)
		if err != nil {
			return err
		}
		if _, err = a.Safety.Kill(ctx, other); err != nil {
			return err
		}
		_, err = a.Safety.Wait(ctx, other)
		if err == nil {
			t.Error("ordinary kill inherited a generation exemption")
		}
		return nil
	})
	if result.Status != StatusFail {
		t.Fatalf("replacement exit became PASS: %+v", result)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.killed != 2 {
		t.Fatal(n.killed)
	}
}
func TestGenerationRestartDoesNotEraseAnEarlierStickyExit(t *testing.T) {
	f, _ := generationFixture(t)
	_, result := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
		other, _, err := a.RunApprovedPackJob(ctx, "volume1", true)
		if err != nil {
			return err
		}
		if _, err = a.Safety.Kill(ctx, other); err != nil {
			return err
		}
		if _, err = a.Safety.Wait(ctx, other); err == nil {
			t.Error("unplanned exit was accepted")
		}
		old, _, err := a.RunApprovedPackJob(ctx, "volume0", true)
		if err != nil {
			return err
		}
		_, _, err = a.RestartApprovedPackGeneration(ctx, "volume-once", old)
		return err
	})
	if result.Status != StatusFail {
		t.Fatalf("restart erased sticky failure: %+v", result)
	}
}

func TestGenerationRestartEpisodeCannotBeReplayed(t *testing.T) {
	f, n := generationFixture(t)
	_, result := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
		old, _, err := a.RunApprovedPackJob(ctx, "volume0", true)
		if err != nil {
			return err
		}
		if _, _, err = a.RestartApprovedPackGeneration(ctx, "volume-once", old); err != nil {
			return err
		}
		again, _, err := a.RunApprovedPackJob(ctx, "volume0", true)
		if err != nil {
			return err
		}
		_, _, err = a.RestartApprovedPackGeneration(ctx, "volume-once", again)
		if err == nil {
			t.Error("episode replay accepted")
		}
		return nil
	})
	if result.Status != StatusFail {
		t.Fatalf("replay became PASS: %+v", result)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.killed != 1 {
		t.Fatal("replayed kill", n.killed)
	}
}

func TestGenerationRestartRequiresConsentEvenThroughCoreCaller(t *testing.T) {
	f, n := generationFixture(t)
	f.publish(t, nil) // Independent GO has allow_mutating=false.
	_, result := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
		old, _, err := a.RunApprovedPackJob(ctx, "volume0", true)
		if err != nil {
			return err
		}
		_, _, err = a.RestartApprovedPackGeneration(ctx, "volume-once", old)
		if err == nil {
			t.Error("no-consent restart accepted")
		}
		return nil
	}, true)
	if result.Status != StatusFail {
		t.Fatalf("core caller bypassed consent: %+v", result)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.killed != 0 || n.authorized != 1 {
		t.Fatal("no-consent mutation", n.killed, n.authorized)
	}
}
func TestGenerationRestartRejectsForeignScope(t *testing.T) {
	f, n := generationFixture(t)
	foreign := fakeScope(t, context.Background(), &fakeOwnedNode{}, testAdmission())
	h, err := foreign.Start(testJob())
	if err != nil {
		t.Fatal(err)
	}
	_, result := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
		_, _, err := a.RestartApprovedPackGeneration(ctx, "volume-once", h)
		if err == nil {
			t.Error("foreign scope accepted")
		}
		return nil
	})
	if result.Status != StatusFail {
		t.Fatal(result)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.authorized != 0 || n.killed != 0 {
		t.Fatal("foreign scope effects", n.authorized, n.killed)
	}
}
func TestGenerationRestartPolicyRefusesMissingAndAmbiguousLabels(t *testing.T) {
	for _, mode := range []string{"missing-victim", "missing-replacement", "same-label", "duplicate-victim", "duplicate-replacement"} {
		t.Run(mode, func(t *testing.T) {
			f, n := generationFixture(t)
			g := f.policy.GenerationRestarts["volume-once"]
			switch mode {
			case "missing-victim":
				g.Victim = "absent"
			case "missing-replacement":
				g.Replacement = "absent"
			case "same-label":
				g.Replacement = g.Victim
			case "duplicate-victim":
				f.policy.Jobs["volume2"] = f.job
				f.policy.GenerationRestarts["another"] = GenerationRestartPlan{Victim: "volume0", Replacement: "volume2"}
			case "duplicate-replacement":
				f.policy.Jobs["volume2"] = f.job
				f.policy.GenerationRestarts["another"] = GenerationRestartPlan{Victim: "volume2", Replacement: "volume1"}
			}
			f.policy.GenerationRestarts["volume-once"] = g
			f.publish(t, nil)
			policy, err := ReadTrustedPolicy(f.profile, uint32(os.Getuid()))
			if err != nil {
				t.Fatal(err)
			}
			run, err := OpenApprovedRun(context.Background(), f.request, policy, &f.runtime)
			if run != nil {
				_ = run.Close()
			}
			if err == nil {
				t.Fatal("ambiguous plan admitted")
			}
			n.mu.Lock()
			defer n.mu.Unlock()
			if n.authorized != 0 || n.killed != 0 {
				t.Fatal("policy refusal executed jobs")
			}
		})
	}
}

func TestGenerationRestartWithRealLocalCustodian(t *testing.T) {
	t.Setenv("S0A_TEST_CUSTODIAN", "")
	f, _ := generationFixture(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	install := filepath.Join(f.dir, "install")
	output := filepath.Join(f.dir, "output")
	for _, d := range []string{install, output} {
		if err = os.Mkdir(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	product := filepath.Join(install, "product")
	if err = os.WriteFile(product, raw, 0755); err != nil {
		t.Fatal(err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(raw))
	f.policy.Runner = filepath.Join(install, "runner")
	d := &f.policy.Deployments[0]
	d.InstallRoot = install
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	d.ExpectedBootID = strings.TrimSpace(string(boot))
	for i := range d.Files {
		d.Files[i].Path = strings.Replace(d.Files[i].Path, "/opt/approved", install, 1)
		if d.Files[i].Path == product {
			d.Files[i].SHA256 = hash
			d.Files[i].Size = int64(len(raw))
		}
	}
	f.job = JobPin{Node: "n1", Executable: product, SHA256: hash, Argv: []string{"s0a-empty-workload"}}
	f.policy.Jobs = map[string]JobPin{"volume0": f.job, "volume1": f.job}
	f.binding.OutputDir = output
	f.binding.Hosts[0].Root = output
	inventory := ApprovedInventory{Version: "s0b-inventory-v1", Hosts: f.binding.Hosts}
	raw, _ = json.Marshal(inventory)
	if err = os.WriteFile(f.binding.Inventory.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	f.binding.Inventory.SHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
	f.publish(t, nil)
	raw, _ = os.ReadFile(filepath.Join(f.dir, "go.json"))
	var goValue ApprovedGO
	_ = json.Unmarshal(raw, &goValue)
	goValue.AllowMutating = true
	raw, _ = json.Marshal(goValue)
	if err = os.WriteFile(filepath.Join(f.dir, "go.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	var bootstrap PolicyBootstrap
	body, _ := os.ReadFile(f.profile)
	_ = json.Unmarshal(body, &bootstrap)
	bootstrap.Approval.SHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
	body, _ = json.Marshal(bootstrap)
	if err = os.WriteFile(f.profile, body, 0600); err != nil {
		t.Fatal(err)
	}
	f.runtime.Jobs = &SSHJobs{Nodes: map[string]*infra.Node{"n1": {IsNative: true}}, Runner: exe}
	// Deployment/authority fixture is CPU data. Signals, process identities and
	// descendant settlement below use the real local custodian, never lab hosts.
	f.runtime.Readback = func(_ context.Context, p infra.DeploymentPolicy) (infra.DeploymentReceipt, error) {
		body, _ := json.Marshal(p)
		r := infra.DeploymentReceipt{NodeID: p.NodeID, Host: p.Host, User: p.User, HostKey: p.HostKey, BootID: p.ExpectedBootID, WorkloadUID: p.WorkloadUID, PolicySHA256: fmt.Sprintf("%x", sha256.Sum256(body)), ReadStart: time.Now(), ReadEnd: time.Now(), Files: map[string]string{}}
		for _, f := range p.Files {
			r.Files[f.Path] = f.SHA256
		}
		return r, nil
	}
	run, result := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
		old, _, err := a.RunApprovedPackJob(ctx, "volume0", true)
		if err != nil {
			return err
		}
		_, _, err = a.RestartApprovedPackGeneration(ctx, "volume-once", old)
		return err
	})
	if result.Status != StatusPass {
		t.Fatalf("actual local restart: %+v", result)
	}
	if err = run.Close(); err != nil {
		t.Fatal(err)
	}
	receipt := run.GenerationRestartReceipts()["volume-once"]
	jobs := run.jobReceipts()
	if receipt.State != "REPLACEMENT_STARTED" || jobs[receipt.VictimJobID].PackLabel != "volume0" || jobs[receipt.ReplacementJobID].PackLabel != "volume1" {
		t.Fatalf("generation/job receipt join: %+v", receipt)
	}
	for _, job := range jobs {
		if job.State != "EXIT_SETTLED" || job.Exit == nil || !job.Exit.Settled {
			t.Fatalf("local child not settled: %+v", job)
		}
	}
}
