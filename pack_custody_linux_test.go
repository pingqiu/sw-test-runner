//go:build linux

package testrunner

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Actual local custodian/pipe/receipt, not an SSH host or a mock exit.
func TestPackCustodyRetainsIntentionalExitPastActionDeadline(t *testing.T) {
	s, b, spec := nativeCustodyScope(t)
	spec.Argv = []string{"s0a-empty-workload"}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	h, err := s.start(ctx, spec, false)
	if err != nil {
		t.Fatal(err)
	}
	<-ctx.Done() // dispatch deadline must not become background lifetime
	if err := b.Settle(t.Context(), h.id); err != nil {
		t.Fatal(err)
	}
	exit, err := s.Wait(t.Context(), h)
	if err != nil || !exit.CleanupStopped || exit.Code != -1 || exit.Signal != 9 {
		t.Fatalf("lost intentional terminal receipt: %+v %v", exit, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPackCustodyNaturalExitIsNotIntentionalCleanup(t *testing.T) {
	s, b, spec := nativeCustodyScope(t)
	spec.Argv = []string{"s0a-mark", s.admission.OutputDir + "/mark"}
	h, err := s.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		raw, err := os.ReadFile("/proc/" + strconv.Itoa(h.job.identity.PID) + "/stat")
		if err != nil {
			t.Fatal(err)
		}
		// Custodian owns reaping; wait until its child has actually exited.
		if strings.HasPrefix(string(raw[strings.LastIndex(string(raw), ")")+1:]), " Z ") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("local fixture did not exit")
		}
		time.Sleep(time.Millisecond)
	}
	if err := b.Settle(t.Context(), h.id); err != nil {
		t.Fatal(err)
	}
	exit, err := s.Wait(t.Context(), h)
	if err != nil || exit.CleanupStopped || exit.Code != 0 || exit.Signal != 0 {
		t.Fatalf("natural exit relabelled as cleanup: %+v %v", exit, err)
	}
}
