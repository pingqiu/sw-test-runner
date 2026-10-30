//go:build linux

package infra

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// CPU-only encrypted loopback SSH. Read commands run as UID 1000, while the
// test-created installation is owned by root. No lab/service/device access.
func deploymentFixture(t *testing.T) (*Node, DeploymentPolicy) {
	return deploymentFixtureGate(t, nil)
}

func deploymentFixtureGate(t *testing.T, gate func()) (*Node, DeploymentPolicy) {
	return deploymentFixtureCommand(t, gate, nil)
}

func deploymentFixtureCommand(t *testing.T, gate func(), rewrite func(string) string) (*Node, DeploymentPolicy) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("local Linux root needed to model a protected installation and unprivileged SSH child")
	}
	root, err := os.MkdirTemp("/opt", "codex03-readback-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) }) // Exact test-owned MkdirTemp root.
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	policy := DeploymentPolicy{NodeID: "fake", InstallRoot: root, WorkloadUID: 1000, RequireNative: true, RequireInterpreter: true}
	for _, file := range []struct {
		name, value string
		roles       []string
		mode        os.FileMode
	}{
		{"bin/runner", "approved runner", []string{"runner", "adapter"}, 0555},
		{"config.json", "approved configuration", []string{"config"}, 0444},
		{"bin/weed", "approved product", []string{"product"}, 0555},
		{"native/provider.so", "approved native", []string{"native"}, 0444},
	} {
		path := filepath.Join(root, file.name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(file.value), file.mode); err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256([]byte(file.value))
		policy.Files = append(policy.Files, DeploymentFile{Path: path, SHA256: hex.EncodeToString(hash[:]), Size: int64(len(file.value)), Roles: file.roles})
	}
	interpreter, err := os.ReadFile("/usr/bin/env")
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(interpreter)
	policy.Files = append(policy.Files, DeploymentFile{Path: "/usr/bin/env", SHA256: hex.EncodeToString(hash[:]), Size: int64(len(interpreter)), Roles: []string{"interpreter"}})
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyfile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyfile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), 0600); err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(20 * time.Second))
				server, channels, requests, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				defer server.Close()
				connectionCtx, connectionCancel := context.WithCancel(t.Context())
				defer connectionCancel()
				go func() { server.Wait(); connectionCancel() }()
				go ssh.DiscardRequests(requests)
				for request := range channels {
					channel, incoming, err := request.Accept()
					if err != nil {
						return
					}
					for request := range incoming {
						if request.Type != "exec" {
							request.Reply(false, nil)
							continue
						}
						var payload struct{ Command string }
						if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
							return
						}
						request.Reply(true, nil)
						if gate != nil {
							gate()
						}
						if rewrite != nil {
							payload.Command = rewrite(payload.Command)
						}
						ctx, cancel := context.WithTimeout(connectionCtx, 15*time.Second)
						cmd := exec.CommandContext(ctx, "/bin/sh", "-c", payload.Command)
						cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: &syscall.Credential{Uid: 1000, Gid: 1000}}
						cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
						cmd.WaitDelay = time.Second
						cmd.Stdout, cmd.Stderr = channel, channel.Stderr()
						err := cmd.Run()
						cancel()
						code := uint32(0)
						if err != nil {
							code = 1
						}
						channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{code}))
						channel.Close()
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { listener.Close(); <-stopped; workers.Wait() })
	node := &Node{Host: listener.Addr().String(), User: "fake", KeyFile: keyfile, HostKey: string(ssh.MarshalAuthorizedKey(signer.PublicKey()))}
	policy.Host, policy.User, policy.HostKey = node.Host, node.User, node.HostKey
	return node, policy
}

func TestDeploymentReadbackSnapshotsApprovedPolicyBeforeSSH(t *testing.T) {
	seen, allow := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(allow) })
	node, policy := deploymentFixtureGate(t, func() { close(seen); <-allow })
	type result struct {
		receipt DeploymentReceipt
		err     error
	}
	done := make(chan result, 1)
	go func() { receipt, err := node.VerifyDeployment(t.Context(), policy); done <- result{receipt, err} }()
	select {
	case <-seen:
	case <-time.After(2 * time.Second):
		t.Fatal("probe was not dispatched")
	}
	policy.Files[0].SHA256 = strings.Repeat("1", 64)
	policy.Files[0].Roles[0] = "foreign-role"
	policy.Files[3].Roles[0] = "runner"
	once.Do(func() { close(allow) })
	select {
	case got := <-done:
		if got.err != nil || got.receipt.Files[policy.Files[0].Path] == policy.Files[0].SHA256 {
			t.Fatalf("readback used caller-mutated policy: %v", got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("probe did not finish")
	}
}

func TestDeploymentReadbackRefusesReplacedRoleBytes(t *testing.T) {
	for _, name := range []string{"bin/runner", "config.json", "bin/weed", "native/provider.so"} {
		t.Run(name, func(t *testing.T) {
			node, policy := deploymentFixture(t)
			file := filepath.Join(policy.InstallRoot, name)
			body, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			body[0] ^= 1 // Same size/mode; only byte verification distinguishes it.
			if err := os.WriteFile(file, body, 0444); err != nil {
				t.Fatal(err)
			}
			receipt, err := node.VerifyDeployment(t.Context(), policy)
			if err == nil || !strings.Contains(err.Error(), "digest mismatch") || receipt.Transcript == "" {
				t.Fatalf("foreign %s accepted or refused for wrong reason: %v", name, err)
			}
		})
	}
}

func TestDeploymentReadbackRefusesUnprotectedOrForeignInstallation(t *testing.T) {
	for _, test := range []struct {
		name, reason string
		change       func(string) error
	}{
		{"extra-native", "extra entries", func(root string) error {
			return os.WriteFile(filepath.Join(root, "native/foreign.so"), []byte("extra"), 0444)
		}},
		{"writable-file", "unprotected", func(root string) error { return os.Chmod(filepath.Join(root, "config.json"), 0666) }},
		{"writable-root", "unprotected", func(root string) error { return os.Chmod(root, 0777) }},
		{"workload-owned", "foreign-owned", func(root string) error { return os.Chown(filepath.Join(root, "config.json"), 1000, 1000) }},

		{"setuid-runner", "unprotected", func(root string) error { return os.Chmod(filepath.Join(root, "bin/runner"), 0555|os.ModeSetuid) }},
		{"capability-runner", "capability", func(root string) error {
			return exec.Command("/usr/sbin/setcap", "cap_dac_override=ep", filepath.Join(root, "bin/runner")).Run()
		}},
		{"symlink", "symlink", func(root string) error {
			file := filepath.Join(root, "native/provider.so")
			if err := os.Remove(file); err != nil {
				return err
			}
			return os.Symlink("/usr/bin/env", file)
		}},
		{"missing-native", "did not complete", func(root string) error { return os.Remove(filepath.Join(root, "native/provider.so")) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			node, policy := deploymentFixture(t)
			if err := test.change(policy.InstallRoot); err != nil {
				t.Fatal(err)
			}
			_, err := node.VerifyDeployment(t.Context(), policy)
			if err == nil || !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("%s: want %q refusal, got %v", test.name, test.reason, err)
			}
		})
	}
}

func TestDeploymentReadbackBindsBootIdentityAndRequiredNativeSet(t *testing.T) {
	node, policy := deploymentFixture(t)
	first, err := node.VerifyDeployment(t.Context(), policy)
	if err != nil {
		t.Fatal(err)
	}
	policy.ExpectedBootID = first.BootID
	if _, err := node.VerifyDeployment(t.Context(), policy); err != nil {
		t.Fatal(err)
	}
	policy.ExpectedBootID = "00000000-0000-0000-0000-000000000001"
	if _, err := node.VerifyDeployment(t.Context(), policy); err == nil || !strings.Contains(err.Error(), "boot or workload") {
		t.Fatalf("wrong boot accepted: %v", err)
	}
	policy.ExpectedBootID = ""
	files := policy.Files[:0]
	for _, file := range policy.Files {
		if len(file.Roles) == 1 && file.Roles[0] == "native" {
			continue
		}
		files = append(files, file)
	}
	policy.Files = files
	if err := os.RemoveAll(filepath.Join(policy.InstallRoot, "native")); err != nil {
		t.Fatal(err)
	}
	receipt, err := node.VerifyDeployment(t.Context(), policy)
	if err == nil || !strings.Contains(err.Error(), "set is empty") || receipt.Transcript != "" {
		t.Fatalf("empty native policy reached probe: %v", err)
	}
}

func TestDeploymentReadbackBindsApprovedSSHIdentityAndInterpreter(t *testing.T) {
	for _, change := range []string{"host", "user", "host-key", "native-mode", "interpreter"} {
		t.Run(change, func(t *testing.T) {
			node, policy := deploymentFixture(t)
			switch change {
			case "host":
				policy.Host = "127.0.0.1:1"
			case "user":
				policy.User = "another-user"
			case "host-key":
				policy.HostKey = "another-key"
			case "native-mode":
				node.IsNative = true
			case "interpreter":
				policy.Files[len(policy.Files)-1].SHA256 = strings.Repeat("1", 64)
			}
			receipt, err := node.VerifyDeployment(t.Context(), policy)
			if err == nil {
				t.Fatal("foreign deployment identity accepted")
			}
			if change != "interpreter" && receipt.Transcript != "" {
				t.Fatal("foreign identity reached probe")
			}
			if change == "interpreter" && !strings.Contains(err.Error(), "digest mismatch") {
				t.Fatalf("wrong interpreter refusal: %v", err)
			}
		})
	}
}

func TestDeploymentReadbackCancellationRefusesBeforeQualification(t *testing.T) {
	node, policy := deploymentFixture(t)
	file := filepath.Join(policy.InstallRoot, "native/provider.so")
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(file, 0444); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := node.VerifyDeployment(ctx, policy)
	if err == nil || ctx.Err() == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("readback outlived admission: %v", err)
	}
}

func TestDeploymentReadbackVerifiesActualProtectedFilesThroughPinnedSSH(t *testing.T) {
	node, policy := deploymentFixture(t)
	receipt, err := node.VerifyDeployment(t.Context(), policy)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.BootID == "" || len(receipt.Files) != 5 || receipt.WorkloadUID != 1000 || receipt.ReadEnd.Before(receipt.ReadStart) {
		t.Fatalf("incomplete deployed readback: %+v", receipt)
	}
	if _, err := node.VerifyDeployment(t.Context(), policy); err != nil {
		t.Fatalf("fresh independent recheck: %v", err)
	}
}

// Barrier after genuine find output models an independent privileged installation.
func TestDeploymentReadbackRefusesEntryAddedAfterEnumeration(t *testing.T) {
	scratch, mkErr := os.MkdirTemp("/opt", "codex01-readback-barrier-")
	if mkErr != nil {
		t.Fatal(mkErr)
	}
	t.Cleanup(func() { os.RemoveAll(scratch) })
	if err := os.Chmod(scratch, 0777); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(scratch, "find-barrier")
	marker, resume := filepath.Join(scratch, "enumerated"), filepath.Join(scratch, "resume")
	script := "#!/bin/sh\n/usr/bin/find \"$@\" || exit $?\nprintf yes > '" + marker + "'\nwhile [ ! -f '" + resume + "' ]; do /usr/bin/sleep 0.01; done\n"
	if err := os.WriteFile(wrapper, []byte(script), 0555); err != nil {
		t.Fatal(err)
	}
	node, policy := deploymentFixtureCommand(t, nil, func(command string) string {
		return strings.ReplaceAll(command, "/usr/bin/find", wrapper)
	})
	added := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(marker); err == nil {
				err := os.WriteFile(filepath.Join(policy.InstallRoot, "native/extra.so"), []byte("late unapproved native"), 0444)
				if err == nil {
					err = os.WriteFile(resume, []byte("continue"), 0444)
				}
				added <- err
				return
			}
			time.Sleep(time.Millisecond)
		}
		added <- os.ErrDeadlineExceeded
	}()
	receipt, err := node.VerifyDeployment(t.Context(), policy)
	if addErr := <-added; addErr != nil {
		t.Fatal(addErr)
	}
	if err == nil {
		t.Fatalf("qualified despite an unapproved extra file present before BEFORE metadata; receipt files=%d", len(receipt.Files))
	}
	if err.Error() != "deployment changed during readback" {
		t.Fatalf("wrong refusal cause: %v", err)
	}
}
