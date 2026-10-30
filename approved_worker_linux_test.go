//go:build linux

package testrunner

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the real claim/flock/dispatch/result entry, not a parallel parser.
func TestApprovedWorkerNeverSourcesSubmittedShell(t *testing.T) {
	root := t.TempDir()
	queue, state := filepath.Join(root, "queue"), filepath.Join(root, "state")
	if err := os.Mkdir(queue, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "submitted-shell-ran")
	payload := "TESTOPS_MONO_REF=$(touch '" + marker + "')\n"
	if err := os.WriteFile(filepath.Join(queue, "request.env"), []byte(payload), 0600); err != nil {
		t.Fatal(err)
	}
	adapter, err := exec.LookPath("false")
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile(adapter)
	if err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(root, "trusted-policy.json")
	if err := os.WriteFile(policy, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("scripts/testops-ci-worker.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script, "--once")
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "TESTOPS_") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env,
		"TESTOPS_QUEUE_DIR="+queue, "TESTOPS_STATE_DIR="+state,
		"TESTOPS_LOCK_FILE="+filepath.Join(root, "lab.lock"),
		"TESTOPS_LOG_DIR="+filepath.Join(root, "logs"),
		"TESTOPS_STATUS_DIR="+filepath.Join(root, "status"),
		"TESTOPS_RDMA_CI_SCRIPT=/bin/true", "TESTOPS_NOTIFY_CMD=:",
		"TESTOPS_APPROVED_ADAPTER="+adapter,
		fmt.Sprintf("TESTOPS_APPROVED_ADAPTER_SHA256=%x", sha256.Sum256(bytes)),
		"TESTOPS_APPROVED_POLICY="+policy,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("worker entry failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("submitted request shell executed before admission; stat=%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(state, "failed", "request.env")); err != nil {
		t.Fatalf("refusal must preserve the failed request: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(state, "done", "request.env")); !os.IsNotExist(err) {
		t.Fatalf("refused request reported done: %v\n%s", err, out)
	}
}
