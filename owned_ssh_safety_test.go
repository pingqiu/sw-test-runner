package testrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/infra"
)

var nativeCustodyTestWorkload func([]string) bool
var approvedWorkerTestEntrypoint func([]string) bool

func TestMain(m *testing.M) {
	if approvedWorkerTestEntrypoint != nil && approvedWorkerTestEntrypoint(os.Args[1:]) {
		os.Exit(0)
	}
	if nativeCustodyTestWorkload != nil && nativeCustodyTestWorkload(os.Args[1:]) {
		os.Exit(0)
	}
	mode := os.Getenv("S0A_TEST_CUSTODIAN")
	if len(os.Args) > 1 && os.Args[1] == "owned-custodian" && (mode == "fake" || mode == "lost-enroll") {
		d, e := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
		for {
			var q custodyRequest
			if d.Decode(&q) != nil {
				os.Exit(0)
			}
			if mode == "lost-enroll" {
				os.Exit(0)
			}
			r := custodyReply{Sequence: q.Sequence}
			if q.Operation == "enroll" {
				r.Identity = ProcessIdentity{Node: q.Spec.Node, RunID: q.Admission.RunID, BootID: "fake-boot", PID: 7, PGID: 7, StartTime: 9, ExecutableSHA256: q.Spec.SHA256}
			}
			if q.Operation == "settle" {
				r.Exit.Settled = true
			}
			if e.Encode(r) != nil {
				os.Exit(1)
			}
		}
	}
	if len(os.Args) > 1 && os.Args[1] == "s0a-empty-workload" {
		for {
			time.Sleep(time.Hour)
		}
	}
	if handled, err := OwnedEntrypoint(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func custodyFixtureRunner(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		git := `C:\Program Files\Git\bin`
		if _, err := os.Stat(filepath.Join(git, "bash.exe")); err != nil {
			t.Fatal("local fixture requires Git Bash")
		}
		t.Setenv("PATH", git+`;C:\Program Files\Git\usr\bin;`+os.Getenv("PATH"))
		exe = "/" + strings.ToLower(exe[:1]) + filepath.ToSlash(exe[2:])
	}
	return exe
}

func TestCustodyCloseKeepsReplyDecoder(t *testing.T) {
	t.Setenv("S0A_TEST_CUSTODIAN", "fake")
	runner := custodyFixtureRunner(t)
	for n := 0; n < 30; n++ {
		a := testAdmission()
		b := &SSHJobs{Nodes: map[string]*infra.Node{"fake": {IsNative: true}}, Runner: runner}
		t.Cleanup(func() {
			for _, c := range b.jobs {
				if c.pipe != nil {
					_ = c.pipe.Close()
				}
			}
		})
		s, err := NewRunScope(t.Context(), a, b, func(context.Context, RunAdmission) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.Start(testJob()); err != nil {
			_ = s.Close()
			t.Fatal(err)
		}
		if err = s.Close(); err != nil {
			t.Errorf("trial %d: cleanup reply lost after work cancellation: %v", n, err)
		}
		b.mu.Lock()
		pending := len(b.jobs)
		b.mu.Unlock()
		if pending != 0 || len(b.slots) != 0 {
			t.Fatal("completed jobs/sessions retained")
		}
	}
}

type blockedCustodyPipe struct {
	entered, closed, returned chan struct{}
	once                      sync.Once
	writes                    atomic.Int32
}

func (p *blockedCustodyPipe) Read([]byte) (int, error) { <-p.closed; return 0, io.EOF }
func (p *blockedCustodyPipe) Write([]byte) (int, error) {
	p.writes.Add(1)
	close(p.entered)
	<-p.closed
	time.Sleep(50 * time.Millisecond) // Distinguish a join from a scheduler race.
	close(p.returned)
	return 0, io.ErrClosedPipe
}
func (p *blockedCustodyPipe) Close() error { p.once.Do(func() { close(p.closed) }); return nil }

type acceptingCustodyPipe struct{ writes atomic.Int32 }

func (p *acceptingCustodyPipe) Read([]byte) (int, error)    { return 0, io.EOF }
func (p *acceptingCustodyPipe) Write(b []byte) (int, error) { p.writes.Add(1); return len(b), nil }
func (p *acceptingCustodyPipe) Close() error                { return nil }

func TestCustodyPrecancelledCallNeverWritesAndLeavesChannelUsable(t *testing.T) {
	p := &acceptingCustodyPipe{}
	c := &custodyChannel{pipe: p, replies: make(chan custodyReply, 1)}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.call(ctx, custodyRequest{Operation: "authorize"}); err == nil {
		t.Fatal("cancelled authorization accepted")
	}
	if p.writes.Load() != 0 {
		t.Fatal("already-cancelled call encoded authorization")
	}
	c.replies <- custodyReply{Sequence: 1}
	live, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	if _, err := c.call(live, custodyRequest{Operation: "settle"}); err != nil || p.writes.Load() != 1 {
		t.Fatalf("pre-cancel poisoned an unused channel: %v", err)
	}
}

func TestCustodyTimedOutWriteSettlesAndPoisonsChannel(t *testing.T) {
	p := &blockedCustodyPipe{entered: make(chan struct{}), closed: make(chan struct{}), returned: make(chan struct{})}
	defer func() { _ = p.Close(); <-p.returned }()
	c := &custodyChannel{pipe: p, replies: make(chan custodyReply, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.call(ctx, custodyRequest{Operation: "authorize"}); done <- err }()
	select {
	case <-p.entered:
	case <-time.After(time.Second):
		t.Fatal("write not entered")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled write did not settle")
	}
	select {
	case <-p.returned:
	default:
		t.Fatal("encoder survived its cancelled call")
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if _, err := c.call(ctx2, custodyRequest{Operation: "authorize"}); err == nil {
		t.Fatal("poisoned channel reused")
	}
	if p.writes.Load() != 1 {
		t.Fatal("another writer reached a poisoned channel")
	}
}

func TestCustodyEnrollmentBeforeDispatchDoesNotPoisonCleanup(t *testing.T) {
	for _, which := range []string{"unknown-node", "setup", "slot-wait"} {
		t.Run(which, func(t *testing.T) {
			b := &SSHJobs{Nodes: map[string]*infra.Node{}, Runner: "/not-used"}
			if which != "unknown-node" {
				b.Nodes["fake"] = &infra.Node{KeyFile: filepath.Join(t.TempDir(), "missing-key")}
			}
			if which == "slot-wait" {
				b.jobs, b.slots = map[string]*custodyChannel{}, make(chan struct{}, 8)
				for i := 0; i < 8; i++ {
					b.slots <- struct{}{}
				}
			}
			s, err := NewRunScope(t.Context(), testAdmission(), b, func(context.Context, RunAdmission) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
			defer cancel()
			if _, err := s.start(ctx, testJob(), false); err == nil {
				t.Fatal("failed enrollment was accepted")
			}
			if err := s.Close(); err != nil {
				t.Fatalf("never-dispatched job poisoned cleanup: %v", err)
			}
			if len(s.jobs) != 0 || len(b.jobs) != 0 {
				t.Fatal("never-dispatched entry retained")
			}
			wantSlots := 0
			if which == "slot-wait" {
				wantSlots = 8
			}
			if len(b.slots) != wantSlots {
				t.Fatal("unowned slot released or owned slot leaked")
			}
			if err := b.Settle(t.Context(), "not-a-known-job"); err == nil {
				t.Fatal("arbitrary unknown ID treated as not started")
			}
		})
	}
}

func TestCustodySettlementRequiresPositiveProofBeforePruning(t *testing.T) {
	p := &acceptingCustodyPipe{}
	released := 0
	c := &custodyChannel{pipe: p, replies: make(chan custodyReply, 1), release: func() { released++ }}
	b := &SSHJobs{jobs: map[string]*custodyChannel{"job": c}}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	c.replies <- custodyReply{Sequence: 1, Exit: ExitInfo{Settled: false}}
	if err := b.Settle(ctx, "job"); err == nil || b.jobs["job"] != c || released != 0 {
		t.Fatal("negative settlement discarded custody")
	}
	c.replies <- custodyReply{Sequence: 2, Exit: ExitInfo{Settled: true}}
	if err := b.Settle(ctx, "job"); err != nil || len(b.jobs) != 0 || released != 1 {
		t.Fatalf("positive settlement did not retire custody: %v", err)
	}
}

func TestCustodyNinthJobWaitsWithoutLeakingOrDisturbingEightOwners(t *testing.T) {
	t.Setenv("S0A_TEST_CUSTODIAN", "fake")
	b := &SSHJobs{Nodes: map[string]*infra.Node{"fake": {IsNative: true}}, Runner: custodyFixtureRunner(t)}
	s, err := NewRunScope(t.Context(), testAdmission(), b, func(context.Context, RunAdmission) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for i := 0; i < 8; i++ {
		if _, err := s.Start(testJob()); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := s.start(ctx, testJob(), false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ninth job did not wait for capacity: %v", err)
	}
	if len(b.jobs) != 8 || len(s.jobs) != 8 || len(b.slots) != 8 {
		t.Fatal("waiting job leaked or disturbed admitted owners")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if len(b.jobs) != 0 || len(b.slots) != 0 {
		t.Fatal("owners or slots survived cleanup")
	}
}

func TestCustodyLostEnrollmentReplyRemainsAnUnknownObligation(t *testing.T) {
	t.Setenv("S0A_TEST_CUSTODIAN", "lost-enroll")
	b := &SSHJobs{Nodes: map[string]*infra.Node{"fake": {IsNative: true}}, Runner: custodyFixtureRunner(t)}
	s, err := NewRunScope(t.Context(), testAdmission(), b, func(context.Context, RunAdmission) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.Start(testJob()); err == nil || !strings.Contains(err.Error(), "UNKNOWN") {
		t.Fatalf("lost enrollment reply discarded uncertainty: %v", err)
	}
	if err := s.Close(); err == nil || len(s.jobs) != 1 || len(b.jobs) != 1 {
		t.Fatal("unproven enrollment was retired as NOT_STARTED")
	}
}
