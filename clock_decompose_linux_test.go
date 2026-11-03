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
	"encoding/json"
	"os"
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

func (b *timedJobs) readback(name string) {
	if !b.verify {
		return
	}
	_ = b.mark(name, func() error { _, err := b.node.VerifyDeployment(context.Background(), b.policy); return err })
}

func (b *timedJobs) Enroll(ctx context.Context, id string, a RunAdmission, s JobSpec) (i ProcessIdentity, err error) {
	b.readback("enroll_verify")
	err = b.mark("enroll_custody", func() error { i, err = b.SSHJobs.Enroll(ctx, id, a, s); return err })
	return i, err
}

func (b *timedJobs) Authorize(ctx context.Context, id string, i ProcessIdentity) error {
	b.readback("authorize_verify")
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
	out, err := os.OpenFile(c.Out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	enc := json.NewEncoder(out)
	for run := 0; run < c.Runs; run++ {
		a := testAdmission()
		a.RunID = "clock-decompose"
		a.OutputDir = c.OutputDir
		a.WorkDeadline = time.Now().Add(2 * time.Minute)
		a.CleanupReserve = 10 * time.Second
		a.HardDeadline = a.WorkDeadline.Add(a.CleanupReserve)
		b := &timedJobs{SSHJobs: &SSHJobs{Nodes: map[string]*infra.Node{"m02": node}, Runner: c.Runner}, t: t, node: node, policy: policy,
			verify: c.Verify, spans: map[string]span{}, errs: map[string]string{}}
		s, err := NewRunScope(t.Context(), a, b, func(context.Context, RunAdmission) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
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
			row["code"], row["job"] = e.Code, json.RawMessage(stdout)
			if derr != nil || json.Valid(stdout) == false {
				row["error"] = "job output undecodable"
			}
		}
		if err := enc.Encode(row); err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
	}
}
