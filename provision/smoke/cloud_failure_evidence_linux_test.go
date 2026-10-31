//go:build linux

package smoke

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/pingqiu/sw-test-runner/inventory"
	"golang.org/x/crypto/ssh"
)

func TestCloudFailedStepEvidencePrecedesDestroy(t *testing.T) {
	for _, fetchFails := range []bool{false, true} {
		t.Run(fmt.Sprint(fetchFails), func(t *testing.T) {
			c := fixture(t)
			_, key, _ := ed25519.GenerateKey(rand.Reader)
			block, _ := ssh.MarshalPrivateKey(key, "cpu-only")
			os.WriteFile(c.Cloud.SSHKey, pem.EncodeToMemory(block), 0600)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			f := &fake{}
			destroyChecked := false
			ctl := Controller{Command: func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
				if tool == "terraform" && args[0] == "output" {
					b, _ := os.ReadFile(filepath.Join(dir, "host.auto.tfvars.json"))
					var keys struct {
						HostKeys struct {
							Private string `json:"ed25519_private"`
						} `json:"host_keys"`
					}
					json.Unmarshal(b, &keys)
					signer, e := ssh.ParsePrivateKey([]byte(keys.HostKeys.Private))
					if e != nil {
						t.Fatal(e)
					}
					server := &ssh.ServerConfig{NoClientAuth: true}
					server.AddHostKey(signer)
					go func() {
						for {
							conn, e := ln.Accept()
							if e != nil {
								return
							}
							go func() {
								sc, channels, requests, e := ssh.NewServerConn(conn, server)
								if e != nil {
									conn.Close()
									return
								}
								defer sc.Close()
								go ssh.DiscardRequests(requests)
								for ch := range channels {
									channel, requests, e := ch.Accept()
									if e != nil {
										return
									}
									for request := range requests {
										if request.Type != "exec" {
											request.Reply(false, nil)
											continue
										}
										var command struct{ Command string }
										ssh.Unmarshal(request.Payload, &command)
										request.Reply(true, nil)
										rc := uint32(7)
										if strings.Contains(command.Command, "journalctl") {
											if fetchFails {
												fmt.Fprint(channel.Stderr(), "fetch refused")
											} else {
												fmt.Fprint(channel, "worker log; service log")
												rc = 0
											}
										} else {
											fmt.Fprint(channel, "setup stdout")
											fmt.Fprint(channel.Stderr(), "diagnostic before destroy")
										}
										channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{rc}))
										channel.Close()
										break
									}
								}
							}()
						}
					}()
					b, _ = os.ReadFile("../../examples/inventory/http.requested.json")
					var d inventory.Document
					json.Unmarshal(b, &d)
					d.Target = "azure"
					id := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/dedicated-test/providers/Microsoft.Compute/virtualMachines/new"
					d.ManagedResourceIDs = []string{id}
					d.Hosts[0].ResourceID = id
					d.Hosts[0].RunRoot = "/data/seaweed-runs/" + d.RunID
					address, port, _ := net.SplitHostPort(ln.Addr().String())
					d.Hosts[0].Management.Address = address
					d.Hosts[0].Management.Port, _ = strconv.Atoi(port)
					return json.Marshal(d)
				}
				if tool == "terraform" && args[0] == "destroy" {
					destroyChecked = true
					step, e := os.ReadFile(filepath.Join(c.Output, "cloud-step-001.json"))
					if e != nil || !strings.Contains(string(step), "diagnostic before destroy") || !strings.Contains(string(step), "cloud-init") {
						t.Errorf("missing original step evidence before destroy: %s %v", step, e)
					}
					fetch, e := os.ReadFile(filepath.Join(c.Output, "cloud-failure-fetch.json"))
					want := "worker log; service log"
					if fetchFails {
						want = "fetch refused"
					}
					if e != nil || !strings.Contains(string(fetch), want) {
						t.Errorf("missing fetch evidence before destroy: %s %v", fetch, e)
					}
					if !fetchFails && e == nil {
						// Execute the exact collector shell with fixture-only path mapping.
						// This catches quoting that a fake SSH reply would otherwise hide.
						var saved struct {
							Command string `json:"command"`
						}
						json.Unmarshal(fetch, &saved)
						remote := t.TempDir()
						os.Mkdir(filepath.Join(remote, "worker"), 0700)
						os.Mkdir(filepath.Join(remote, "bin"), 0700)
						os.WriteFile(filepath.Join(remote, "worker", "kv_smoke.py"), []byte("captured worker source"), 0600)
						os.WriteFile(filepath.Join(remote, "worker", "stderr.log"), []byte("captured worker stderr"), 0600)
						os.WriteFile(filepath.Join(remote, "cloud-init.log"), []byte("captured cloud-init output"), 0600)
						os.WriteFile(filepath.Join(remote, "bin", "sudo"), []byte("#!/bin/sh\nprintf 'captured service journal\\n'\n"), 0700)
						b, _ := os.ReadFile(filepath.Join(c.Output, "requested-inventory.json"))
						var doc inventory.Document
						json.Unmarshal(b, &doc)
						command := strings.ReplaceAll(saved.Command, doc.Hosts[0].RunRoot, remote)
						command = strings.ReplaceAll(command, "/var/log/cloud-init-output.log", filepath.Join(remote, "cloud-init.log"))
						cmd := exec.CommandContext(ctx, "sh", "-c", command)
						cmd.Env = append(os.Environ(), "PATH="+filepath.Join(remote, "bin")+":"+os.Getenv("PATH"))
						out, e := cmd.CombinedOutput()
						for _, want := range []string{"captured worker source", "captured worker stderr", "captured cloud-init output", "captured service journal"} {
							if e != nil || !strings.Contains(string(out), want) {
								t.Errorf("actual collector shell lost %q: %s %v", want, out, e)
							}
						}
					}
				}
				return f.command(ctx, dir, tool, args...)
			}}
			r := ctl.Execute(t.Context(), "run", c)
			if r.Code != "PACK_FAILED" || !strings.Contains(r.Error, "prepare") || !strings.Contains(r.Error, "cloud-init") || !destroyChecked || !r.RemainingObserved || !strings.HasSuffix(strings.Join(f.events, ","), "terraform destroy,az resource") {
				t.Fatalf("%+v destroy=%v events=%v", r, destroyChecked, f.events)
			}
		})
	}
}
