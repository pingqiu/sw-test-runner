//go:build linux

package testrunner

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQueueWorkerRetiresApprovedConfigurationBeforeClaim(t *testing.T) {
	for _, name := range []string{"TESTOPS_APPROVED_ADAPTER", "TESTOPS_APPROVED_ADAPTER_SHA256", "TESTOPS_APPROVED_POLICY", "all"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			queue := filepath.Join(root, "queue")
			os.Mkdir(queue, 0700)
			marker := filepath.Join(root, "sourced")
			request := filepath.Join(queue, "r.env")
			payload := "touch '" + marker + "'\n"
			os.WriteFile(request, []byte(payload), 0600)
			vars := map[string]string{"TESTOPS_QUEUE_DIR": queue, "TESTOPS_STATE_DIR": filepath.Join(root, "state"), "TESTOPS_LOG_DIR": filepath.Join(root, "logs"), "TESTOPS_STATUS_DIR": filepath.Join(root, "status"), "TESTOPS_LOCK_FILE": filepath.Join(root, "lock"), "TESTOPS_RDMA_CI_SCRIPT": "/bin/true", "TESTOPS_NOTIFY_CMD": ":"}
			for _, k := range []string{"TESTOPS_APPROVED_ADAPTER", "TESTOPS_APPROVED_ADAPTER_SHA256", "TESTOPS_APPROVED_POLICY"} {
				if name == k || name == "all" {
					vars[k] = "retired"
				}
			}
			out, err := runQueueWorker(t, vars)
			if err == nil || !strings.Contains(string(out), "retired") {
				t.Fatalf("old configuration did not refuse: %v %s", err, out)
			}
			if b, e := os.ReadFile(request); e != nil || string(b) != payload {
				t.Fatal("request claimed or changed", e)
			}
			if _, e := os.Stat(marker); !os.IsNotExist(e) {
				t.Fatal("retired configuration sourced request")
			}
		})
	}
}

func TestQueueWorkerKeepsOrdinaryClaimLockAndResults(t *testing.T) {
	for _, rc := range []string{"0", "23"} {
		t.Run(rc, func(t *testing.T) {
			root := t.TempDir()
			queue := filepath.Join(root, "queue")
			os.Mkdir(queue, 0700)
			state := filepath.Join(root, "state")
			os.WriteFile(filepath.Join(queue, "r.env"), []byte("TESTOPS_MONO_REF=expected-ref\nTESTOPS_RUN_BY=ordinary-worker\n"), 0600)
			hook := filepath.Join(root, "run.sh")
			os.WriteFile(hook, []byte(`#!/bin/sh
[ "$TESTOPS_MONO_REF" = expected-ref ] || exit 91
if flock -n "$TESTOPS_LOCK_FILE" -c true; then exit 92; fi
echo 'bundle: ordinary-bundle'
exit `+rc+"\n"), 0700)
			out, err := runQueueWorker(t, map[string]string{"TESTOPS_QUEUE_DIR": queue, "TESTOPS_STATE_DIR": state, "TESTOPS_LOG_DIR": filepath.Join(root, "logs"), "TESTOPS_STATUS_DIR": filepath.Join(root, "status"), "TESTOPS_LOCK_FILE": filepath.Join(root, "lock"), "TESTOPS_RDMA_CI_SCRIPT": hook, "TESTOPS_NOTIFY_CMD": ":"})
			if err != nil {
				t.Fatalf("worker failed: %v %s", err, out)
			}
			phase, want := "done", "pass"
			if rc != "0" {
				phase, want = "failed", "fail"
			}
			if _, e := os.Stat(filepath.Join(state, phase, "r.env")); e != nil {
				t.Fatal("request transition", e, string(out))
			}
			b, e := os.ReadFile(filepath.Join(root, "status", "r.json"))
			if e != nil {
				t.Fatal(e)
			}
			var status map[string]any
			if json.Unmarshal(b, &status) != nil || status["state"] != want || status["bundle_dir"] != "ordinary-bundle" || status["mono_ref"] != "expected-ref" || status["run_by"] != "ordinary-worker" {
				t.Fatalf("lost result metadata: %s", b)
			}
		})
	}
}

func runQueueWorker(t *testing.T, vars map[string]string) ([]byte, error) {
	t.Helper()
	script, e := filepath.Abs("scripts/testops-ci-worker.sh")
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", script, "--once")
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "TESTOPS_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	for k, v := range vars {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	return cmd.CombinedOutput()
}
