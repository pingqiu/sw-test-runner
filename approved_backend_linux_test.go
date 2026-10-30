//go:build linux

package testrunner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

type changedAfterEnroll struct {
	OwnedJobBackend
	change func()
}

func (b changedAfterEnroll) Enroll(ctx context.Context, id string, a RunAdmission, s JobSpec) (ProcessIdentity, error) {
	i, err := b.OwnedJobBackend.Enroll(ctx, id, a, s)
	if err == nil {
		b.change()
	}
	return i, err
}

func TestApprovedRunRechecksBeforeExecution(t *testing.T) {
	for _, when := range []string{"before-enroll", "before-authorize"} {
		t.Run(when, func(t *testing.T) {
			f := newApprovedFixture(t)
			change := func() {
				if err := os.WriteFile(filepath.Join(f.dir, "scenario.yaml"), []byte("name: drifted\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if when == "before-authorize" {
				f.runtime.Jobs = changedAfterEnroll{f.node, change}
			}
			policy, err := ReadTrustedPolicy(f.profile, uint32(os.Getuid()))
			if err != nil {
				t.Fatal(err)
			}
			run, err := OpenApprovedRun(context.Background(), f.request, policy, &f.runtime)
			if err != nil {
				t.Fatal(err)
			}
			if when == "before-enroll" {
				change()
			}
			_, err = run.Scope().Start(JobSpec{Node: f.job.Node, Executable: f.job.Executable, SHA256: f.job.SHA256, Argv: f.job.Argv})
			if err == nil || f.node.authorized != 0 {
				t.Fatalf("changed input authorized execution: err=%v authorized=%d", err, f.node.authorized)
			}
			_ = run.Close()
			f.node.mu.Lock()
			defer f.node.mu.Unlock()
			if len(f.node.ids) != 0 {
				t.Fatal("inert child was abandoned on authority refusal")
			}
		})
	}
}
