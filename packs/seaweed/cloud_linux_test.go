//go:build linux

package seaweed

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/inventory"
	"golang.org/x/crypto/ssh"
)

func cloudSSH(t *testing.T, summary string) (string, string, string, *atomic.Int32) {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(key)
	private, _ := ssh.MarshalPrivateKey(key, "cpu-only")
	file := filepath.Join(t.TempDir(), "client-key")
	if e := os.WriteFile(file, pem.EncodeToMemory(private), 0600); e != nil {
		t.Fatal(e)
	}
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	// Ubuntu normally offers both. The ed25519 pin must select its algorithm,
	// rather than letting the client prefer the server's unrelated ECDSA key.
	ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecdsaSigner, err := ssh.NewSignerFromKey(ecdsaKey)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AddHostKey(ecdsaSigner)
	cfg.AddHostKey(signer)
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { ln.Close() })
	calls := &atomic.Int32{}
	go func() {
		for {
			conn, e := ln.Accept()
			if e != nil {
				return
			}
			go func() {
				sc, channels, reqs, e := ssh.NewServerConn(conn, cfg)
				if e != nil {
					conn.Close()
					return
				}
				defer sc.Close()
				go ssh.DiscardRequests(reqs)
				for ch := range channels {
					c, reqs, e := ch.Accept()
					if e != nil {
						return
					}
					go func() {
						defer c.Close()
						for r := range reqs {
							if r.Type != "exec" {
								r.Reply(false, nil)
								continue
							}
							var cmd struct{ Command string }
							ssh.Unmarshal(r.Payload, &cmd)
							calls.Add(1)
							r.Reply(true, nil)
							if strings.HasPrefix(cmd.Command, "cat >") {
								io.Copy(io.Discard, c)
							}
							if strings.Contains(cmd.Command, "kv_smoke.py") && !strings.HasPrefix(cmd.Command, "cat >") {
								fmt.Fprintln(c, summary)
							}
							c.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
							return
						}
					}()
				}
			}()
		}
	}()
	return ln.Addr().String(), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))), file, calls
}

func cloudInput(t *testing.T, address string) []byte {
	t.Helper()
	f := newHTTPFixture(t)
	var d inventory.Document
	json.Unmarshal([]byte(f.plan.Requested), &d)
	host, port, _ := net.SplitHostPort(address)
	fmt.Sscan(port, &d.Hosts[0].Management.Port)
	d.Hosts[0].Management.Address = host
	d.Hosts[0].Roles = []string{"master", "filer", "volume", "s3", "client"}
	d.Hosts[0].Services = []inventory.Service{{Name: "master_http", Port: 9333}, {Name: "master_grpc", Port: 19333}, {Name: "filer_http", Port: 8888}, {Name: "filer_grpc", Port: 18888}, {Name: "volume_http", Port: 8080}, {Name: "volume_grpc", Port: 18080}, {Name: "s3_http", Port: 8333}}
	d.Hosts[0].RunRoot = "/data/seaweed-runs/" + d.RunID
	raw, _ := json.Marshal(d)
	return raw
}

func TestCloudHTTPUsesPinnedSSHWithoutAdmission(t *testing.T) {
	address, pin, key, calls := cloudSSH(t, `{"status":"PASS","ack":16,"exact":16,"wrong":0}`)
	cfg := CloudConfig{SSHKey: key, HostKey: pin, Output: t.TempDir(), Files: map[string]CloudFile{}}
	for _, name := range []string{"weed", "weed-volume", "native.tar"} {
		p := filepath.Join(t.TempDir(), name)
		os.WriteFile(p, []byte(name), 0600)
		cfg.Files[name] = CloudFile{Path: p, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(name)))}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, e := RunCloudHTTP(ctx, cloudInput(t, address), cfg)
	if e != nil || result == "" || calls.Load() == 0 {
		t.Fatalf("%s %v calls=%d", result, e, calls.Load())
	}
}

func TestCloudHTTPHostKeyMismatchExecutesNothing(t *testing.T) {
	address, _, key, calls := cloudSSH(t, "")
	_, other, _, _ := cloudSSH(t, "")
	cfg := CloudConfig{SSHKey: key, HostKey: other, Output: t.TempDir(), Files: map[string]CloudFile{}}
	for _, name := range []string{"weed", "weed-volume", "native.tar"} {
		p := filepath.Join(t.TempDir(), name)
		os.WriteFile(p, []byte(name), 0600)
		cfg.Files[name] = CloudFile{Path: p, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(name)))}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, e := RunCloudHTTP(ctx, cloudInput(t, address), cfg)
	if e == nil || !strings.Contains(e.Error(), "host key") || calls.Load() != 0 {
		t.Fatalf("%v calls=%d", e, calls.Load())
	}
}

func TestCloudHTTPMissingOrWrongSmokeStaysFailed(t *testing.T) {
	for _, summary := range []string{"", `{"status":"PASS","ack":16,"exact":15,"wrong":1}`, `{"status":"FAIL","ack":16,"exact":16,"wrong":0}`} {
		t.Run(summary, func(t *testing.T) {
			address, pin, key, _ := cloudSSH(t, summary)
			cfg := CloudConfig{SSHKey: key, HostKey: pin, Output: t.TempDir(), Files: map[string]CloudFile{}}
			for _, name := range []string{"weed", "weed-volume", "native.tar"} {
				p := filepath.Join(t.TempDir(), name)
				os.WriteFile(p, []byte(name), 0600)
				cfg.Files[name] = CloudFile{Path: p, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(name)))}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, e := RunCloudHTTP(ctx, cloudInput(t, address), cfg)
			if e == nil {
				t.Fatal("missing/wrong workload passed")
			}
		})
	}
}
