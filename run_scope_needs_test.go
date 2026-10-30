package testrunner

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type controlledOwnedNode struct {
	*fakeOwnedNode
	enroll func(context.Context, string, RunAdmission, JobSpec) (ProcessIdentity, error)
	wait   func(context.Context, string) (ExitInfo, error)
}

func (n *controlledOwnedNode) Enroll(ctx context.Context, id string, a RunAdmission, spec JobSpec) (ProcessIdentity, error) {
	return n.enroll(ctx, id, a, spec)
}

func (n *controlledOwnedNode) Wait(ctx context.Context, id string) (ExitInfo, error) {
	return n.wait(ctx, id)
}

func TestNeed1ConcurrentStartsReachEnrollmentTogether(t *testing.T) {
	n := &fakeOwnedNode{}
	s := fakeScope(t, context.Background(), n, testAdmission())
	arrived, release := make(chan struct{}, 2), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	s.backend = &controlledOwnedNode{fakeOwnedNode: n, enroll: func(ctx context.Context, id string, a RunAdmission, spec JobSpec) (ProcessIdentity, error) {
		i, err := n.Enroll(ctx, id, a, spec)
		arrived <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return i, ctx.Err()
		}
		return i, err
	}}
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, err := s.Start(testJob()); results <- err }()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-arrived:
		case <-time.After(300 * time.Millisecond):
			t.Error("sibling enrollment serialized")
		}
	}
	once.Do(func() { close(release) })
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}

func TestNeed2FaultCallbacksMayStartAndStopJobs(t *testing.T) {
	n := &fakeOwnedNode{}
	s := fakeScope(t, context.Background(), n, testAdmission())
	job := make(chan JobHandle, 1)
	started := make(chan error, 1)
	err := s.ApplyFault("restart", func(context.Context) error {
		go func() { h, e := s.Start(testJob()); job <- h; started <- e }()
		select {
		case e := <-started:
			return e
		case <-time.After(300 * time.Millisecond):
			return errors.New("callback cannot reenter Start")
		}
	}, func(ctx context.Context) error { return s.Stop(ctx, <-job) }, func(context.Context) error { return nil })
	if err != nil {
		t.Error(err)
	}
	if err := s.ClearFault(context.Background(), "restart"); err != nil {
		t.Fatal(err)
	}
}

func TestNeed3ProvenOneShotsLeaveNoCloseWork(t *testing.T) {
	n := &fakeOwnedNode{settleDelay: time.Millisecond}
	a := testAdmission()
	a.CleanupReserve = 500 * time.Millisecond
	s := fakeScope(t, context.Background(), n, a)
	for i := 0; i < 1000; i++ {
		h, err := s.Start(testJob())
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Stop(context.Background(), h); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.jobs) != 0 {
		t.Fatalf("retained %d proven jobs", len(s.jobs))
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if len(n.settled) != 1000 {
		t.Fatalf("Close resettled proven jobs: %d", len(n.settled))
	}
}

func TestNeed4WaitReturnsExitAndBoundLogPathsOnce(t *testing.T) {
	n := &fakeOwnedNode{exitCode: 7}
	s := fakeScope(t, context.Background(), n, testAdmission())
	h, err := s.Start(testJob())
	if err != nil {
		t.Fatal(err)
	}
	i, err := s.Wait(context.Background(), h)
	if err != nil || i.Code != 7 || !i.Settled || i.StdoutPath != h.job.spec.StdoutPath || i.StderrPath != h.job.spec.StderrPath {
		t.Fatalf("bad exit receipt: %+v %v", i, err)
	}
	if _, err := s.Wait(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if n.waited != 1 || len(n.settled) != 0 || len(s.jobs) != 0 {
		t.Fatal("proven Wait did not retire exactly once")
	}
}

func TestNeed4UnprovenWaitRetainsCustody(t *testing.T) {
	for _, kind := range []string{"unsettled", "identity", "path"} {
		t.Run(kind, func(t *testing.T) {
			n := &fakeOwnedNode{waitUnsettled: kind == "unsettled", waitWrongIdentity: kind == "identity", waitWrongPath: kind == "path"}
			s := fakeScope(t, context.Background(), n, testAdmission())
			h, err := s.Start(testJob())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Wait(context.Background(), h); err == nil || len(s.jobs) != 1 {
				t.Fatal("unproven Wait retired custody")
			}
		})
	}
	n := &fakeOwnedNode{}
	s := fakeScope(t, context.Background(), n, testAdmission())
	spec := testJob()
	spec.StdoutPath = "/run/fake/../other/out"
	if _, err := s.Start(spec); err == nil || len(n.ids) != 0 {
		t.Fatal("unbound log path reached backend")
	}
	spec.StdoutPath = "/run/fake/once.stdout"
	spec.StderrPath = "/run/fake/once.stderr"
	h, err := s.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(spec); err == nil {
		t.Fatal("completed job's evidence can be overwritten")
	}
}

func TestNeed4SignalExitKeepsNativeStatus(t *testing.T) {
	for _, tc := range []struct {
		name         string
		code, signal int
		valid        bool
	}{
		{"signaled", -1, 9, true}, {"missing-signal", -1, 0, false},
		{"contradictory", 0, 9, false}, {"invalid-code", 256, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &fakeOwnedNode{exitCode: tc.code, exitSignal: tc.signal}
			s := fakeScope(t, context.Background(), n, testAdmission())
			h, err := s.Start(testJob())
			if err != nil {
				t.Fatal(err)
			}
			i, err := s.Wait(context.Background(), h)
			if (err == nil) != tc.valid || (tc.valid && (i.Code != -1 || i.Signal != 9)) {
				t.Fatalf("changed native terminal status: %+v %v", i, err)
			}
		})
	}
}

func TestNeed5KillWitnessDoesNotRetireAndRejectsDeadOrReplaced(t *testing.T) {
	for _, kind := range []string{"live", "dead", "replaced"} {
		t.Run(kind, func(t *testing.T) {
			n := &fakeOwnedNode{}
			s := fakeScope(t, context.Background(), n, testAdmission())
			h, err := s.Start(testJob())
			if err != nil {
				t.Fatal(err)
			}
			n.killNotAlive = kind == "dead"
			n.replaced = kind == "replaced"
			w, err := s.Kill(context.Background(), h)
			if kind == "live" {
				if err != nil || !w.AliveBefore || w.Identity != h.job.identity || w.Signal != "KILL" || w.AppliedAt.IsZero() {
					t.Fatalf("bad kill witness: %+v %v", w, err)
				}
			} else if err == nil || n.killed != 0 {
				t.Fatal("dead/replaced victim credited or signaled")
			}
			if len(s.jobs) != 1 {
				t.Fatal("signal mistaken for absence proof")
			}
		})
	}
}

func TestNeed5LostEnrollmentCannotIssueKill(t *testing.T) {
	n := &fakeOwnedNode{lostReply: true}
	s := fakeScope(t, context.Background(), n, testAdmission())
	h, err := s.Start(testJob())
	if err == nil {
		t.Fatal("expected unknown enrollment")
	}
	if _, err := s.Kill(context.Background(), h); err == nil || n.killed != 0 {
		t.Fatal("unconfirmed identity reached Kill")
	}
}

func TestNeedCloseDuringEnrollNeverAuthorizesLateJob(t *testing.T) {
	n := &fakeOwnedNode{}
	s := fakeScope(t, context.Background(), n, testAdmission())
	arrived, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	s.backend = &controlledOwnedNode{fakeOwnedNode: n, enroll: func(ctx context.Context, id string, a RunAdmission, spec JobSpec) (ProcessIdentity, error) {
		i, err := n.Enroll(ctx, id, a, spec)
		close(arrived)
		<-release
		return i, err
	}}
	started, closed := make(chan error, 1), make(chan error, 1)
	go func() { _, err := s.Start(testJob()); started <- err }()
	<-arrived
	go func() { closed <- s.Close() }()
	select {
	case <-s.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("Close could not close admission")
	}
	once.Do(func() { close(release) })
	if err := <-started; err == nil {
		t.Fatal("late job authorized")
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if n.authorized != 0 || len(n.ids) != 0 {
		t.Fatal("late issuer escaped settlement")
	}
}

func TestNeedCloseCancelsOutstandingWaitBeforeSettlement(t *testing.T) {
	n := &fakeOwnedNode{}
	s := fakeScope(t, context.Background(), n, testAdmission())
	arrived := make(chan struct{})
	s.backend = &controlledOwnedNode{fakeOwnedNode: n, enroll: n.Enroll, wait: func(ctx context.Context, _ string) (ExitInfo, error) {
		close(arrived)
		<-ctx.Done()
		return ExitInfo{}, ctx.Err()
	}}
	h, err := s.Start(testJob())
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { _, err := s.Wait(context.Background(), h); waited <- err }()
	<-arrived
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-waited; err == nil || len(s.jobs) != 0 {
		t.Fatal("wait not cancelled/settled")
	}
}
