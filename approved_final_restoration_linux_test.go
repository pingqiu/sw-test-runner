//go:build linux

package testrunner

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Only the compiled CPU jobs/readback seam is fake. Admission, engine, fault
// qualification, Close, resource proof and lease acquire/release are unchanged.
func TestApprovedFinalRestorationControls(t *testing.T) {
	for _, mode := range []string{"ordinary-pass", "ordinary-fail", "cleared-good", "never-cleared", "stale-final"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
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
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe, "-test.run=^TestApprovedFinalRestorationControlChild$", "-test.v", "-test.timeout=15s")
			for _, e := range os.Environ() {
				if !strings.HasPrefix(e, "APPROVED_FINAL_RESTORE_") && !strings.HasPrefix(e, "HOME=") {
					cmd.Env = append(cmd.Env, e)
				}
			}
			cmd.Env = append(cmd.Env, "HOME="+dir, "APPROVED_FINAL_RESTORE_MODE="+mode, "APPROVED_FINAL_RESTORE_LOCK="+lock)
			// Child FD9 shares the inherited locked open description. Close the
			// parent's descriptor immediately after Start; no detached holder.
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
				t.Fatalf("control process: %v", err)
			}
		})
	}
}

func TestApprovedFinalRestorationControlChild(t *testing.T) {
	mode := os.Getenv("APPROVED_FINAL_RESTORE_MODE")
	if mode == "" {
		t.Skip("only executed by bounded reviewer parent")
	}
	lock := os.Getenv("APPROVED_FINAL_RESTORE_LOCK")
	if err := verifyWorkerFlock(lock); err != nil {
		t.Fatal(err)
	}
	f := newApprovedFixture(t)
	apply, restore := f.job, f.job
	apply.Argv, restore.Argv = []string{"apply"}, []string{"restore"}
	f.policy.Jobs["apply"], f.policy.Jobs["restore"] = apply, restore
	grant := CleanupGrant{Kind: "partition", Node: "n1", Resource: "run1-private-port", Before: map[string]string{"owner": "run1", "link": "UP"}, During: map[string]string{"owner": "run1", "link": "DOWN"}, Apply: apply, Restore: restore, Deadline: f.binding.HardDeadline}
	resource := &virtualFault{state: "UP"}
	backend := &faultOwnedBackend{fakeOwnedNode: f.node, resource: resource, apply: apply, restore: restore}
	f.runtime.Jobs = backend
	scenario := []byte("name: reviewer-standards\nphases:\n  - name: witness\n    actions:\n      - action: final_restore_witness\n")
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
	if err = os.Mkdir(strings.TrimSuffix(lock, ".lock")+".quarantine", 0700); err != nil {
		t.Fatal(err)
	}
	bootstrap.WorkerLease = &WorkerLeaseConfig{LockFile: lock}
	bootstrap.LedgerDir = lock + ".ledger"
	if err = os.Mkdir(bootstrap.LedgerDir, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(f.profile, raw, 0600); err != nil {
		t.Fatal(err)
	}

	var run *ApprovedRun
	var firstClearSucceeded bool
	var foreign atomic.Bool
	var foreignObservations atomic.Int64
	register := func(registry *Registry) {
		registry.RegisterFunc("final_restore_witness", TierCore, func(ctx context.Context, a *ActionContext, _ Action) (map[string]string, error) {
			run = a.approved
			if mode == "ordinary-pass" || mode == "ordinary-fail" {
				h, e := a.Safety.Start(JobSpec{Node: f.job.Node, Executable: f.job.Executable, SHA256: f.job.SHA256, Argv: f.job.Argv})
				if e != nil {
					return nil, e
				}
				if _, e = a.Safety.Wait(ctx, h); e != nil {
					return nil, e
				}
				if mode == "ordinary-fail" {
					return nil, errors.New("reviewer intentional workload failure after proven job exit")
				}
				return nil, nil
			}
			plan := WitnessedFault{Kind: grant.Kind, Node: grant.Node, Resource: grant.Resource, Before: grant.Before, During: grant.During, Apply: JobSpec{Node: apply.Node, Executable: apply.Executable, SHA256: apply.SHA256, Argv: apply.Argv}, Restore: JobSpec{Node: restore.Node, Executable: restore.Executable, SHA256: restore.SHA256, Argv: restore.Argv}}
			plan.Observe = func(context.Context) (FaultObservation, error) {
				resource.mu.Lock()
				state := resource.state
				resource.mu.Unlock()
				owner := "run1"
				if foreign.Load() {
					owner = "foreign"
					foreignObservations.Add(1)
				}
				return FaultObservation{Node: "n1", BootID: "boot", Resource: grant.Resource, State: map[string]string{"owner": owner, "link": state}}, nil
			}
			if e := a.Safety.ApplyWitnessedFault(ctx, plan.Key(), plan); e != nil {
				return nil, e
			}
			if mode == "never-cleared" {
				foreign.Store(true)
			}
			e := a.Safety.ClearFault(context.Background(), plan.Key())
			firstClearSucceeded = e == nil
			if mode == "stale-final" {
				foreign.Store(true)
			}
			return nil, e
		})
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	result, bundle, entryErr := RunApprovedRequest(ctx, f.request, f.profile, register, &f.runtime, nil)
	if run == nil || result == nil || bundle == "" {
		t.Fatalf("entry did not execute witness: result=%+v bundle=%q err=%v", result, bundle, entryErr)
	}
	closeErr := run.Close()
	settled := run.resourcesSettled()
	run.scope.mu.Lock()
	jobs := len(run.scope.jobs)
	r := run.scope.restores[grant.Node+"/"+grant.Resource]
	run.scope.mu.Unlock()
	cleared := false
	cleanupErr := ""
	if r != nil {
		r.gate <- struct{}{}
		cleared = r.cleared
		if r.cleanupErr != nil {
			cleanupErr = r.cleanupErr.Error()
		}
		<-r.gate
	}
	acquire, e := os.ReadFile(filepath.Join(bootstrap.LedgerDir, f.binding.LeaseID+"-acquire.json"))
	if e != nil || len(acquire) == 0 {
		t.Fatalf("missing actual acquire: %v", e)
	}
	release, releaseErr := os.ReadFile(filepath.Join(bootstrap.LedgerDir, f.binding.LeaseID+"-release.json"))
	released := releaseErr == nil
	if releaseErr != nil && !os.IsNotExist(releaseErr) {
		t.Fatal(releaseErr)
	}
	var leaseEvidence struct {
		Released bool `json:"released"`
	}
	raw, e = os.ReadFile(filepath.Join(bundle, "artifacts/worker-lease.json"))
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(raw, &leaseEvidence); e != nil {
		t.Fatal(e)
	}
	if leaseEvidence.Released != released {
		t.Fatal("lease evidence disagrees with actual release file")
	}
	stringErr := func(e error) string {
		if e == nil {
			return ""
		}
		return e.Error()
	}
	observation := map[string]any{"mode": mode, "result_status": result.Status, "entry_error": stringErr(entryErr), "close_error": stringErr(closeErr), "first_clear_succeeded": firstClearSucceeded, "cleared": cleared, "cleanup_error": cleanupErr, "foreign_observations": foreignObservations.Load(), "live_jobs": jobs, "resources_settled": settled, "release_exists": released, "release_record": string(release), "job_receipts": run.jobReceipts(), "result_error": result.Error}
	raw, e = json.Marshal(observation)
	if e != nil {
		t.Fatal(e)
	}
	t.Logf("APPROVED_FINAL_RESTORE_OBSERVATION %s", raw)
	if jobs != 0 {
		t.Fatalf("unexpected live jobs: %d", jobs)
	}
	if mode == "stale-final" {
		if !firstClearSucceeded || !cleared || closeErr == nil || !strings.Contains(closeErr.Error(), "foreign fault state/ownership") || foreignObservations.Load() == 0 || settled || released || result.Status != StatusFail {
			t.Fatal("final foreign restoration must retain UNKNOWN after an earlier successful clear")
		}
	} else if mode == "never-cleared" {
		if firstClearSucceeded || cleared || closeErr == nil || settled || released {
			t.Fatal("uncleared negative control did not retain UNKNOWN")
		}
	} else {
		if closeErr != nil || !settled || !released {
			t.Fatal("proven settlement positive failed")
		}
		if mode == "ordinary-pass" && result.Status != StatusPass {
			t.Fatal("ordinary positive did not PASS")
		}
		if mode == "ordinary-fail" && result.Status != StatusFail {
			t.Fatal("intentional failed work did not FAIL")
		}
		if mode == "cleared-good" && (!firstClearSucceeded || !cleared) {
			t.Fatal("good restoration did not clear")
		}
	}
}
