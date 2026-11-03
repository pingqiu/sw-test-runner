//go:build linux

package testrunner

// Soak clock-bracket step 1 (claude01 6ae9d04b; plan r2 9f8fca40, accepted by codex01 6ae9d02c).
// Measurement only, never run in CI: it is skipped unless S0B_CLOCK_DECOMPOSE names a JSON config.
//
// It times one approved-style M02 job through the real RunScope -> SSHJobs -> owned-custodian path over a real
// SSH connection, with the controller and the node on ONE host, so every stamp (Go CLOCK_MONOTONIC here,
// Python time.monotonic() in the job) is in the same clock domain and the split is exact up to read cost.
// The approved backend's per-step work check is reproduced in its order: approvedJobs.Enroll and Authorize
// each call work() -> authority() + readbackNode() -> VerifyDeployment BEFORE the custody call
// (approved_backend.go). authority() reads only local controller files and is not reproduced; that is a
// stated gap, not a measured zero.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/pingqiu/sw-test-runner/infra"
)

type decomposeConfig struct {
	Host, User, HostKey, KeyFile string
	Runner                       string // this test binary's path on the node
	Policy                       string // DeploymentPolicy JSON for the node (the r8b m02 file set, rewritten)
	Python, PythonSHA256, Job    string // interpreter and the job script (stamps T0/entry/exit)
	OutputDir, Out               string
	Runs                         int
	Verify                       bool // reproduce approvedJobs' VerifyDeployment before Enroll and before Authorize
}

func mono(t *testing.T) float64 {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		t.Fatal(err)
	}
	return float64(ts.Sec) + float64(ts.Nsec)/1e9
}

type span struct{ Start, End float64 }

// timedJobs is the approved backend's ordering around the real SSH backend, with stamps.
type timedJobs struct {
	*SSHJobs
	t      *testing.T
	node   *infra.Node
	policy infra.DeploymentPolicy
	verify bool
	boot   string
	mu     sync.Mutex
	spans  map[string]span
	errs   map[string]string
}

func (b *timedJobs) mark(name string, f func() error) error {
	s := mono(b.t)
	err := f()
	e := mono(b.t)
	b.mu.Lock()
	b.spans[name] = span{s, e}
	if err != nil {
		b.errs[name] = err.Error()
	}
	b.mu.Unlock()
	return err
}

// readback is the approved guard, fail-closed: an error stops the attempt before its custody effect, as
// approvedJobs.work does. It also binds the node to this host: its readback boot id must equal ours.
func (b *timedJobs) readback(ctx context.Context, name string) error {
	if !b.verify {
		return nil
	}
	return b.mark(name, func() error {
		r, err := b.node.VerifyDeployment(ctx, b.policy)
		if err == nil && r.BootID != b.boot {
			err = errors.New("readback boot is not this host's: the harness measures one host only")
		}
		return err
	})
}

func (b *timedJobs) Enroll(ctx context.Context, id string, a RunAdmission, s JobSpec) (i ProcessIdentity, err error) {
	if err = b.readback(ctx, "enroll_verify"); err != nil {
		return i, errors.Join(ErrJobNotStarted, err) // approvedJobs.Enroll: a failed work check never starts the job
	}
	err = b.mark("enroll_custody", func() error { i, err = b.SSHJobs.Enroll(ctx, id, a, s); return err })
	if err == nil && i.BootID != b.boot {
		err = errors.New("enrolled on another boot: the harness measures one host only")
	}
	return i, err
}

func (b *timedJobs) Authorize(ctx context.Context, id string, i ProcessIdentity) error {
	if err := b.readback(ctx, "authorize_verify"); err != nil {
		return err // approvedJobs.Authorize: no authorization after a failed work check
	}
	return b.mark("authorize_custody", func() error { return b.SSHJobs.Authorize(ctx, id, i) })
}

func (b *timedJobs) Wait(ctx context.Context, id string) (e ExitInfo, err error) {
	err = b.mark("wait", func() error { e, err = b.SSHJobs.Wait(ctx, id); return err })
	return e, err
}

func TestSoakClockDecompose(t *testing.T) {
	path := os.Getenv("S0B_CLOCK_DECOMPOSE")
	if path == "" {
		t.Skip("measurement harness; set S0B_CLOCK_DECOMPOSE to a config file")
	}
	var c decomposeConfig
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &c) != nil || c.Runs <= 0 {
		t.Fatalf("config: %v", err)
	}
	var policy infra.DeploymentPolicy
	if raw, err = os.ReadFile(c.Policy); err != nil || json.Unmarshal(raw, &policy) != nil {
		t.Fatalf("policy: %v", err)
	}
	node := &infra.Node{Host: c.Host, User: c.User, HostKey: c.HostKey, KeyFile: c.KeyFile}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.OpenFile(c.Out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := out.Close(); err != nil {
			t.Errorf("rows file close: %v", err)
		}
	})
	enc := json.NewEncoder(out)
	// Provenance first: which host, boot, binary, config and policy produced these rows.
	hostname, _ := os.Hostname()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	prov := map[string]any{"provenance": true, "hostname": hostname, "boot_id": strings.TrimSpace(string(boot)),
		"binary_sha256": fileSHA(t, self), "config_sha256": fileSHA(t, path), "policy_sha256": fileSHA(t, c.Policy), "job_sha256": fileSHA(t, c.Job)}
	if err := enc.Encode(prov); err != nil {
		t.Fatal(err)
	}
	for run := 0; run < c.Runs; run++ {
		measure(t, c, policy, node, strings.TrimSpace(string(boot)), run, enc)
	}
}

func fileSHA(t *testing.T, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// measure runs one job. Its scope is closed on every path, and the row is written before any failure is raised,
// so a failed attempt is kept as evidence but still fails the test.
func measure(t *testing.T, c decomposeConfig, policy infra.DeploymentPolicy, node *infra.Node, boot string, run int, enc *json.Encoder) {
	{
		a := testAdmission()
		a.RunID = "clock-decompose"
		a.OutputDir = c.OutputDir
		a.WorkDeadline = time.Now().Add(2 * time.Minute)
		a.CleanupReserve = 10 * time.Second
		a.HardDeadline = a.WorkDeadline.Add(a.CleanupReserve)
		b := &timedJobs{SSHJobs: &SSHJobs{Nodes: map[string]*infra.Node{"m02": node}, Runner: c.Runner}, t: t, node: node, policy: policy,
			verify: c.Verify, boot: boot, spans: map[string]span{}, errs: map[string]string{}}
		s, err := NewRunScope(t.Context(), a, b, func(context.Context, RunAdmission) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		closed := false
		closeScope := func() error {
			if closed {
				return nil
			}
			closed = true
			return s.Close()
		}
		defer func() {
			if err := closeScope(); err != nil {
				t.Errorf("run %d: scope close: %v", run, err)
			}
		}()
		spec := JobSpec{Node: "m02", Executable: c.Python, SHA256: c.PythonSHA256, Argv: []string{"-I", c.Job}, CaptureOutput: true}
		t1 := mono(t) // runtime.go: t1 immediately before RunApprovedPackJob
		h, err := s.Start(spec)
		var e ExitInfo
		if err == nil {
			e, err = s.Wait(t.Context(), h)
		}
		t4 := mono(t) // runtime.go: t4 immediately after it returns
		row := map[string]any{"run": run, "verify": c.Verify, "t1": t1, "t4": t4, "spans": b.spans, "errors": b.errs}
		if err != nil {
			row["error"] = err.Error()
		} else {
			stdout, _, derr := e.Output.Decode()
			row["code"], row["identity"] = e.Code, e.Identity
			if derr != nil || !json.Valid(stdout) {
				row["error"] = "job output undecodable"
			} else {
				row["job"] = json.RawMessage(stdout)
				if e.Code != 0 {
					row["error"] = "job exit nonzero"
				}
			}
		}
		if cerr := closeScope(); cerr != nil {
			row["close_error"] = cerr.Error()
		}
		if eerr := enc.Encode(row); eerr != nil {
			t.Fatalf("run %d: row not recorded: %v", run, eerr)
		}
		if row["error"] != nil || row["close_error"] != nil {
			t.Fatalf("run %d failed: %v %v", run, row["error"], row["close_error"])
		}
	}
}
