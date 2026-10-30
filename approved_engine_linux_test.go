//go:build linux

package testrunner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestApprovedEngineUsesOriginalCleanupEnvelope(t *testing.T) {
	f := newApprovedFixture(t)
	p, err := ReadTrustedPolicy(f.profile, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenApprovedRun(context.Background(), f.request, p, &f.runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	run.scenario.Phases[1].Always = true
	r := NewRegistry()
	seen := 0
	r.RegisterFunc("probe", TierCore, func(ctx context.Context, a *ActionContext, _ Action) (map[string]string, error) {
		seen++
		if seen == 2 {
			deadline, ok := ctx.Deadline()
			if !ok || deadline.After(f.binding.HardDeadline) {
				t.Error("always phase extended original hard deadline")
			}
			if _, err := a.Safety.Start(JobSpec{Node: f.job.Node, Executable: f.job.Executable, SHA256: f.job.SHA256, Argv: f.job.Argv}); err == nil {
				t.Error("always phase reopened work admission")
			}
		}
		return nil, nil
	})
	result := run.Execute(context.Background(), NewEngine(r, nil), &ActionContext{})
	if result.Status != StatusPass || seen != 2 {
		t.Fatalf("%#v seen=%d", result, seen)
	}
}

func TestApprovedEngineCancelsCurrentPhaseOnRevocation(t *testing.T) {
	f := newApprovedFixture(t)
	p, err := ReadTrustedPolicy(f.profile, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenApprovedRun(context.Background(), f.request, p, &f.runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	r := NewRegistry()
	r.RegisterFunc("probe", TierCore, func(ctx context.Context, _ *ActionContext, _ Action) (map[string]string, error) {
		if err := os.WriteFile(filepath.Join(f.dir, "cancel.json"), []byte("cancel"), 0600); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2500 * time.Millisecond):
			t.Error("current phase survived authority revocation")
			return nil, nil
		}
	})
	result := run.Execute(context.Background(), NewEngine(r, nil), &ActionContext{})
	if result.Status != StatusFail {
		t.Fatalf("revoked run passed: %#v", result)
	}
}

func TestApprovedEngineCannotPassUnknownOwnedCleanup(t *testing.T) {
	f := newApprovedFixture(t)
	f.node.unknownExit = true
	p, err := ReadTrustedPolicy(f.profile, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenApprovedRun(context.Background(), f.request, p, &f.runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	r := NewRegistry()
	r.RegisterFunc("probe", TierCore, func(context.Context, *ActionContext, Action) (map[string]string, error) {
		_, err := run.Scope().Start(JobSpec{Node: f.job.Node, Executable: f.job.Executable, SHA256: f.job.SHA256, Argv: f.job.Argv})
		return nil, err
	})
	result := run.Execute(context.Background(), NewEngine(r, nil), &ActionContext{})
	if result.Status != StatusFail || result.Error == "" {
		t.Fatalf("UNKNOWN cleanup passed: %#v", result)
	}
	for _, receipt := range run.jobReceipts() {
		if receipt.State != "UNKNOWN" {
			t.Fatalf("unsettled custody disappeared: %#v", receipt)
		}
	}
}
