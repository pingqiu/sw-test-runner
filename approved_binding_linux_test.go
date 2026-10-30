//go:build linux

package testrunner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestApprovedBindingRefusesEachForeignDimension(t *testing.T) {
	changes := map[string]func(*RunBinding){
		"run": func(b *RunBinding) { b.RunID = "other" }, "request": func(b *RunBinding) { b.RequestID = "other" },
		"workload": func(b *RunBinding) { b.Workload = "s3" }, "transport": func(b *RunBinding) { b.Transport = "http" },
		"inventory": func(b *RunBinding) { b.Inventory.SHA256 = string(make([]byte, 64)) }, "scenario": func(b *RunBinding) { b.Scenario.Path += "other" },
		"policy": func(b *RunBinding) { b.Policy.Path += "other" }, "artifacts": func(b *RunBinding) { b.Artifacts.Path += "other" },
		"host": func(b *RunBinding) { b.Hosts[0].Host = "foreign" }, "root": func(b *RunBinding) { b.OutputDir = "/foreign" },
		"key": func(b *RunBinding) { b.Hosts[0].HostKey = "foreign" }, "lease": func(b *RunBinding) { b.LeaseID = "ffffffffffffffffffffffffffffffff" },
		"owner": func(b *RunBinding) { b.LeaseOwner = "other" }, "resource": func(b *RunBinding) { b.LeaseResource = "other" },
		"work-deadline": func(b *RunBinding) { b.WorkDeadline = b.WorkDeadline.Add(time.Hour) }, "hard-deadline": func(b *RunBinding) { b.HardDeadline = b.HardDeadline.Add(time.Hour) },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			f := newApprovedFixture(t)
			p, e := ReadTrustedPolicy(f.profile, uint32(os.Getuid()))
			if e != nil {
				t.Fatal(e)
			}
			change(&f.binding)
			raw, _ := json.Marshal(ClaimedRequest{Version: "s0b-request-v1", Binding: f.binding})
			if e = os.WriteFile(f.request, raw, 0600); e != nil {
				t.Fatal(e)
			}
			run, e := OpenApprovedRun(context.Background(), f.request, p, &f.runtime)
			if e == nil {
				run.Close()
				t.Fatal("foreign request admitted")
			}
			if f.node.authorized != 0 {
				t.Fatal("foreign request executed")
			}
		})
	}
}

func TestApprovedExecutionRefusesPostAdmissionAuthorityLoss(t *testing.T) {
	for _, loss := range []string{"lease-release", "lease-stale", "approval-permissions", "approval-symlink"} {
		t.Run(loss, func(t *testing.T) {
			f := newApprovedFixture(t)
			p, e := ReadTrustedPolicy(f.profile, uint32(os.Getuid()))
			if e != nil {
				t.Fatal(e)
			}
			run, e := OpenApprovedRun(context.Background(), f.request, p, &f.runtime)
			if e != nil {
				t.Fatal(e)
			}
			defer run.Close()
			switch loss {
			case "lease-release":
				e = os.WriteFile(filepath.Join(f.dir, "ledger", f.binding.LeaseID+"-release.json"), []byte("{}"), 0600)
			case "lease-stale":
				raw, _ := json.Marshal(map[string]any{"lease_id": f.binding.LeaseID, "agent": "codex03", "renewed": float64(time.Now().Unix() - 601)})
				e = os.WriteFile(filepath.Join(f.dir, "ledger", f.binding.LeaseID+"-renew.json"), raw, 0600)
			case "approval-permissions":
				e = os.Chmod(filepath.Join(f.dir, "go.json"), 0666)
			case "approval-symlink":
				e = os.Rename(filepath.Join(f.dir, "go.json"), filepath.Join(f.dir, "other-go.json"))
				if e == nil {
					e = os.Symlink("other-go.json", filepath.Join(f.dir, "go.json"))
				}
			}
			if e != nil {
				t.Fatal(e)
			}
			if _, e = run.Scope().Start(JobSpec{Node: f.job.Node, Executable: f.job.Executable, SHA256: f.job.SHA256, Argv: f.job.Argv}); e == nil || f.node.authorized != 0 {
				t.Fatalf("unverified authority executed: %v count=%d", e, f.node.authorized)
			}
		})
	}
}
