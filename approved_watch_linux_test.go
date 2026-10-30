//go:build linux

package testrunner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestApprovedRunStopsOwnedWorkOnAuthorityLoss(t *testing.T) {
	f := newApprovedFixture(t)
	policy, err := ReadTrustedPolicy(f.profile, uint32(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	run, err := OpenApprovedRun(context.Background(), f.request, policy, &f.runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	if _, err = run.Scope().Start(JobSpec{Node: f.job.Node, Executable: f.job.Executable, SHA256: f.job.SHA256, Argv: f.job.Argv}); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(f.dir, "go.json")); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(3 * time.Second)
	settled := false
	for time.Now().Before(until) {
		f.node.mu.Lock()
		settled = len(f.node.ids) == 0
		f.node.mu.Unlock()
		if settled {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !settled {
		t.Fatal("authority loss left owned work running without another dispatch")
	}
	if err = run.Close(); err == nil {
		t.Fatal("unreadable work authority must not qualify the run")
	}
}
