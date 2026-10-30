//go:build linux

package testrunner

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestApprovedWorkerSeparateClaimLockWithCanonicalHolder(t *testing.T) {
	for _, mode := range []string{"positive", "missing-lease", "released-lease"} {
		t.Run(mode, func(t *testing.T) {
			f := newApprovedFixture(t)
			raw := []byte("name: approved\nphases:\n  - name: owned\n    actions:\n      - action: approved_job\n        job: probe\n  - name: second\n    actions:\n      - action: approved_job\n        job: probe\n")
			if err := os.WriteFile(f.binding.Scenario.Path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			f.binding.Scenario.SHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
			f.publish(t, nil)
			state := f.dir
			queue := filepath.Join(f.dir, "queue")
			if err := os.Mkdir(queue, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(f.request, filepath.Join(queue, "r1.env")); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(f.dir, "submitted-shell-ran")
			switch mode {
			case "missing-lease":
				if err := os.Remove(filepath.Join(f.dir, "ledger", f.binding.LeaseID+"-acquire.json")); err != nil {
					t.Fatal(err)
				}
			case "released-lease":
				if err := os.WriteFile(filepath.Join(f.dir, "ledger", f.binding.LeaseID+"-release.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			// The external lease holder retains the canonical flock throughout
			// the unchanged worker's claim, actual adapter admission and jobs.
			holder, err := os.OpenFile(filepath.Join(f.dir, "rdma-lab.lock"), os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Close()
			if err = syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Fatal(err)
			}
			defer syscall.Flock(int(holder.Fd()), syscall.LOCK_UN)

			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(exe)
			if err != nil {
				t.Fatal(err)
			}
			adapter := filepath.Join(f.dir, "adapter")
			if err = os.WriteFile(adapter, body, 0700); err != nil {
				t.Fatal(err)
			}
			digest := fmt.Sprintf("%x", sha256.Sum256(body))
			if mode == "adapter-pin" {
				digest = strings.Repeat("0", 64)
			}
			script, err := filepath.Abs("scripts/testops-ci-worker.sh")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", script, "--once")
			for _, e := range os.Environ() {
				if !strings.HasPrefix(e, "TESTOPS_") && !strings.HasPrefix(e, "S0B_CPU_") {
					cmd.Env = append(cmd.Env, e)
				}
			}
			cmd.Env = append(cmd.Env, "S0B_CPU_ADAPTER=1", "TESTOPS_QUEUE_DIR="+queue, "TESTOPS_STATE_DIR="+state, "TESTOPS_LOCK_FILE="+filepath.Join(f.dir, "flock"), "TESTOPS_LOG_DIR="+filepath.Join(f.dir, "logs"), "TESTOPS_STATUS_DIR="+filepath.Join(f.dir, "status"), "TESTOPS_NOTIFY_CMD=:", "TESTOPS_RDMA_CI_SCRIPT=/bin/true", "TESTOPS_APPROVED_ADAPTER="+adapter, "TESTOPS_APPROVED_ADAPTER_SHA256="+digest, "TESTOPS_APPROVED_POLICY="+f.profile)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("worker failure: %v %s", err, out)
			}
			log, err := os.ReadFile(filepath.Join(f.dir, "logs/r1.log"))
			if err != nil {
				t.Fatal(err)
			}
			contender, err := os.OpenFile(filepath.Join(f.dir, "rdma-lab.lock"), os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer contender.Close()
			if err := syscall.Flock(int(contender.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
				t.Fatal("canonical exclusion lost while worker claimed")
			}
			t.Logf("mode=%s canonical holder retained; worker claimed; %s", mode, string(log))
			want := "failed"
			if mode == "positive" {
				want = "done"
				if !strings.Contains(string(log), "CPU authorized_jobs=2") {
					t.Fatalf("adapter did not execute owned job: %s", log)
				}
			} else if !strings.Contains(string(log), "CPU authorized_jobs=0") {
				t.Fatalf("refusal executed a job: %s", log)
			}
			if _, err = os.Stat(filepath.Join(state, want, "r1.env")); err != nil {
				t.Fatalf("wrong transition %s: %v %s", want, err, log)
			}
			if _, err = os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("submitted shell ran: %v", err)
			}
			if mode == "positive" {
				start := strings.Index(string(log), "bundle: ")
				if start < 0 {
					t.Fatal("missing bundle")
				}
				dir := strings.Split(string(log)[start+len("bundle: "):], "\n")[0]
				raw, e := os.ReadFile(filepath.Join(dir, "artifacts", "authority.json"))
				if e != nil {
					t.Fatal(e)
				}
				var evidence struct {
					Jobs    map[string]ApprovedJobReceipt
					Request ClaimedRequest
					GOFile  FilePin
					Lease   LeaseObservation
				}
				if e = json.Unmarshal(raw, &evidence); e != nil {
					t.Fatal(e)
				}
				if len(evidence.Jobs) != 2 || evidence.Request.Binding.RunID != "run1" || evidence.Lease.LeaseID != f.binding.LeaseID {
					t.Fatalf("incomplete custody evidence: %s", raw)
				}
				for _, job := range evidence.Jobs {
					if job.State != "EXIT_SETTLED" || job.Exit == nil || job.Spec.Argv[0] != "" {
						t.Fatalf("lost final proof/empty argv: %#v", job)
					}
				}
				for name, digest := range map[string]string{"claimed-request.json": evidence.Request.SHA256, "approved-go.json": evidence.GOFile.SHA256, "inventory.json": f.binding.Inventory.SHA256, "artifacts.json": f.binding.Artifacts.SHA256, "run-policy.json": f.binding.Policy.SHA256} {
					// Request.SHA256 is intentionally excluded from request JSON; the
					// independent GO supplies its exact original-byte digest.
					if name == "claimed-request.json" {
						var goData ApprovedGO
						goRaw, e := os.ReadFile(filepath.Join(dir, "artifacts", "approved-go.json"))
						if e != nil {
							t.Fatal(e)
						}
						if e = json.Unmarshal(goRaw, &goData); e != nil {
							t.Fatal(e)
						}
						digest = goData.RequestSHA256
					}
					body, e := os.ReadFile(filepath.Join(dir, "artifacts", name))
					if e != nil {
						t.Fatal(e)
					}
					if fmt.Sprintf("%x", sha256.Sum256(body)) != digest {
						t.Fatalf("snapshot mismatch: %s", name)
					}
				}
			}
		})
	}
}
