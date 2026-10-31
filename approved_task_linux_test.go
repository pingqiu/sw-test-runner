//go:build linux

package testrunner

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func setGenerationConsent(t *testing.T, f *approvedFixture) {
	t.Helper()
	name := filepath.Join(f.dir, "go.json")
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var goValue ApprovedGO
	if err = json.Unmarshal(raw, &goValue); err != nil {
		t.Fatal(err)
	}
	goValue.AllowMutating = true
	raw, _ = json.Marshal(goValue)
	if err = os.WriteFile(name, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var profile PolicyBootstrap
	body, _ := os.ReadFile(f.profile)
	if err = json.Unmarshal(body, &profile); err != nil {
		t.Fatal(err)
	}
	profile.Approval.SHA256 = fmt.Sprintf("%x", sha256.Sum256(raw))
	body, _ = json.Marshal(profile)
	if err = os.WriteFile(f.profile, body, 0600); err != nil {
		t.Fatal(err)
	}
}

type taskNode struct {
	*generationNode
	release      chan struct{}
	once         sync.Once
	taskWaiting  chan struct{}
	taskCode     int
	serviceWaits atomic.Int32
}

func (n *taskNode) Wait(ctx context.Context, id string) (ExitInfo, error) {
	n.mu.Lock()
	spec := n.specs[id]
	cleanup := n.cleanup[id]
	n.mu.Unlock()
	if !spec.CaptureOutput {
		if !cleanup {
			n.serviceWaits.Add(1)
		}
		return n.generationNode.Wait(ctx, id)
	}
	n.once.Do(func() { close(n.taskWaiting) })
	select {
	case <-n.release:
	case <-ctx.Done():
		return ExitInfo{}, ctx.Err()
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	raw := []byte("{\"status\":\"OBSERVED\"}\n")
	return ExitInfo{Identity: n.ids[id], Code: n.taskCode, Settled: true, StdoutPath: spec.StdoutPath, StderrPath: spec.StderrPath, Output: CapturedOutput{Complete: true, StdoutBase64: base64.StdEncoding.EncodeToString(raw), StdoutSHA256: fmt.Sprintf("%x", sha256.Sum256(raw)), StderrSHA256: fmt.Sprintf("%x", sha256.Sum256(nil)), ReadStart: time.Now(), ReadEnd: time.Now()}}, nil
}

func TestApprovedTaskWaitCannotOccupyVictimGate(t *testing.T) {
	f, n := generationFixture(t)
	node := &taskNode{generationNode: n, release: make(chan struct{}), taskWaiting: make(chan struct{})}
	f.runtime.Jobs = node
	f.policy.Jobs["read-load"] = f.job
	f.publish(t, nil)
	// Restore explicit mutation approval after publishing the task pin.
	setGenerationConsent(t, f)
	_, result := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
		old, _, err := a.RunApprovedPackJob(ctx, "volume0", true)
		if err != nil {
			return err
		}
		task, err := a.StartApprovedPackTask(ctx, "read-load")
		if err != nil {
			return err
		}
		done := make(chan error, 1)
		go func() { _, e := a.WaitApprovedPackTask(ctx, task); done <- e }()
		select {
		case <-node.taskWaiting:
		case <-ctx.Done():
			return ctx.Err()
		}
		// The task Wait is blocked. Kill+old Wait must nevertheless own the victim
		// gate and finish; starting a service Wait watcher here would deadlock it.
		_, _, err = a.RestartApprovedPackGeneration(ctx, "volume-once", old)
		close(node.release)
		if waitErr := <-done; err == nil {
			err = waitErr
		}
		return err
	})
	if result.Status != StatusPass {
		t.Fatalf("load waiter blocked victim restart: %+v", result)
	}
}

func TestApprovedTaskWaitRefusesServiceBeforeBackendWait(t *testing.T) {
	f, n := generationFixture(t)
	node := &taskNode{generationNode: n}
	f.runtime.Jobs = node
	_, result := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
		service, _, err := a.RunApprovedPackJob(ctx, "volume0", true)
		if err != nil {
			return err
		}
		before := node.serviceWaits.Load()
		_, err = a.WaitApprovedPackTask(ctx, service)
		if err == nil {
			return errors.New("service watcher admitted")
		}
		if node.serviceWaits.Load() != before {
			t.Error("service task waiter reached backend Wait")
		}
		return nil // Refusal must remain sticky even if a caller swallows it.
	})
	if result.Status != StatusFail {
		t.Fatal("service watcher refusal was hidden")
	}
}

func TestApprovedTaskNonzeroExitCannotBeIgnored(t *testing.T) {
	f, n := generationFixture(t)
	node := &taskNode{generationNode: n, release: make(chan struct{}), taskWaiting: make(chan struct{}), taskCode: 2}
	close(node.release)
	f.runtime.Jobs = node
	_, result := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
		h, err := a.StartApprovedPackTask(ctx, "volume0")
		if err != nil {
			return err
		}
		if _, err = a.WaitApprovedPackTask(ctx, h); err == nil {
			t.Error("nonzero finite task accepted")
		}
		return nil
	})
	if result.Status != StatusFail {
		t.Fatal("ignored finite-task failure became PASS")
	}
}
