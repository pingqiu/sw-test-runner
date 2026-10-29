//go:build linux

package testrunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/infra"
	"golang.org/x/sys/unix"
)

func init() {
	nativeCustodyTestWorkload = func(args []string) bool {
		if len(args) != 2 {
			return false
		}
		if args[0] == "s0a-mark" {
			if err := os.WriteFile(args[1], []byte("owned"), 0600); err != nil {
				panic(err)
			}
			return true
		}
		if args[0] == "s0a-unmark" {
			if err := os.Remove(args[1]); err != nil {
				panic(err)
			}
			return true
		}
		if args[0] != "s0a-fork-exit" {
			return false
		}
		c := exec.Command(args[1], "s0a-empty-workload")
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := c.Start(); err != nil {
			panic(err)
		}
		fmt.Fprintln(os.Stdout, c.Process.Pid)
		_ = c.Process.Release()
		return true
	}
}

func nativeCustodyScope(t *testing.T, work ...time.Duration) (*RunScope, *SSHJobs, JobSpec) {
	t.Helper()
	t.Setenv("S0A_TEST_CUSTODIAN", "")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	a := testAdmission()
	if len(work) != 0 {
		a.WorkDeadline = time.Now().Add(work[0])
		a.CleanupReserve = 2 * time.Second
		a.HardDeadline = a.WorkDeadline.Add(a.CleanupReserve)
	}
	a.OutputDir = t.TempDir()
	if err := os.Chmod(a.OutputDir, 0700); err != nil {
		t.Fatal(err)
	}
	b := &SSHJobs{Nodes: map[string]*infra.Node{"local": {IsNative: true}}, Runner: exe}
	s, err := NewRunScope(t.Context(), a, b, func(context.Context, RunAdmission) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, b, JobSpec{Node: "local", Executable: exe, SHA256: hex.EncodeToString(hash[:])}
}

func TestCustodyNativeWaitReapsEscapedDescendant(t *testing.T) {
	s, _, spec := nativeCustodyScope(t)
	spec.Argv = []string{"s0a-fork-exit", spec.Executable}
	h, err := s.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	exit, err := s.Wait(t.Context(), h)
	if err != nil || !exit.Settled || exit.Code != 0 {
		t.Fatalf("no native descendant settlement: %+v %v", exit, err)
	}
	data, err := os.ReadFile(exit.StdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err == nil {
		unix.Close(fd)
		t.Fatal("escaped descendant survived settled receipt")
	}
	if err != unix.ESRCH {
		t.Fatal(err)
	}
}

func TestCustodyNativeControllerPipeLossKillsButCannotFabricateReceipt(t *testing.T) {
	s, b, spec := nativeCustodyScope(t)
	spec.Argv = []string{"s0a-empty-workload"}
	h, err := s.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.PidfdOpen(h.job.identity.PID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	b.mu.Lock()
	pipe := b.jobs[h.id].pipe
	b.mu.Unlock()
	if err := pipe.Close(); err != nil {
		t.Fatal(err)
	}
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	if n, err := unix.Poll(poll, 2000); err != nil || n != 1 {
		t.Fatalf("owned leader survived EOF: %d %v", n, err)
	}
	if err := s.Close(); err == nil {
		t.Fatal("lost remote settlement receipt was fabricated")
	}
	if time.Now().After(s.admission.HardDeadline) {
		t.Fatal("test consumed the hard envelope")
	}
}

func TestCustodyNativeFaultRestoresThroughWorkDeadline(t *testing.T) {
	s, _, spec := nativeCustodyScope(t, time.Second)
	file := s.admission.OutputDir + "/owned-fault-marker"
	apply, restore := spec, spec
	apply.Argv, restore.Argv = []string{"s0a-mark", file}, []string{"s0a-unmark", file}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	p := WitnessedFault{Kind: "fill_disk", Node: "local", Resource: file, Before: map[string]string{"marker": "absent"}, During: map[string]string{"marker": "owned"}, Apply: apply, Restore: restore}
	p.Observe = func(context.Context) (FaultObservation, error) {
		state := "absent"
		data, err := os.ReadFile(file)
		if err == nil {
			state = string(data)
		} else if os.IsNotExist(err) {
			err = nil
		}
		return FaultObservation{Node: "local", BootID: strings.TrimSpace(string(boot)), Resource: file, State: map[string]string{"marker": state}}, err
	}
	if err := s.ApplyWitnessedFault(t.Context(), p.Key(), p); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.done:
	case <-time.After(4 * time.Second):
		t.Fatal("automatic cleanup did not finish")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	r := s.FaultReceipts()[p.Key()]
	if r.RestoreExit.Identity.PID == 0 || r.RestoreExit.Code != 0 || r.Restored.State["marker"] != "absent" || r.Restored.ReadStart.Before(s.admission.WorkDeadline) {
		t.Fatalf("no native cleanup-phase restoration proof: %+v", r)
	}
}
