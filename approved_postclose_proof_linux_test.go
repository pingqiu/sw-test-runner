//go:build linux

package testrunner

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestApprovedPostCloseProof(t *testing.T) {
	for _, mode := range []string{"repeated-close", "always-clear", "fail-final", "always-foreign", "always-foreign-then-good"} {
		t.Run(mode, func(t *testing.T) {
			dir := privateTestDir(t, ".s0b-postclose-")
			lock := filepath.Join(dir, "canonical.lock")
			f, err := os.OpenFile(lock, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
				t.Fatal(err)
			}
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe, "-test.run=^TestApprovedPostCloseProofChild$", "-test.v", "-test.timeout=10s")
			for _, e := range os.Environ() {
				if !strings.HasPrefix(e, "APPROVED_POSTCLOSE_") && !strings.HasPrefix(e, "HOME=") {
					cmd.Env = append(cmd.Env, e)
				}
			}
			cmd.Env = append(cmd.Env, "HOME="+dir, "APPROVED_POSTCLOSE_MODE="+mode, "APPROVED_POSTCLOSE_LOCK="+lock)
			for i := 3; i <= 9; i++ {
				cmd.ExtraFiles = append(cmd.ExtraFiles, f)
			}
			var output strings.Builder
			cmd.Stdout, cmd.Stderr = &output, &output
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			if err = f.Close(); err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			t.Log(output.String())
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestApprovedPostCloseProofChild(t *testing.T) {
	mode := os.Getenv("APPROVED_POSTCLOSE_MODE")
	if mode == "" {
		t.Skip("bounded child only")
	}
	lock := os.Getenv("APPROVED_POSTCLOSE_LOCK")
	if err := verifyWorkerFlock(lock); err != nil {
		t.Fatal(err)
	}
	f := newApprovedFixture(t)
	apply, restore := f.job, f.job
	apply.Argv, restore.Argv = []string{"apply"}, []string{"restore"}
	f.policy.Jobs["apply"], f.policy.Jobs["restore"] = apply, restore
	grant := CleanupGrant{Kind: "partition", Node: "n1", Resource: "run1-private-port", Before: map[string]string{"owner": "run1", "link": "UP"}, During: map[string]string{"owner": "run1", "link": "DOWN"}, Apply: apply, Restore: restore, Deadline: f.binding.HardDeadline}
	resource := &virtualFault{state: "UP"}
	counter := &postCloseRestoreCounter{OwnedJobBackend: &faultOwnedBackend{fakeOwnedNode: f.node, resource: resource, apply: apply, restore: restore}}
	f.runtime.Jobs = counter
	scenario := []byte("name: postclose-proof\nphases:\n  - name: work\n    actions:\n      - action: postclose_apply\n  - name: final\n    always: true\n    actions:\n      - action: postclose_always\n")
	if err := os.WriteFile(f.binding.Scenario.Path, scenario, 0600); err != nil {
		t.Fatal(err)
	}
	f.binding.Scenario.SHA256 = fmt.Sprintf("%x", sha256.Sum256(scenario))
	f.publish(t, []CleanupGrant{grant})
	var bootstrap PolicyBootstrap
	raw, err := os.ReadFile(f.profile)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &bootstrap); err != nil {
		t.Fatal(err)
	}
	bootstrap.WorkerLease = &WorkerLeaseConfig{LockFile: lock}
	bootstrap.LedgerDir = lock + ".ledger"
	for _, dir := range []string{bootstrap.LedgerDir, strings.TrimSuffix(lock, ".lock") + ".quarantine"} {
		if err = os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	raw, err = json.Marshal(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(f.profile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var run *ApprovedRun
	var foreign atomic.Bool
	var before, after bool
	var alwaysRan bool
	var clearErr, close1, close2 error
	proof := func(s *RunScope) bool {
		s.mu.Lock()
		r := s.restores[grant.Node+"/"+grant.Resource]
		s.mu.Unlock()
		r.gate <- struct{}{}
		v := r.finalVerified
		<-r.gate
		return v
	}
	plan := WitnessedFault{Kind: grant.Kind, Node: grant.Node, Resource: grant.Resource, Before: grant.Before, During: grant.During, Apply: JobSpec{Node: apply.Node, Executable: apply.Executable, SHA256: apply.SHA256, Argv: apply.Argv}, Restore: JobSpec{Node: restore.Node, Executable: restore.Executable, SHA256: restore.SHA256, Argv: restore.Argv}}
	plan.Observe = func(context.Context) (FaultObservation, error) {
		resource.mu.Lock()
		state := resource.state
		resource.mu.Unlock()
		owner := "run1"
		if foreign.Load() {
			owner = "foreign"
		}
		return FaultObservation{Node: "n1", BootID: "boot", Resource: grant.Resource, State: map[string]string{"owner": owner, "link": state}}, nil
	}
	register := func(reg *Registry) {
		reg.RegisterFunc("postclose_apply", TierCore, func(ctx context.Context, a *ActionContext, _ Action) (map[string]string, error) {
			run = a.approved
			err := a.Safety.ApplyWitnessedFault(ctx, plan.Key(), plan)
			if mode == "fail-final" {
				foreign.Store(true)
			}
			return nil, err
		})
		reg.RegisterFunc("postclose_always", TierCore, func(ctx context.Context, a *ActionContext, _ Action) (map[string]string, error) {
			alwaysRan = true
			before = proof(a.Safety)
			close1 = a.Safety.Close()
			if mode == "always-clear" || mode == "fail-final" {
				clearErr = a.Safety.ClearFault(ctx, plan.Key())
			}
			if strings.HasPrefix(mode, "always-foreign") {
				foreign.Store(true)
				clearErr = a.Safety.ClearFault(ctx, plan.Key())
				if clearErr == nil {
					t.Fatal("foreign post-Close requalification succeeded")
				}
				if mode == "always-foreign-then-good" {
					foreign.Store(false)
					_ = a.Safety.ClearFault(ctx, plan.Key())
				}
			}
			after = proof(a.Safety)
			close2 = a.Safety.Close()
			return nil, clearErr
		})
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	result, bundle, entryErr := RunApprovedRequest(ctx, f.request, f.profile, register, &f.runtime, nil)
	if run == nil || result == nil || bundle == "" || !alwaysRan {
		t.Fatalf("not composed: result=%+v bundle=%q err=%v always=%t", result, bundle, entryErr, alwaysRan)
	}
	_, err = os.Stat(filepath.Join(bootstrap.LedgerDir, f.binding.LeaseID+"-acquire.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = os.Stat(filepath.Join(bootstrap.LedgerDir, f.binding.LeaseID+"-release.json"))
	released := err == nil
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var evidence struct {
		Released bool `json:"released"`
	}
	raw, err = os.ReadFile(filepath.Join(bundle, "artifacts/worker-lease.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.Released != released {
		t.Fatal("lease evidence mismatch")
	}
	run.scope.mu.Lock()
	jobs := len(run.scope.jobs)
	run.scope.mu.Unlock()
	settled := run.resourcesSettled()
	str := func(e error) string {
		if e == nil {
			return ""
		}
		return e.Error()
	}
	raw, _ = json.Marshal(map[string]any{"mode": mode, "before_always_final_proof": before, "after_always_final_proof": after, "final_proof": proof(run.scope), "always_clear_error": str(clearErr), "close1_error": str(close1), "close2_error": str(close2), "final_close_error": str(run.Close()), "entry_error": str(entryErr), "result_status": result.Status, "resources_settled": settled, "live_jobs": jobs, "release_exists": released, "evidence_released": evidence.Released})
	t.Logf("APPROVED_POSTCLOSE_OBSERVATION %s", raw)
	if jobs != 0 {
		t.Fatal("unexpected live jobs")
	}
	wantRestores := int64(1)
	if mode == "fail-final" {
		wantRestores = 0
	}
	if counter.restores.Load() != wantRestores {
		t.Fatalf("completed restore reran: %d, want %d", counter.restores.Load(), wantRestores)
	}
	switch mode {
	case "repeated-close":
		if !before || !after || close1 != nil || close2 != nil || !settled || !released || result.Status != StatusPass {
			t.Fatal("repeated Close positive failed")
		}
	case "always-clear":
		if !before || !after || clearErr != nil || close1 != nil || close2 != nil || !settled || !released || result.Status != StatusPass {
			t.Fatal("successful always ClearFault must preserve final settlement and release")
		}
	case "always-foreign", "always-foreign-then-good":
		if !before || after || clearErr == nil || settled || released || result.Status != StatusFail {
			t.Fatal("post-Close failure must invalidate proof and later public clear must not revive it")
		}
	case "fail-final":
		if before || after || clearErr == nil || close1 == nil || settled || released || result.Status != StatusFail {
			t.Fatal("failed-final negative failed")
		}
	}
}

// Count dispatches at the existing backend seam; never edit restoration proof.
type postCloseRestoreCounter struct {
	OwnedJobBackend
	restores atomic.Int64
}

func (b *postCloseRestoreCounter) Enroll(ctx context.Context, id string, a RunAdmission, s JobSpec) (ProcessIdentity, error) {
	if len(s.Argv) > 0 && s.Argv[0] == "restore" {
		b.restores.Add(1)
	}
	return b.OwnedJobBackend.Enroll(ctx, id, a, s)
}
