package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

func TestScenarioHostKeyReachesSSHVerification(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(t.TempDir(), "client-key")
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
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
				conn.SetDeadline(time.Now().Add(3 * time.Second))
				defer conn.Close()
				config := &ssh.ServerConfig{NoClientAuth: true}
				config.AddHostKey(signer)
				server, channels, requests, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				for channel := range channels {
					channel.Reject(ssh.Prohibited, "no commands in config test")
				}
			}()
		}
	}()
	defer func() { listener.Close(); <-stopped; workers.Wait() }()
	for _, test := range []struct {
		name, hostKey string
		wantOK        bool
	}{
		{"approved", "{{ approved_host_key }}", true},
		{"missing", "", false},
		{"invalid", "unapproved-key", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var scenario tr.Scenario
			input := fmt.Sprintf("env:\n  approved_host_key: %s\ntopology:\n  nodes:\n    fake:\n      host: %s\n      user: fake\n      key: %s\n      host_key: %s\n", strconv.Quote(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))), strconv.Quote(listener.Addr().String()), strconv.Quote(key), strconv.Quote(test.hostKey))
			if err := yaml.Unmarshal([]byte(input), &scenario); err != nil {
				t.Fatal(err)
			}

			ctx, err := setupActionContext(&scenario, func(string, ...interface{}) {})
			if ctx != nil {
				for _, node := range ctx.Nodes {
					node.Close()
				}
			}
			if (err == nil) != test.wantOK {
				t.Fatalf("host-key config admission: %v, want success=%t", err, test.wantOK)
			}
		})
	}
}
