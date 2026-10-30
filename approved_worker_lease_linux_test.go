//go:build linux

package testrunner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
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

// This counter wraps the actual local custody backend; it does not replace it.
type leaseCustodyCounter struct {
	OwnedJobBackend
	authorized atomic.Int64
}

func (b *leaseCustodyCounter) Authorize(ctx context.Context, id string, i ProcessIdentity) error {
	err := b.OwnedJobBackend.Authorize(ctx, id, i)
	if err == nil {
		b.authorized.Add(1)
	}
	return err
}

// Receipt loss is injected AFTER the real local custodian settles the child.
// The adapter must preserve UNKNOWN rather than manufacture its missing proof.
type lostLeaseReceipt struct{ OwnedJobBackend }

func (b lostLeaseReceipt) Wait(ctx context.Context, id string) (ExitInfo, error) {
	_, err := b.OwnedJobBackend.Wait(ctx, id)
	return ExitInfo{}, errors.Join(err, errors.New("test: actual settlement receipt lost"))
}

func TestApprovedWorkerOwnsCanonicalLeaseLifetime(t *testing.T) {
	modes := []string{"valid", "cancel", "unknown", "missing-go", "wrong-go", "revoked-go", "foreign-request", "foreign-lease", "existing-id", "long-envelope", "missing-fd", "wrong-fd"}
	if os.Getenv("S0B_RENEW_REAL_WAIT") == "1" {
		modes = []string{"renew"}
	}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			f := newApprovedFixture(t)
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(exe)
			if err != nil {
				t.Fatal(err)
			}
			release := filepath.Join(f.dir, "release")
			if err = os.Mkdir(release, 0700); err != nil {
				t.Fatal(err)
			}
			adapter := filepath.Join(release, "runner")
			if err = os.WriteFile(adapter, body, 0700); err != nil {
				t.Fatal(err)
			}
			digest := fmt.Sprintf("%x", sha256.Sum256(body))
			dep := &f.policy.Deployments[0]
			dep.InstallRoot = release
			for i := range dep.Files {
				dep.Files[i].Path = filepath.Join(release, filepath.Base(dep.Files[i].Path))
			}
			dep.Files[0].Path = adapter
			dep.Files[0].SHA256 = digest
			dep.Files[0].Size = int64(len(body))
			dep.Files[0].Roles = []string{"runner", "product"}
			boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
			if err != nil {
				t.Fatal(err)
			}
			dep.ExpectedBootID = strings.TrimSpace(string(boot))
			f.policy.Runner = adapter
			f.job.Executable = adapter
			f.job.SHA256 = digest
			f.job.Argv = []string{"s0a-profile-output"}
			if mode == "cancel" || mode == "renew" {
				f.job.Argv = []string{"s0a-empty-workload"}
			}
			f.policy.Jobs["probe"] = f.job
			f.binding.OutputDir = filepath.Join(f.dir, "output")
			if err = os.Mkdir(f.binding.OutputDir, 0700); err != nil {
				t.Fatal(err)
			}
			f.binding.Hosts[0].Root = f.binding.OutputDir
			inv, _ := json.Marshal(ApprovedInventory{Version: "s0b-inventory-v1", Hosts: f.binding.Hosts})
			if err = os.WriteFile(f.binding.Inventory.Path, inv, 0600); err != nil {
				t.Fatal(err)
			}
			f.binding.Inventory.SHA256 = fmt.Sprintf("%x", sha256.Sum256(inv))
			scenario := []byte("name: owned-lease\nphases:\n  - name: owned\n    actions:\n      - action: approved_job\n        job: probe\n")
			if mode == "cancel" || mode == "renew" {
				scenario = []byte(strings.Replace(string(scenario), "approved_job", "lease_hold_job", 1))
			}
			if err = os.WriteFile(f.binding.Scenario.Path, scenario, 0600); err != nil {
				t.Fatal(err)
			}
			f.binding.Scenario.SHA256 = fmt.Sprintf("%x", sha256.Sum256(scenario))
			if mode == "long-envelope" {
				f.binding.WorkDeadline = time.Now().Add(11 * time.Minute)
				f.binding.HardDeadline = f.binding.WorkDeadline.Add(5 * time.Second)
				f.binding.CleanupReserveMS = 5000
			}
			if mode == "renew" {
				f.binding.WorkDeadline = time.Now().Add(255 * time.Second)
				f.binding.HardDeadline = f.binding.WorkDeadline.Add(5 * time.Second)
				f.binding.CleanupReserveMS = 5000
			}
			f.publish(t, nil)
			lock := filepath.Join(f.dir, "rdma-lab.lock")
			ledger := lock + ".ledger"
			if err = os.Rename(filepath.Join(f.dir, "ledger"), ledger); err != nil {
				t.Fatal(err)
			}
			acquire := filepath.Join(ledger, f.binding.LeaseID+"-acquire.json")
			if err = os.Remove(acquire); err != nil {
				t.Fatal(err)
			}
			var bootstrap map[string]any
			raw, _ := os.ReadFile(f.profile)
			if err = json.Unmarshal(raw, &bootstrap); err != nil {
				t.Fatal(err)
			}
			bootstrap["ledger_dir"] = ledger
			bootstrap["worker_lease"] = map[string]any{"lock_file": lock}
			raw, _ = json.Marshal(bootstrap)
			if err = os.WriteFile(f.profile, raw, 0600); err != nil {
				t.Fatal(err)
			}
			existingAcquire := []byte(`{"lease_id":"0123456789abcdef0123456789abcdef","agent":"foreign","started":1}`)
			switch mode {
			case "existing-id":
				if err = os.WriteFile(acquire, existingAcquire, 0600); err != nil {
					t.Fatal(err)
				}
			case "missing-go":
				if err = os.Remove(filepath.Join(f.dir, "go.json")); err != nil {
					t.Fatal(err)
				}
			case "revoked-go":
				if err = os.WriteFile(filepath.Join(f.dir, "cancel.json"), []byte("cancelled"), 0600); err != nil {
					t.Fatal(err)
				}
			case "foreign-request":
				raw, _ := os.ReadFile(f.request)
				raw = []byte(strings.Replace(string(raw), `"run_id":"run1"`, `"run_id":"foreign"`, 1))
				if err = os.WriteFile(f.request, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong-go":
				if err = os.WriteFile(filepath.Join(f.dir, "go.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "foreign-lease":
				if err = os.WriteFile(filepath.Join(ledger, strings.Repeat("f", 32)+"-acquire.json"), []byte(`{"lease_id":"ffffffffffffffffffffffffffffffff","agent":"foreign","started":1}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			queue := filepath.Join(f.dir, "queue")
			if err = os.Mkdir(queue, 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.Rename(f.request, filepath.Join(queue, "r1.env")); err != nil {
				t.Fatal(err)
			}
			script, err := filepath.Abs("scripts/testops-ci-worker.sh")
			if err != nil {
				t.Fatal(err)
			}
			workerBound := 15 * time.Second
			if mode == "renew" {
				workerBound = 260 * time.Second
			}
			ctx, cancel := context.WithTimeout(t.Context(), workerBound)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", script, "--once")
			for _, v := range os.Environ() {
				if !strings.HasPrefix(v, "TESTOPS_") && !strings.HasPrefix(v, "S0B_CPU_") && !strings.HasPrefix(v, "S0A_TEST_") {
					cmd.Env = append(cmd.Env, v)
				}
			}
			cmd.Env = append(cmd.Env, "S0B_CPU_ADAPTER=1", "S0B_CPU_CUSTODY="+mode, "S0B_CPU_CANONICAL_OBSERVED="+filepath.Join(f.dir, "canonical-observed"), "TESTOPS_QUEUE_DIR="+queue, "TESTOPS_STATE_DIR="+f.dir, "TESTOPS_LOCK_FILE="+lock, "TESTOPS_LOG_DIR="+filepath.Join(f.dir, "logs"), "TESTOPS_STATUS_DIR="+filepath.Join(f.dir, "status"), "TESTOPS_NOTIFY_CMD=:", "TESTOPS_APPROVED_ADAPTER="+adapter, "TESTOPS_APPROVED_ADAPTER_SHA256="+digest, "TESTOPS_APPROVED_POLICY="+f.profile)
			// Wrong/missing FD controls retain the real worker's canonical flock but
			// point the protected bootstrap at a different inode or close FD9 only at
			// the compiled test-entry seam, never via a deployed request flag.
			if mode == "wrong-fd" {
				bootstrap["worker_lease"] = map[string]any{"lock_file": filepath.Join(f.dir, "other.lock")}
				newLedger := filepath.Join(f.dir, "other.lock.ledger")
				if err = os.Rename(ledger, newLedger); err != nil {
					t.Fatal(err)
				}
				ledger = newLedger
				acquire = filepath.Join(ledger, f.binding.LeaseID+"-acquire.json")
				bootstrap["ledger_dir"] = ledger
				raw, _ = json.Marshal(bootstrap)
				if err = os.WriteFile(f.profile, raw, 0600); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(f.dir, "other.lock"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			cmd.Stdout = &output
			cmd.Stderr = &output
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			if mode == "cancel" || mode == "renew" {
				until := time.Now().Add(5 * time.Second)
				for {
					if _, e := ReadLiveLease(ledger, f.binding.LeaseID, f.binding.LeaseOwner); e == nil {
						break
					}
					if time.Now().After(until) {
						_ = cmd.Process.Kill()
						_ = cmd.Wait()
						t.Fatalf("acquire not live before observation: %s", output.String())
					}
					time.Sleep(5 * time.Millisecond)
				}
				probe, e := unix.Open(lock, unix.O_RDWR|unix.O_CLOEXEC, 0)
				if e != nil {
					t.Fatal(e)
				}
				e = unix.Flock(probe, unix.LOCK_EX|unix.LOCK_NB)
				_ = unix.Close(probe)
				if e != unix.EWOULDBLOCK && e != unix.EAGAIN {
					t.Fatal("worker did not retain canonical exclusion during live lease")
				}
				if err = os.WriteFile(filepath.Join(f.dir, "canonical-observed"), []byte("observed"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err = cmd.Wait(); err != nil {
				t.Fatalf("worker: %v %s", err, output.String())
			}
			logs, err := os.ReadFile(filepath.Join(f.dir, "logs/r1.log"))
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: %s", mode, logs)
			wantAuthorized := "0"
			if mode == "valid" || mode == "cancel" || mode == "renew" || mode == "unknown" {
				wantAuthorized = "1"
			}
			if !strings.Contains(string(logs), "CPU actual_custodian_authorized_jobs="+wantAuthorized+"\n") {
				t.Fatalf("wrong actual authorization count: %s", logs)
			}
			exists := func(name string) bool { _, err := os.Stat(name); return err == nil }
			started := exists(acquire)
			released := exists(filepath.Join(ledger, f.binding.LeaseID+"-release.json"))
			switch mode {
			case "valid", "cancel", "renew":
				if !started || !released {
					t.Fatalf("no proven lease lifetime started=%t released=%t: %s", started, released, logs)
				}
			case "existing-id":
				raw, e := os.ReadFile(acquire)
				if e != nil || !bytes.Equal(raw, existingAcquire) || released {
					t.Fatal("existing acquire overwritten or released")
				}
				files, e := os.ReadDir(f.binding.OutputDir)
				if e != nil || len(files) != 0 {
					t.Fatal("existing lease authorized custody")
				}
			case "unknown":
				if !started || released {
					t.Fatalf("UNKNOWN not retained: %s", logs)
				}
			default:
				children, e := os.ReadDir(f.binding.OutputDir)
				if e != nil || len(children) != 0 {
					t.Fatalf("refused authority reached custodian: %v %v", children, e)
				}
				if started || released {
					t.Fatalf("refused bootstrap wrote usable lease: %s", logs)
				}
			}
			if mode == "renew" && !exists(filepath.Join(ledger, f.binding.LeaseID+"-renew.json")) {
				t.Fatal("240-second actual renewal missing")
			}
			if mode == "valid" || mode == "cancel" || mode == "renew" {
				at := strings.Index(string(logs), "bundle: ")
				if at < 0 {
					t.Fatal("no actual custody bundle")
				}
				bundle := strings.Split(string(logs)[at+8:], "\n")[0]
				raw, e := os.ReadFile(filepath.Join(bundle, "artifacts/authority.json"))
				if e != nil {
					t.Fatal(e)
				}
				var proof struct{ Jobs map[string]ApprovedJobReceipt }
				if e = json.Unmarshal(raw, &proof); e != nil {
					t.Fatal(e)
				}
				if len(proof.Jobs) != 1 {
					t.Fatalf("missing native custody: %s", raw)
				}
				for _, j := range proof.Jobs {
					if j.Identity.PID <= 0 || j.Identity.BootID != dep.ExpectedBootID {
						t.Fatalf("wrong native identity: %+v", j)
					}
					if mode == "valid" {
						if j.State != "EXIT_SETTLED" || j.Exit == nil || !j.Exit.Settled {
							t.Fatalf("missing native exit: %+v", j)
						}
					} else if j.State != "SETTLED" && j.State != "EXIT_SETTLED" {
						t.Fatalf("no positive cleanup settlement: %+v", j)
					}
					// Foreground cancellation's existing SETTLED row intentionally
					// retains the earlier cancelled Wait; independently check the
					// exact actual leader was reaped, not just a desired exit code.
					fd, e := unix.PidfdOpen(j.Identity.PID, 0)
					if e == nil {
						_ = unix.Close(fd)
						t.Fatal("native leader survives settled lease release")
					}
					if e != unix.ESRCH {
						t.Fatal(e)
					}
				}
			}
			if mode == "wrong-fd" && !strings.Contains(string(logs), "FD9 does not match") {
				t.Fatalf("wrong rejection boundary: %s", logs)
			}
			for _, kind := range []string{"acquire", "renew", "release"} {
				raw, e := os.ReadFile(filepath.Join(ledger, f.binding.LeaseID+"-"+kind+".json"))
				if e == nil {
					t.Logf("%s record: %s", kind, raw)
				}
			}
			if mode == "valid" && !exists(filepath.Join(f.dir, "done/r1.env")) {
				t.Fatalf("valid run refused: %s", logs)
			}
			if mode != "valid" && !exists(filepath.Join(f.dir, "failed/r1.env")) {
				t.Fatalf("refusal/cancellation reported pass: %s", logs)
			}
		})
	}
}
