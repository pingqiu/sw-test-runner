package testrunner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// This node never executes processes or opens network connections.
type fakeOwnedNode struct {
	mu                               sync.Mutex
	ids                              map[string]ProcessIdentity
	settled                          []string
	authorized                       int
	lostReply, replaced, unknownExit bool
	seen                             RunAdmission
	args                             []string
}

func (n *fakeOwnedNode) Enroll(_ context.Context, id string, a RunAdmission, spec JobSpec) (ProcessIdentity, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	i := ProcessIdentity{Node: spec.Node, BootID: "boot", RunID: a.RunID, ExecutableSHA256: spec.SHA256, PID: 41, PGID: 41, StartTime: 9}
	n.ids[id] = i
	n.seen, n.args = a, spec.Argv
	if n.lostReply {
		return ProcessIdentity{}, errors.New("reply lost after inert enrollment")
	}
	return i, nil
}

func (n *fakeOwnedNode) Authorize(_ context.Context, id string, i ProcessIdentity) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.replaced {
		return errors.New("generation changed: no signal/authorization")
	}
	if n.ids[id] != i {
		return errors.New("identity mismatch")
	}
	n.authorized++
	return nil
}

func (n *fakeOwnedNode) Settle(ctx context.Context, id string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.settled = append(n.settled, id)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if n.unknownExit {
		return errors.New("descendant absence UNKNOWN")
	}
	delete(n.ids, id)
	return nil
}

func testAdmission() RunAdmission {
	return RunAdmission{RunID: "run", Workload: "kv", RequestID: "fresh-kv", ManifestSHA256: strings.Repeat("a", 64), WorkDeadline: time.Now().Add(time.Minute), HardDeadline: time.Now().Add(2 * time.Minute), CleanupReserve: time.Second}
}

func fakeScope(t *testing.T, ctx context.Context, n *fakeOwnedNode, a RunAdmission) *RunScope {
	t.Helper()
	n.ids = make(map[string]ProcessIdentity)
	s, err := NewRunScope(ctx, a, n, func(_ context.Context, a RunAdmission) error {
		if a.RequestID != "fresh-"+a.Workload {
			return errors.New("wrong request/workload")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func testJob() JobSpec {
	return JobSpec{Node: "fake", Executable: "/fake/bench", SHA256: strings.Repeat("b", 64), Argv: []string{"", "30", "a'b"}}
}

func TestScopeAuthorityIsFreshAndWorkloadBound(t *testing.T) {
	for _, workload := range []string{"kv", "s3"} {
		n := &fakeOwnedNode{}
		a := testAdmission()
		a.Workload, a.RequestID = workload, "fresh-"+workload
		s := fakeScope(t, context.Background(), n, a)
		if _, err := s.Start(testJob()); err != nil {
			t.Fatal(err)
		}
		if n.seen.RequestID != a.RequestID || n.args[0] != "" || n.args[1] != "30" {
			t.Fatal("lost authority or empty argv")
		}
	}
	a := testAdmission()
	a.Workload = "s3"
	_, err := NewRunScope(context.Background(), a, &fakeOwnedNode{}, func(_ context.Context, a RunAdmission) error {
		if a.RequestID != "fresh-"+a.Workload {
			return errors.New("historical KV request is not S3 authority")
		}
		return nil
	})
	if err == nil {
		t.Fatal("accepted historical KV request as S3")
	}
	for _, change := range []func(*RunAdmission){
		func(a *RunAdmission) { a.ManifestSHA256 = "missing" },
		func(a *RunAdmission) { a.WorkDeadline = time.Now().Add(-time.Second) },
		func(a *RunAdmission) { a.HardDeadline = a.WorkDeadline },
		func(a *RunAdmission) { a.CleanupReserve = 0 },
	} {
		a := testAdmission()
		change(&a)
		if s, err := NewRunScope(context.Background(), a, &fakeOwnedNode{}, func(context.Context, RunAdmission) error { return nil }); err == nil {
			_ = s.Close()
			t.Fatal("accepted invalid envelope")
		}
	}
}

func TestScopeLostEnrollmentReplyRetainsPossibleIssuer(t *testing.T) {
	n := &fakeOwnedNode{lostReply: true}
	s := fakeScope(t, context.Background(), n, testAdmission())
	h, err := s.Start(testJob())
	if err == nil || h.id == "" || len(s.jobs) != 1 || n.authorized != 0 {
		t.Fatal("lost reply discarded or authorized")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if len(n.settled) != 1 || len(n.ids) != 0 {
		t.Fatal("possible issuer escaped cleanup")
	}
}

func TestScopeChangedGenerationNeverAuthorizes(t *testing.T) {
	n := &fakeOwnedNode{replaced: true}
	s := fakeScope(t, context.Background(), n, testAdmission())
	if _, err := s.Start(testJob()); err == nil || n.authorized != 0 {
		t.Fatal("authorized replacement")
	}
}

func TestScopeCancellationClosesAdmissionAndSettlesEveryBench(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	n := &fakeOwnedNode{}
	s := fakeScope(t, ctx, n, testAdmission())
	for i := 0; i < 2; i++ {
		if _, err := s.Start(testJob()); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	select {
	case <-s.done:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not settle")
	}
	if _, err := s.Start(testJob()); err == nil {
		t.Fatal("post-cancel admission")
	}
	if len(n.settled) != 2 || len(n.ids) != 0 {
		t.Fatal("bench escaped")
	}
}

func TestScopeWorkDeadlineSettlesWithoutCaller(t *testing.T) {
	n := &fakeOwnedNode{}
	a := testAdmission()
	a.WorkDeadline = time.Now().Add(30 * time.Millisecond)
	s := fakeScope(t, context.Background(), n, a)
	if _, err := s.Start(testJob()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.done:
	case <-time.After(time.Second):
		t.Fatal("deadline not enforced")
	}
	if len(n.ids) != 0 {
		t.Fatal("deadline orphaned job")
	}
}

func TestScopeForeignHandleAndUnknownSettlement(t *testing.T) {
	n := &fakeOwnedNode{unknownExit: true}
	s := fakeScope(t, context.Background(), n, testAdmission())
	h, _ := s.Start(testJob())
	other := fakeScope(t, context.Background(), &fakeOwnedNode{}, testAdmission())
	if err := other.Stop(context.Background(), h); err == nil {
		t.Fatal("foreign handle accepted")
	}
	if err := s.Close(); err == nil {
		t.Fatal("UNKNOWN was CLEAN")
	}
	if err := s.Close(); err == nil || len(n.settled) != 1 {
		t.Fatal("close not idempotent")
	}
}

func TestScopePartialFaultAndFailedVerificationCannotDisappear(t *testing.T) {
	s := fakeScope(t, context.Background(), &fakeOwnedNode{}, testAdmission())
	restores := 0
	err := s.ApplyFault("fake/qdisc", func(context.Context) error { return errors.New("partial mutation") },
		func(ctx context.Context) error {
			restores++
			if ctx.Err() != nil {
				t.Fatal("inherited cancellation")
			}
			return nil
		},
		func(context.Context) error { return errors.New("still applied") })
	if err == nil {
		t.Fatal("partial fault accepted")
	}
	if err := s.ApplyFault("fake/qdisc", func(context.Context) error { t.Fatal("overwrote restore handle"); return nil }, nil, nil); err == nil {
		t.Fatal("conflict accepted")
	}
	if err := s.Close(); err == nil || restores != 1 || len(s.restores) != 1 {
		t.Fatal("restore acceptance mistaken for proof")
	}
}

func TestScopeCleanupAttemptsAllDespiteFailure(t *testing.T) {
	n := &fakeOwnedNode{unknownExit: true}
	s := fakeScope(t, context.Background(), n, testAdmission())
	for i := 0; i < 2; i++ {
		_, _ = s.Start(testJob())
	}
	called := false
	_ = s.ApplyFault("first", func(context.Context) error { return nil }, func(context.Context) error { called = true; return nil }, func(context.Context) error { return nil })
	_ = s.ApplyFault("last", func(context.Context) error { return nil }, func(context.Context) error { return errors.New("restore failed") }, func(context.Context) error { return nil })
	if err := s.Close(); err == nil || !called || len(n.settled) != 2 {
		t.Fatal("failure skipped obligations")
	}
}

func TestScopeRestorePanicDoesNotSkipJobsAndClearedFaultIsReverified(t *testing.T) {
	n := &fakeOwnedNode{}
	s := fakeScope(t, context.Background(), n, testAdmission())
	_, _ = s.Start(testJob())
	checks, restores := 0, 0
	_ = s.ApplyFault("good", func(context.Context) error { return nil },
		func(context.Context) error { restores++; return nil },
		func(context.Context) error { checks++; return nil })
	if err := s.ClearFault(context.Background(), "good"); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearFault(context.Background(), "good"); err != nil {
		t.Fatal(err)
	}
	_ = s.ApplyFault("bad", func(context.Context) error { return nil },
		func(context.Context) error { panic("restore") }, func(context.Context) error { return nil })
	if err := s.Close(); err == nil || !strings.Contains(err.Error(), "cleanup panic") {
		t.Fatal("panic was CLEAN")
	}
	if restores != 1 || checks != 3 || len(n.settled) != 1 {
		t.Fatal("cleanup skipped or reapplied cleared mutation")
	}
}

func TestScopeEngineCannotPassUnknownOwnedCleanup(t *testing.T) {
	s := fakeScope(t, context.Background(), &fakeOwnedNode{unknownExit: true}, testAdmission())
	_, _ = s.Start(testJob())
	got := NewEngine(NewRegistry(), nil).Run(context.Background(), &Scenario{Name: "scope"}, &ActionContext{Safety: s})
	if got.Status != StatusFail || !strings.Contains(got.Error, "UNKNOWN") {
		t.Fatalf("unknown job passed: %+v", got)
	}
}
