//go:build linux

package infra

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func disposeECLocal(t *testing.T, base, target, evidence string) {
	t.Helper()
	var killed []map[string]any
	for round := 0; round < 100; round++ {
		entries, _ := os.ReadDir("/proc")
		left := 0
		for _, entry := range entries {
			var pid int
			if _, e := fmt.Sscan(entry.Name(), &pid); e != nil || pid == os.Getpid() {
				continue
			}
			prefix := filepath.Join("/proc", entry.Name())
			exe, _ := os.Readlink(filepath.Join(prefix, "exe"))
			cmd, _ := os.ReadFile(filepath.Join(prefix, "cmdline"))
			if !strings.Contains(exe, base) && !strings.Contains(exe, target) && !bytes.Contains(cmd, []byte(base)) && !bytes.Contains(cmd, []byte(target)) {
				continue
			}
			left++
			raw, _ := os.ReadFile(filepath.Join(prefix, "stat"))
			killed = append(killed, map[string]any{"pid": pid, "exe": exe, "cmdline": string(cmd), "stat": string(raw)})
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		if left == 0 {
			b, _ := json.MarshalIndent(killed, "", "  ")
			_ = os.WriteFile(filepath.Join(evidence, "disposal.json"), b, 0644)
			if len(killed) > 0 {
				t.Error("owned processes survived worker settlement; forced disposal recorded")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("local replica process teardown did not complete")
}

func TestECLocalStage1(t *testing.T) {
	if os.Getenv("EC_LOCAL_WEED") == "" {
		t.Skip("opt-in real product: scripts/run-ec-stage1.sh")
	}
	if os.Geteuid() != 0 {
		t.Fatal("local protected install requires root")
	}
	evidence := os.Getenv("EC_LOCAL_EVIDENCE")
	source, e := filepath.Abs("..")
	if e != nil {
		t.Fatal(e)
	}
	prepare := filepath.Join(source, "packs/seaweed/local-ec/prepare.py")
	base, e := os.MkdirTemp("/opt", "codex03-ec-full-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	if e = os.Chmod(base, 0755); e != nil {
		t.Fatal(e)
	}
	node, policy := deploymentFixtureSession(t, nil, nil, 600*time.Second, 1001)

	t.Cleanup(func() { disposeECLocal(t, base, policy.InstallRoot, evidence) })
	for _, f := range []string{"bin/runner", "bin/weed", "config.json", "native/provider.so"} {
		if e = os.Remove(filepath.Join(policy.InstallRoot, f)); e != nil {
			t.Fatal(e)
		}
	}
	for _, d := range []string{"bin", "native"} {
		if e = os.Remove(filepath.Join(policy.InstallRoot, d)); e != nil {
			t.Fatal(e)
		}
	}
	key, e := os.ReadFile(node.KeyFile)
	if e != nil {
		t.Fatal(e)
	}
	keyPath := filepath.Join(base, "fixture-key")
	if e = os.WriteFile(keyPath, key, 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.Chown(keyPath, 1001, 1001); e != nil {
		t.Fatal(e)
	}
	contextFile := filepath.Join(base, "context.json")
	b, _ := json.Marshal(map[string]string{"host": node.Host, "host_key": node.HostKey, "key_file": keyPath})
	if e = os.WriteFile(contextFile, b, 0644); e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command("/usr/bin/python3", prepare, source, base, policy.InstallRoot, contextFile)
	output, e := cmd.CombinedOutput()
	if err := os.WriteFile(filepath.Join(evidence, "prepare.log"), output, 0644); err != nil {
		t.Fatal(err)
	}
	if e != nil {
		t.Fatalf("prepare: %v %s", e, output)
	}
	var f struct {
		Run, Lease, Worker, Service, Data, Record string
		Env                                       map[string]string
	}
	b, e = os.ReadFile(filepath.Join(base, "fixture.json"))
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(b, &f); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(evidence, "fixture.json"), b, 0644); e != nil {
		t.Fatal(e)
	}
	worker := exec.Command("/bin/bash", f.Worker, "--once")
	worker.Dir = filepath.Dir(filepath.Dir(f.Worker))
	worker.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + filepath.Join(base, "controller"), "LANG=C"}
	for k, v := range f.Env {
		worker.Env = append(worker.Env, k+"="+v)
	}
	worker.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: &syscall.Credential{Uid: 1001, Gid: 1001}}
	// UMask is inherited; the test runner launches this parent with0077.
	output, e = worker.CombinedOutput()
	_ = os.WriteFile(filepath.Join(evidence, "worker-shell.log"), output, 0644)
	if err := exec.Command("tar", "-czf", filepath.Join(evidence, "target-output.tar.gz"), "-C", f.Data, ".").Run(); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("cp", "-r", f.Service, filepath.Join(evidence, "worker-service")).Run(); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("cp", "-r", f.Record, filepath.Join(evidence, "records")).Run(); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("cp", "-r", filepath.Join(base, "locks/lab.lock.ledger"), filepath.Join(evidence, "ledger")).Run(); err != nil {
		t.Fatal(err)
	}
	var status struct {
		State  string
		Bundle string `json:"bundle_dir"`
	}
	statusRaw, err := os.ReadFile(filepath.Join(f.Service, "state/status", f.Run+".json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(statusRaw, &status)
	var released struct{ Outcome string }
	relRaw, err := os.ReadFile(filepath.Join(base, "locks/lab.lock.ledger", f.Lease+"-release.json"))
	if err == nil {
		_ = json.Unmarshal(relRaw, &released)
	}
	var report map[string]any
	if strings.HasPrefix(status.Bundle, f.Service+"/results/") {
		raw, _ := os.ReadFile(filepath.Join(status.Bundle, "artifacts/ec-stage1.json"))
		_ = json.Unmarshal(raw, &report)
	}
	result := map[string]any{"worker_state": status.State, "shell_error": fmt.Sprint(e), "lease_outcome": released.Outcome, "ec": report, "host_contact": false, "worker_uid": 1001, "umask": "0077"}
	raw, _ := json.MarshalIndent(result, "", "  ")
	_ = os.WriteFile(filepath.Join(evidence, "RESULT.json"), raw, 0644)
	if e != nil || status.State != "pass" || released.Outcome != "RESOURCES_SETTLED" || report["s1"] != "PASS" || report["mandatory_bytes_deadlines"] != "PASS" || report["wrong_byte_control"] != "WRONG_BYTE_REJECTED" || report["s2"] != "NOT_COVERED" || report["exposure"] != "NOT_OBSERVED" {
		t.Fatalf("whole EC stage did not qualify: %s", raw)
	}
	if len(report["losses"].([]any)) != 4 || len(report["reads"].([]any)) != 8 {
		t.Fatal("missing cumulative4loss/8route reads")
	}
	ports := []int{23555, 33555, 23888, 33888, 23833, 33833}
	for i := 0; i < 14; i++ {
		ports = append(ports, 24100+i, 34100+i)
	}
	for _, port := range ports {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			c.Close()
			t.Fatalf("port %d still listening after settlement", port)
		}
	}
	t.Log("LOCAL_EC_STAGE_PASS: seed encode plant S1 both routes S2 four held losses/8routes, RESOURCES_SETTLED; exposure NOT_OBSERVED")
}
