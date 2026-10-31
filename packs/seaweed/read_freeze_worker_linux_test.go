//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"fmt"
	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/infra"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 6 && os.Args[1] == "approved-run" && os.Getenv("FREEZE_CPU_CUSTODIAN") != "" {
		runtime := tr.ApprovedRuntime{Jobs: &tr.SSHJobs{Nodes: map[string]*infra.Node{"m01": {IsNative: true}, "m02": {IsNative: true}}, Runner: os.Getenv("FREEZE_CPU_CUSTODIAN")}, Readback: func(_ context.Context, p infra.DeploymentPolicy) (infra.DeploymentReceipt, error) {
			raw, _ := json.Marshal(p)
			r := infra.DeploymentReceipt{NodeID: p.NodeID, Host: p.Host, User: p.User, HostKey: p.HostKey, BootID: p.ExpectedBootID, WorkloadUID: p.WorkloadUID, ReadStart: time.Now(), ReadEnd: time.Now(), PolicySHA256: hash(raw), Files: map[string]string{}}
			for _, f := range p.Files {
				r.Files[f.Path] = f.SHA256
			}
			return r, nil
		}}
		result, bundle, err := tr.RunApprovedRequest(context.Background(), os.Args[3], os.Args[5], registerFastRestart, &runtime, nil)
		fmt.Printf("bundle: %s\nresult: %+v err: %v\n", bundle, result, err)
		if err != nil || result == nil || result.Status != tr.StatusPass {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestFreezeActualWorkerClaimAndEngine(t *testing.T) {
	f := freezeFixture(t, "positive")
	queue := filepath.Join(f.dir, "queue")
	if err := os.Mkdir(queue, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.request, filepath.Join(queue, filepath.Base(f.request))); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	adapter := filepath.Join(f.dir, "test-adapter")
	if err = os.WriteFile(adapter, bytes, 0700); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("../../scripts/testops-ci-worker.sh")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", script, "--once")
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "TESTOPS_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "FREEZE_CPU_CUSTODIAN="+f.runtime.Jobs.(*tr.SSHJobs).Runner, "TESTOPS_QUEUE_DIR="+queue, "TESTOPS_STATE_DIR="+f.dir, "TESTOPS_LOCK_FILE="+filepath.Join(f.dir, "lab.lock"), "TESTOPS_LOG_DIR="+filepath.Join(f.dir, "logs"), "TESTOPS_STATUS_DIR="+filepath.Join(f.dir, "status"), "TESTOPS_NOTIFY_CMD=:", "TESTOPS_APPROVED_ADAPTER="+adapter, "TESTOPS_APPROVED_ADAPTER_SHA256="+hash(bytes), "TESTOPS_APPROVED_POLICY="+f.profile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("worker %v %s", err, out)
	}
	log, _ := os.ReadFile(filepath.Join(f.dir, "logs", "pack-request.log"))
	if _, err = os.Stat(filepath.Join(f.dir, "done", "pack-request.env")); err != nil {
		t.Fatalf("not cleanly done: %v %s %s", err, out, log)
	}
	if !strings.Contains(string(log), "result:") || !strings.Contains(string(log), "err: <nil>") {
		t.Fatalf("adapter entry not exercised: %s", log)
	}
}
