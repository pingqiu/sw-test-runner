//go:build linux

package smoke

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/inventory"
	"golang.org/x/crypto/ssh"
)

func TestCloudWorkCancellationReachesControllerDestroyAndList(t *testing.T) {
	c := fixture(t)
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	block, _ := ssh.MarshalPrivateKey(key, "cpu-only")
	if e := os.WriteFile(c.Cloud.SSHKey, pem.EncodeToMemory(block), 0600); e != nil {
		t.Fatal(e)
	}
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	accepted := make(chan net.Conn, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		conn, e := listener.Accept()
		if e != nil {
			return
		}
		defer conn.Close()
		accepted <- conn
		cancel()
		io.Copy(io.Discard, conn)
	}()
	b, e := os.ReadFile("../../examples/inventory/http.requested.json")
	if e != nil {
		t.Fatal(e)
	}
	var d inventory.Document
	if e = json.Unmarshal(b, &d); e != nil {
		t.Fatal(e)
	}
	d.Target = "azure"
	id := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/dedicated-test/providers/Microsoft.Compute/virtualMachines/new"
	d.ManagedResourceIDs = []string{id}
	d.Hosts[0].ResourceID = id
	d.Hosts[0].RunRoot = "/data/seaweed-runs/" + d.RunID
	address, port, _ := net.SplitHostPort(listener.Addr().String())
	d.Hosts[0].Management.Address = address
	d.Hosts[0].Management.Port, _ = strconv.Atoi(port)
	raw, _ := json.Marshal(d)
	f := &fake{}
	ctl := Controller{Command: func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
		if tool == "terraform" && args[0] == "output" {
			f.events = append(f.events, "terraform output")
			return raw, nil
		}
		return f.command(ctx, dir, tool, args...)
	}}
	done := make(chan Result, 1)
	go func() { done <- ctl.Execute(ctx, "run", c) }()
	var peer net.Conn
	select {
	case peer = <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("no pack connection")
	}
	defer peer.Close()
	select {
	case r := <-done:
		if r.Code != "PACK_FAILED" || !r.RemainingObserved || !f.cleanContext || !strings.HasSuffix(strings.Join(f.events, ","), "terraform destroy,az resource") {
			t.Fatalf("%+v events=%v", r, f.events)
		}
		capture, e := os.ReadFile(filepath.Join(c.Output, "cloud-failure-fetch.json"))
		var fetch struct {
			Error string `json:"error"`
		}
		if e != nil || json.Unmarshal(capture, &fetch) != nil || fetch.Error == "" {
			t.Fatalf("expired work must record the failed bounded fetch before destroy: %s %v", capture, e)
		}
	case <-time.After(time.Second):
		peer.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("controller stuck after peer close")
		}
		t.Fatal("work cancellation failed to reach final cleanup")
	}
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("test peer not joined")
	}
}
