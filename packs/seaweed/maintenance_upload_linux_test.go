//go:build linux

package seaweed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/inventory"
	"golang.org/x/crypto/ssh"
)

// Rehearse the real phased prepare/upload caller before executing the helper.
// The earlier maintenance host double returned init success without requiring
// that prepare had actually delivered the file used by that command.
func TestMaintenancePrepareDeliversHelperToExecutionRoles(t *testing.T) {
	for _, role := range []string{"meta", "volume", "client"} {
		for _, maintenance := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/maintenance=%t", role, maintenance), func(t *testing.T) {
				var mu sync.Mutex
				uploaded := map[string][]byte{}
				address, pin, key, _ := cloudSSH(t, "", func(command string, channel ssh.Channel) uint32 {
					if strings.HasPrefix(command, "cat > ") {
						body, err := io.ReadAll(channel)
						if err != nil {
							return 1
						}
						name := filepath.Base(strings.Trim(strings.TrimPrefix(command, "cat > "), "'"))
						mu.Lock()
						uploaded[name] = body
						mu.Unlock()
					}
					return 0 // No host shell or service is executed by this SSH double.
				})
				var document inventory.Document
				if err := json.Unmarshal(cloudInput(t, address), &document); err != nil {
					t.Fatal(err)
				}
				document.Target = "onprem"
				host := document.Hosts[0]
				host.ID = role
				host.Roles = []string{role}
				if role == "meta" {
					host.Roles = []string{"master", "filer", "s3"}
				}
				host.RunRoot = cloudRunRoot(document, host)
				document.Hosts = []inventory.Host{host}
				config := CloudConfig{SSHKey: key, HostKey: pin, Output: t.TempDir(), Files: map[string]CloudFile{}}
				if maintenance {
					config.MaintenanceTask = "volume.balance"
				}
				for _, name := range []string{"weed", "weed-volume", "native.tar"} {
					path := filepath.Join(t.TempDir(), name)
					if err := os.WriteFile(path, []byte(name), 0600); err != nil {
						t.Fatal(err)
					}
					config.Files[name] = CloudFile{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(name)))}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if _, err := runCloudNode(ctx, document, host, config, "prepare"); err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				defer mu.Unlock()
				body, present := uploaded["maintenance.py"]
				if present != maintenance {
					t.Fatalf("maintenance helper delivery to %s: uploaded=%t want=%t", role, present, maintenance)
				}
				for _, artifact := range []string{"weed", "weed-volume", "native.tar"} {
					_, present := uploaded[artifact]
					if present != cloudArtifact(host, artifact) {
						t.Fatalf("unrelated artifact admission changed: %s on %s", artifact, role)
					}
				}
				if role != "client" {
					for name := range uploaded {
						if strings.HasSuffix(name, ".py") && name != "maintenance.py" {
							t.Fatalf("unrelated helper reached server role: %s", name)
						}
					}
				}
				if maintenance {
					want, err := workerFiles.ReadFile("worker/maintenance.py")
					if err != nil || !bytes.Equal(body, want) {
						t.Fatalf("delivered helper differs from embedded bytes: %v", err)
					}
				}
				if maintenance && role == "meta" {
					root := t.TempDir()
					helper := filepath.Join(root, "maintenance.py")
					if err := os.WriteFile(helper, body, 0600); err != nil {
						t.Fatal(err)
					}
					raw, _ := json.Marshal(map[string]string{"root": root, "stage": "init", "task": "volume.balance", "collection": "maint-upload-caller", "run_id": "upload-caller", "config_sha256": strings.Repeat("a", 64)})
					output, err := exec.CommandContext(ctx, "python3", "-I", helper, string(raw)).CombinedOutput()
					var result struct {
						Initialized bool `json:"initialized"`
					}
					if err != nil || json.Unmarshal(output, &result) != nil || !result.Initialized {
						t.Fatalf("real delivered meta helper init failed: %v %s", err, output)
					}
				}
			})
		}
	}
}
