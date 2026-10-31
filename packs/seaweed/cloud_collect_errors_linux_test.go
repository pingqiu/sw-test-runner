//go:build linux

package seaweed

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestCloudDeferredCollectionWriteErrorsStayFailed(t *testing.T) {
	for _, destination := range []string{"step-json", "service-log", "both"} {
		t.Run(destination, func(t *testing.T) {
			output := t.TempDir()
			var blocked atomic.Bool
			address, pin, key, _ := cloudSSH(t, "", func(command string, channel ssh.Channel) uint32 {

				if strings.Contains(command, "cloud-init status --wait --format json") {
					fmt.Fprint(channel, cleanCloudInit)
					return 0
				}
				if strings.HasPrefix(command, "cat >") {
					io.Copy(io.Discard, channel)
				}
				if strings.Contains(command, "kv_smoke.py") && !strings.HasPrefix(command, "cat >") {
					fmt.Fprintln(channel, `{"status":"PASS","ack":16,"exact":16,"wrong":0}`)
				}
				if strings.Contains(command, "journalctl") && blocked.CompareAndSwap(false, true) {
					// Fail the destination at the actual remote-exec -> local-write boundary.
					// A directory gives deterministic EISDIR even when the test runs as root.
					if destination != "service-log" {
						files, _ := filepath.Glob(filepath.Join(output, "cloud-step-*.json"))
						if e := os.Mkdir(filepath.Join(output, fmt.Sprintf("cloud-step-%03d.json", len(files)+1)), 0700); e != nil {
							t.Error(e)
						}
					}
					if destination != "step-json" {
						if e := os.Mkdir(filepath.Join(output, "cloud-services.log"), 0700); e != nil {
							t.Error(e)
						}
					}
				}
				return 0
			})
			cfg := CloudConfig{SSHKey: key, HostKey: pin, Output: output, Files: map[string]CloudFile{}}
			for _, name := range []string{"weed", "weed-volume", "native.tar"} {
				p := filepath.Join(t.TempDir(), name)
				os.WriteFile(p, []byte(name), 0600)
				cfg.Files[name] = CloudFile{Path: p, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(name)))}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			_, err := RunCloudHTTP(ctx, cloudInput(t, address), cfg)
			if err == nil {
				t.Fatal("deferred collection evidence write failed but pack returned nil")
			}
			if destination != "service-log" && !strings.Contains(err.Error(), "cloud-step-") {
				t.Errorf("step JSON write error lost: %v", err)
			}
			if destination != "step-json" && !strings.Contains(err.Error(), "cloud-services.log") {
				t.Errorf("service log write error lost: %v", err)
			}
			if _, e := os.Stat(filepath.Join(output, "cloud-failure-fetch.json")); e != nil {
				t.Errorf("deferred error did not enter failure capture: %v", e)
			}
		})
	}
}
