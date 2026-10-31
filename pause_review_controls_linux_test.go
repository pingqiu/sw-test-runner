//go:build linux

package testrunner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/pingqiu/sw-test-runner/infra"
	"testing"
	"time"
)

// Frozen field order and types from 6594164:approved_entry.go. Compare the
// actual emitted file, preserving all runtime values rather than deleting fields.
func assertParentAuthorityBytes(t *testing.T, raw []byte) {
	t.Helper()
	var parent struct {
		Generations map[string]GenerationRestartReceipt
		Request     ClaimedRequest
		GO          ApprovedGO
		Profile     FilePin
		GOFile      FilePin
		Readbacks   map[string]json.RawMessage
		Faults      map[string]FaultReceipt
		Lease       LeaseObservation
		Jobs        map[string]ApprovedJobReceipt
	}
	if err := json.Unmarshal(raw, &parent); err != nil {
		t.Fatal(err)
	}
	expected, err := json.MarshalIndent(parent, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, append(expected, '\n')) {
		t.Fatalf("no-pause authority bytes differ from parent schema: %s", raw)
	}
}

func TestPauseNoPauseParentWireGolden(t *testing.T) {
	request := custodyRequest{Sequence: 7, Operation: "status", Identity: ProcessIdentity{Node: "old", PID: 42}}
	reply := custodyReply{Sequence: 7, Error: "original", Identity: request.Identity}
	inputs := PackInputs{Scenario: []byte("original"), Jobs: map[string]JobPin{"old": {Node: "host"}}}
	parentRequest := struct {
		Sequence  uint64
		Operation string
		Admission RunAdmission
		Spec      JobSpec
		Identity  ProcessIdentity
	}{request.Sequence, request.Operation, request.Admission, request.Spec, request.Identity}
	parentReply := struct {
		EffectDeadline interface{}
		Sequence       uint64
		Error          string
		Identity       ProcessIdentity
		Exit           ExitInfo
		Kill           KillWitness
	}{reply.EffectDeadline, reply.Sequence, reply.Error, reply.Identity, reply.Exit, reply.Kill}
	parentInputs := struct {
		Scenario []byte
		Binding  RunBinding
		Jobs     map[string]JobPin
		Restarts map[string]GenerationRestartPlan
		Boots    map[string]string
		Scripts  map[string][]byte
		Files    map[string]map[string]string
	}{inputs.Scenario, inputs.Binding, inputs.Jobs, inputs.Restarts, inputs.Boots, inputs.Scripts, inputs.Files}
	for _, pair := range [][2]any{{request, parentRequest}, {reply, parentReply}, {inputs, parentInputs}} {
		got, e := json.Marshal(pair[0])
		if e != nil {
			t.Fatal(e)
		}
		want, e := json.Marshal(pair[1])
		if e != nil {
			t.Fatal(e)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("wire bytes changed: got %s want %s", got, want)
		}
	}
}

func TestPauseReadbackFailuresCannotRelease(t *testing.T) {
	for _, phase := range []string{"CONT", "final"} {
		for _, mode := range []string{"boot", "bytes", "read-error", "panic"} {
			t.Run(phase+"/"+mode, func(t *testing.T) {
				f, n := pauseFixture(t)
				read := f.runtime.Readback
				hits := 0
				f.runtime.Readback = func(ctx context.Context, p infra.DeploymentPolicy) (infra.DeploymentReceipt, error) {
					r, err := read(ctx, p)
					n.mu.Lock()
					selected := (phase == "CONT" && n.state == "STOPPED") || (phase == "final" && n.resumeCalls > 0)
					n.mu.Unlock()
					if selected {
						hits++
						switch mode {
						case "boot":
							r.BootID = "foreign-boot"
						case "bytes":
							for k := range r.Files {
								r.Files[k] = "foreign"
								break
							}
						case "read-error":
							return r, errors.New("protected readback lost")
						case "panic":
							panic("readback panic")
						}
					}
					return r, err
				}
				run, result := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
					h, _, err := a.RunApprovedPackJob(ctx, "volume0", true)
					if err != nil {
						return err
					}
					_, err = a.PauseApprovedPackGeneration(ctx, "freeze", h)
					return err
				})
				if hits == 0 || result.Status != StatusFail || run.resourcesSettled() {
					t.Fatalf("readback failure hidden: hits=%d result=%s settled=%v", hits, result.Status, run.resourcesSettled())
				}
				want := 0
				if phase == "final" {
					want = 1
				}
				if n.resumeCalls != want {
					t.Fatalf("CONT calls=%d want %d", n.resumeCalls, want)
				}
			})
		}
	}
}

func TestPauseEarlyClearFencesAdmissionBeforeJoiningStop(t *testing.T) {
	f, n := pauseFixture(t)
	n.stopEntered, n.stopRelease = make(chan struct{}), make(chan struct{})
	run, _ := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
		victim, _, err := a.RunApprovedPackJob(ctx, "volume0", true)
		if err != nil {
			return err
		}
		applied := make(chan error, 1)
		go func() { _, e := a.PauseApprovedPackGeneration(ctx, "freeze", victim); applied <- e }()
		select {
		case <-n.stopEntered:
		case <-time.After(time.Second):
			return errors.New("STOP not dispatched")
		}
		cleared := make(chan error, 1)
		go func() { cleared <- a.Safety.ClearFault(context.Background(), "process-pause/"+victim.id) }()
		until := time.Now().Add(time.Second)
		closed := false
		for time.Now().Before(until) {
			a.Safety.mu.Lock()
			closed = a.Safety.closed
			a.Safety.mu.Unlock()
			if closed {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if !closed {
			t.Error("admission remained open while terminal clear waited for STOP")
		}
		n.mu.Lock()
		calls := n.resumeCalls
		n.mu.Unlock()
		if calls != 0 {
			t.Error("CONT preceded STOP join")
		}
		close(n.stopRelease)
		<-applied
		select {
		case e := <-cleared:
			return e
		case <-time.After(time.Second):
			return errors.New("clear did not join")
		}
	})
	if n.resumeCalls != 1 || !run.resourcesSettled() {
		t.Fatalf("terminal clear failed: calls=%d settled=%v", n.resumeCalls, run.resourcesSettled())
	}
}
