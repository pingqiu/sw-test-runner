//go:build linux

package testrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Local real custodian, real pidfd and real multithreaded Go victim. No SSH/lab claim.
func TestProcessPauseCustody(t *testing.T) {
	for _, mode := range []string{"roundtrip", "wrong-pid", "wrong-start", "expired", "past-hard", "dead", "replay"} {
		t.Run(mode, func(t *testing.T) {
			s, b, spec := nativeCustodyScope(t)
			spec.Argv = []string{"s0a-empty-workload"}
			h, err := s.Start(spec)
			if err != nil {
				t.Fatal(err)
			}
			i, deadline := h.job.identity, s.admission.HardDeadline
			// Match the already-ready service prerequisite: let the test Go runtime start its threads.
			time.Sleep(100 * time.Millisecond)
			switch mode {
			case "wrong-pid":
				i.PID++
			case "wrong-start":
				i.StartTime++
			case "expired":
				deadline = time.Now().Add(-time.Second)
			case "past-hard":
				deadline = deadline.Add(time.Second)
			case "dead":
				if _, err = s.Kill(t.Context(), h); err != nil {
					t.Fatal(err)
				}
				if _, err = s.Wait(t.Context(), h); err != nil {
					t.Fatal(err)
				}
			}
			// STOP must bind its exact retained resume obligation before the signal;
			// no separate preparation round trip or arbitrary signal is accepted.
			stopped, err := testPauseCommand(b, t.Context(), h.id, "pause-stop", i, deadline)
			if mode != "roundtrip" && mode != "replay" {
				if err == nil {
					t.Fatalf("%s accepted: %+v", mode, stopped)
				}
				return
			}
			if err != nil || stopped.State != "STOPPED" || stopped.Before != "LIVE" || stopped.Identity != i {
				diagnostic, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				exit, waitErr := b.Wait(diagnostic, h.id)
				out, _ := os.ReadFile(h.job.spec.StdoutPath)
				stderr, _ := os.ReadFile(h.job.spec.StderrPath)
				t.Fatalf("STOP: %+v %v terminal=%+v wait=%v stdout=%q stderr=%q", stopped, err, exit, waitErr, out, stderr)
			}
			if state, err := processTaskState(fmt.Sprintf("/proc/%d/task", i.PID)); err != nil || state != "STOPPED" {
				t.Fatalf("not all tasks stopped: %s %v", state, err)
			}
			if mode == "replay" {
				if _, err = testPauseCommand(b, t.Context(), h.id, "pause-stop", i, deadline); err == nil {
					t.Fatal("replayed STOP accepted")
				}
			}
			resumed, err := testPauseCommand(b, t.Context(), h.id, "pause-resume", i, deadline)
			if err != nil || resumed.State != "LIVE" || resumed.Identity != i {
				t.Fatalf("CONT: %+v %v", resumed, err)
			}
			time.Sleep(100 * time.Millisecond)
			if state, err := processTaskState(fmt.Sprintf("/proc/%d/task", i.PID)); err != nil || state != "LIVE" {
				t.Fatalf("not all tasks resumed: %s %v", state, err)
			}
			if _, err = testPauseCommand(b, t.Context(), h.id, "pause-resume", i, deadline); err == nil {
				t.Fatal("replayed CONT accepted")
			}
		})
	}
}

type pauseOrderAuthority struct{}

func (pauseOrderAuthority) bindFault(context.Context, string, WitnessedFault, FaultObservation) error {
	return nil
}
func (pauseOrderAuthority) bindFaultJob(string, string, bool, JobSpec) error { return nil }
func (pauseOrderAuthority) faultDeadline(string) (time.Time, error) {
	return time.Now().Add(time.Second), nil
}
func (pauseOrderAuthority) qualifyRestoration(context.Context, string) error { return nil }

func TestPauseNoVictimKeepsExistingCloseOrder(t *testing.T) {
	for _, authority := range []bool{false, true} {
		t.Run(fmt.Sprint(authority), func(t *testing.T) {
			s, _, spec := nativeCustodyScope(t)
			if authority {
				s.authority = pauseOrderAuthority{}
			}
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
			observed := false
			if err = s.ApplyFault("ordinary", func(context.Context) error { return nil }, func(context.Context) error {
				poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
				n, e := unix.Poll(poll, 0)
				if e != nil {
					return e
				}
				if (n != 0) != authority {
					return fmt.Errorf("ordinary restoration order changed: dead=%v authority=%v", n != 0, authority)
				}
				observed = true
				return nil
			}, func(context.Context) error {
				if !observed {
					return fmt.Errorf("missing restore")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type pauseNode struct {
	*generationNode
	state                    string
	mode                     string
	stopEntered, stopRelease chan struct{}
	stopCheck                func()
	resumeCalls              int
}

func (n *pauseNode) pauseCommand(ctx context.Context, id, op string, i ProcessIdentity, deadline time.Time) (ProcessPauseWitness, error) {
	if op == "pause-stop" && n.stopEntered != nil {
		close(n.stopEntered)
		<-n.stopRelease // Deliberately model an issuer which outlives cancellation.
	}
	if op == "pause-stop" && n.stopCheck != nil {
		n.stopCheck()
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ids[id] != i || n.cleanup[id] || !deadline.After(time.Now()) {
		return ProcessPauseWitness{}, fmt.Errorf("foreign/dead/expired generation")
	}
	w := ProcessPauseWitness{Identity: i, ObservedAt: time.Now(), State: "LIVE"}
	switch op {
	case "pause-stop":
		n.state = "STOPPED"
		w.Before = "LIVE"
		if n.mode == "baseline" {
			w.Before = ""
		}
		w.State = "STOPPED"
		if n.mode == "lost-stop" {
			return w, fmt.Errorf("lost STOP response")
		}
		if n.mode == "mixed" {
			w.State = "MIXED"
		}
		if n.mode == "missing" {
			w.ObservedAt = time.Time{}
		}
		if n.mode == "pid" {
			w.Identity.PID++
		}
		if n.mode == "start" {
			w.Identity.StartTime++
		}
	case "pause-resume":
		n.resumeCalls++
		if n.mode == "cont-failure" && n.resumeCalls == 1 {
			return w, fmt.Errorf("CONT failed")
		}
		// A settled OTHER job has already had its receipt retired from ids.
		if n.mode == "barrier" && len(n.ids) != 1 {
			return w, fmt.Errorf("other issuers still live")
		}
		n.state = "LIVE"
	case "pause-observe":
		if n.mode == "final-foreign" {
			w.Identity.StartTime++
		}
		w.State = n.state
	default:
		return w, fmt.Errorf("unexpected command")
	}
	n.events = append(n.events, op)
	return w, nil
}
func pauseFixture(t *testing.T) (*approvedFixture, *pauseNode) {
	f, g := generationFixture(t)
	f.policy.GenerationRestarts = nil
	f.policy.ProcessPause = &ProcessPausePlan{Name: "freeze", Victim: "volume0", Deadline: f.binding.HardDeadline}
	f.publish(t, nil)
	setGenerationConsent(t, f)
	n := &pauseNode{generationNode: g, state: "LIVE"}
	f.runtime.Jobs = n
	return f, n
}

func TestApprovedPauseCustodyAndRetainedCleanup(t *testing.T) {
	for _, mode := range []string{"pass", "barrier", "lost-stop", "late-proof", "baseline", "mixed", "missing", "pid", "start", "revoked", "unreadable", "cont-failure", "final-foreign", "expired", "foreign-label", "no-consent"} {
		t.Run(mode, func(t *testing.T) {
			f, n := pauseFixture(t)
			n.mode = mode
			var pauseErr error
			var old JobHandle
			run, result := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
				var err error
				label := "volume0"
				if mode == "foreign-label" {
					label = "volume1"
				}
				old, _, err = a.RunApprovedPackJob(ctx, label, true)
				if err != nil {
					return err
				}
				if mode == "barrier" {
					if _, _, err = a.RunApprovedPackJob(ctx, "volume1", true); err != nil {
						return err
					}
				}
				if mode == "expired" {
					a.approved.runPolicy.ProcessPause = &ProcessPausePlan{Name: "freeze", Victim: "volume0", Deadline: time.Now().Add(-time.Second)}
				}
				if mode == "no-consent" {
					a.approved.approval.AllowMutating = false
				}
				step, cancelStep := context.WithCancel(ctx)
				n.stopCheck = func() {
					priorCheck := true
					a.Safety.mu.Lock()
					priorCheck = a.Safety.pause != nil && a.Safety.restores[a.Safety.pause.Key] != nil && a.Safety.pause.Identity == old.job.identity
					a.Safety.mu.Unlock()
					if !priorCheck {
						t.Error("resume obligation missing before STOP")
					}
					if mode == "late-proof" {
						cancelStep()
						time.Sleep(10 * time.Millisecond)
					}
				}
				_, pauseErr = a.PauseApprovedPackGeneration(step, "freeze", old)
				cancelStep()
				if mode == "revoked" {
					if err = os.WriteFile(filepath.Join(f.dir, "go.json"), []byte("{}"), 0600); err != nil {
						return err
					}
				}
				if mode == "unreadable" {
					if err = os.Remove(filepath.Join(f.dir, "go.json")); err != nil {
						return err
					}
				}
				if mode == "cont-failure" {
					if err = a.Safety.ClearFault(ctx, "process-pause/"+old.id); err == nil {
						return fmt.Errorf("CONT failure hidden")
					}
				}
				if mode == "revoked" || mode == "unreadable" {
					_, _, err = a.RunApprovedPackJob(ctx, "volume1", true)
					if err == nil {
						return fmt.Errorf("revoked WORK still admitted")
					}
					return err
				}
				return pauseErr
			})
			if mode == "pass" || mode == "barrier" {
				if result.Status != StatusPass {
					t.Fatalf("%s: %s close=%v", mode, result.Status, run.Close())
				}
			} else if result.Status != StatusFail {
				t.Fatalf("%s produced %s", mode, result.Status)
			}
			receipt := run.ProcessPauseReceipt()
			if mode == "pass" || mode == "barrier" || mode == "lost-stop" || mode == "late-proof" || mode == "baseline" || mode == "mixed" || mode == "missing" || mode == "pid" || mode == "start" || mode == "revoked" || mode == "unreadable" {
				if receipt.Final.Identity != old.job.identity || receipt.Final.State != "LIVE" || n.resumeCalls != 1 {
					t.Fatalf("no exact retained cleanup after %s: %+v calls=%d", mode, receipt, n.resumeCalls)
				}
				if !run.resourcesSettled() {
					t.Fatal("proven resumed/settled resources retained UNKNOWN")
				}
			}
			if mode == "cont-failure" || mode == "final-foreign" || mode == "expired" {
				if run.resourcesSettled() {
					t.Fatal("failed/unverifiable CONT released")
				}
			}
			if mode == "cont-failure" && n.resumeCalls != 1 {
				t.Fatal("Close revived failed CONT by retry")
			}
			if mode == "foreign-label" || mode == "no-consent" {
				if n.resumeCalls != 0 || n.state != "LIVE" {
					t.Fatal("unapproved fault reached custody")
				}
			}
		})
	}
}

func TestPauseWitnessTaskSets(t *testing.T) {
	for _, states := range []string{"TTT", "RSDI", "TR", "t", "Z", ""} {
		t.Run(states, func(t *testing.T) {
			root := t.TempDir()
			for j, state := range states {
				task := filepath.Join(root, fmt.Sprint(j+1))
				if err := os.Mkdir(task, 0700); err != nil {
					t.Fatal(err)
				}
				fields := []string{string(state)}
				for len(fields) < 20 {
					fields = append(fields, "1")
				}
				if err := os.WriteFile(filepath.Join(task, "stat"), []byte("1 (space ) name) "+strings.Join(fields, " ")), 0600); err != nil {
					t.Fatal(err)
				}
			}
			state, err := processTaskState(root)
			if states == "TTT" {
				if err != nil || state != "STOPPED" {
					t.Fatalf("%s %v", state, err)
				}
			} else if states == "RSDI" {
				if err != nil || state != "LIVE" {
					t.Fatalf("%s %v", state, err)
				}
			} else if err == nil {
				t.Fatalf("incomplete/mixed/unknown task set credited: %s", state)
			}
		})
	}
}

func TestPauseAbsentWireFieldsUnchanged(t *testing.T) {
	for _, v := range []any{custodyRequest{}, custodyReply{}, PackInputs{}} {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "Pause") {
			t.Fatalf("absent pause changed existing wire bytes: %s", raw)
		}
	}
}

func TestPauseCloseJoinsDelayedStopBeforeResume(t *testing.T) {
	f, n := pauseFixture(t)
	n.stopEntered, n.stopRelease = make(chan struct{}), make(chan struct{})
	run, _ := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
		h, _, err := a.RunApprovedPackJob(ctx, "volume0", true)
		if err != nil {
			return err
		}
		if _, _, err = a.RunApprovedPackJob(ctx, "volume1", true); err != nil {
			return err
		}
		applied := make(chan error, 1)
		go func() { _, err := a.PauseApprovedPackGeneration(ctx, "freeze", h); applied <- err }()
		select {
		case <-n.stopEntered:
		case <-time.After(time.Second):
			return fmt.Errorf("STOP never dispatched")
		}
		closed := make(chan error, 1)
		go func() { closed <- a.Safety.Close() }()
		select {
		case err := <-closed:
			return fmt.Errorf("Close escaped active STOP issuer: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		n.mu.Lock()
		early := n.resumeCalls
		earlySettlement := len(n.cleanup)
		n.mu.Unlock()
		if early != 0 || earlySettlement != 0 {
			t.Error("CONT or other work settlement preceded STOP issuer join")
		}
		close(n.stopRelease)
		select {
		case <-applied:
		case <-time.After(time.Second):
			return fmt.Errorf("apply did not join")
		}
		select {
		case err = <-closed:
		case <-time.After(time.Second):
			return fmt.Errorf("Close did not join")
		}
		return err
	})
	if n.resumeCalls != 1 || n.state != "LIVE" || !run.resourcesSettled() {
		t.Fatalf("delayed STOP escaped restoration: calls=%d state=%s settled=%v", n.resumeCalls, n.state, run.resourcesSettled())
	}
}

func TestPauseCustodianRejectsExpiredEffectEnvelope(t *testing.T) {
	for _, op := range []string{"pause-stop", "pause-resume"} {
		t.Run(op, func(t *testing.T) {
			s, b, spec := nativeCustodyScope(t)
			spec.Argv = []string{"s0a-empty-workload"}
			h, err := s.Start(spec)
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(100 * time.Millisecond)
			if op == "pause-resume" {
				if _, err = testPauseCommand(b, t.Context(), h.id, "pause-stop", h.job.identity, s.admission.HardDeadline); err != nil {
					t.Fatal(err)
				}
			}
			b.mu.Lock()
			ch := b.jobs[h.id]
			b.mu.Unlock()
			_, err = ch.call(t.Context(), custodyRequest{Operation: op, Identity: h.job.identity, PauseDeadline: s.admission.HardDeadline, Admission: RunAdmission{EffectDeadline: time.Now().Add(-time.Second)}})
			if err == nil {
				t.Fatal("expired action envelope still signaled victim")
			}
			want := "LIVE"
			if op == "pause-resume" {
				want = "STOPPED"
			}
			state, err := processTaskState(fmt.Sprintf("/proc/%d/task", h.job.identity.PID))
			if err != nil || state != want {
				t.Fatalf("expired request changed victim: state=%s want=%s err=%v", state, want, err)
			}
		})
	}
}

func testPauseCommand(b *SSHJobs, ctx context.Context, id, op string, i ProcessIdentity, deadline time.Time) (ProcessPauseWitness, error) {
	r, e := b.command(ctx, id, op, i, deadline)
	return r.Pause, e
}
func (n *pauseNode) command(ctx context.Context, id, op string, i ProcessIdentity, deadline ...time.Time) (custodyReply, error) {
	if len(deadline) != 1 {
		return custodyReply{}, fmt.Errorf("missing exact pause deadline")
	}
	w, e := n.pauseCommand(ctx, id, op, i, deadline[0])
	return custodyReply{Pause: w}, e
}

// The subprocess runs the real custodian; only STOP's already-produced reply
// is delayed. Enrollment, signaling, retained grant, sequencing and CONT are real.
type delayedPauseReply struct{ io.Writer }

func (w delayedPauseReply) Write(raw []byte) (int, error) {
	var r custodyReply
	if json.Unmarshal(raw, &r) == nil && r.Pause.State == "STOPPED" {
		time.Sleep(time.Second)
	}
	return w.Writer.Write(raw)
}
func init() {
	pauseCustodyTestEntrypoint = func(args []string) bool {
		if len(args) != 1 || args[0] != "owned-custodian" || os.Getenv("S0A_TEST_CUSTODIAN") != "delay-pause-reply" {
			return false
		}
		if err := serveCustody(os.Stdin, delayedPauseReply{os.Stdout}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return true
	}
}
func TestPauseRealCustodyLostStopReplyJoinsBeforeCont(t *testing.T) {
	s, b, spec := nativeCustodyScope(t, 4*time.Second)
	t.Setenv("S0A_TEST_CUSTODIAN", "delay-pause-reply")
	spec.Argv = []string{"s0a-empty-workload"}
	h, err := s.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	limited, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	_, err = testPauseCommand(b, limited, h.id, "pause-stop", h.job.identity, s.admission.HardDeadline)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected lost STOP reply, got %v", err)
	}
	state, err := processTaskState(fmt.Sprintf("/proc/%d/task", h.job.identity.PID))
	if err != nil || state != "STOPPED" {
		t.Fatalf("fault was not active after lost reply: %s %v", state, err)
	}
	cleanup, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	resumed, err := testPauseCommand(b, cleanup, h.id, "pause-resume", h.job.identity, s.admission.HardDeadline)
	if err != nil || resumed.State != "LIVE" || resumed.Identity != h.job.identity {
		t.Fatalf("no exact CONT after old reply joined: %+v %v", resumed, err)
	}
	final, err := testPauseCommand(b, cleanup, h.id, "pause-observe", h.job.identity, s.admission.HardDeadline)
	if err != nil || final.State != "LIVE" || final.Identity != resumed.Identity {
		t.Fatalf("late STOP or missing final proof: %+v %v", final, err)
	}
}
