//go:build linux

package testrunner

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/infra"
	"golang.org/x/sys/unix"
)

// Real owned-custodian/owned-exec children, not fabricated service receipts.
// Seventeen retained services plus one leaf is the EC stage's required shape.
func TestCustodyDeclaredServicesRemainLiveWhileLeafRuns(t *testing.T) {
	t.Setenv("S0A_TEST_CUSTODIAN", "")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	policy := ApprovedRunPolicy{Runner: exe, Jobs: map[string]JobPin{}}
	for n := 0; n < 18; n++ {
		policy.Jobs[fmt.Sprint(n)] = JobPin{}
	}
	runtime := approvedSSHRuntime(policy, nil, map[string]*infra.Node{"local": {IsNative: true}})
	a := testAdmission()
	a.OutputDir = t.TempDir()
	if err := os.Chmod(a.OutputDir, 0700); err != nil {
		t.Fatal(err)
	}
	a.WorkDeadline = time.Now().Add(8 * time.Second)
	a.HardDeadline = a.WorkDeadline.Add(2 * time.Second)
	s, err := NewRunScope(t.Context(), a, runtime.Jobs, func(context.Context, RunAdmission) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	spec := JobSpec{Node: "local", Executable: exe, SHA256: fmt.Sprintf("%x", sha256.Sum256(raw))}
	spec.Argv = []string{"s0a-empty-workload"}
	var services []JobHandle
	for n := 0; n < 17; n++ {
		h, err := s.Start(spec)
		if err != nil {
			t.Fatalf("background service %d cannot start with earlier services live: %v", n+1, err)
		}
		services = append(services, h)
	}
	leaf := spec
	leaf.Argv = []string{"s0a-profile-output"}
	h, err := s.Start(leaf)
	if err != nil {
		t.Fatal("leaf cannot run alongside the declared services:", err)
	}
	exit, err := s.Wait(t.Context(), h)
	if err != nil || !exit.Settled || exit.Code != 0 {
		t.Fatalf("leaf did not finish: %+v %v", exit, err)
	}
	for _, service := range services {
		fd, err := unix.PidfdOpen(service.job.identity.PID, 0)
		if err != nil {
			t.Fatal(err)
		}
		n, err := unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, 0)
		unix.Close(fd)
		if err != nil || n != 0 {
			t.Fatalf("service stopped to make room for the leaf: %d %v", n, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, service := range services {
		fd, err := unix.PidfdOpen(service.job.identity.PID, 0)
		if fd >= 0 {
			unix.Close(fd)
		}
		if err != unix.ESRCH {
			t.Fatalf("settled service still exists: %v", err)
		}
	}
}

func TestCustodyDefaultCapacityStillWaitsAndCancels(t *testing.T) {
	s, _, spec := nativeCustodyScope(t, 2*time.Second)
	spec.Argv = []string{"s0a-empty-workload"}
	for n := 0; n < 8; n++ {
		if _, err := s.Start(spec); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Start(spec); !errors.Is(err, ErrJobNotStarted) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("default limit no longer bounds the ninth live service: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
