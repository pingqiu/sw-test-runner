package infra

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type pausedSSHConn struct {
	net.Conn
	paused  atomic.Bool
	release <-chan struct{}
}

func (c *pausedSSHConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if c.paused.Load() {
		<-c.release
	}
	return n, err
}

// A real encrypted loopback SSH transport, without shell/host/lab execution.
func commandSSHFixture(t *testing.T) *Node {
	t.Helper()
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
	var mu sync.Mutex
	var conns []net.Conn
	var wg sync.WaitGroup
	stopped := make(chan struct{})
	unpause := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				wire := &pausedSSHConn{Conn: conn, release: unpause}
				server, channels, requests, err := ssh.NewServerConn(wire, config)
				if err != nil {
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				sessions := 0
				for request := range channels {
					sessions++
					if sessions > 1 {
						_ = request.Reject(ssh.ResourceShortage, "one session per connection")
						continue
					}
					channel, incoming, err := request.Accept()
					if err != nil {
						return
					}
					wg.Add(1)
					go func() {
						defer wg.Done()
						defer channel.Close()
						for request := range incoming {
							var payload struct{ Command string }
							if ssh.Unmarshal(request.Payload, &payload) != nil {
								return
							}
							if payload.Command == "'hang'" {
								continue
							}
							if payload.Command == "'blocked'" {
								wire.paused.Store(true)
								_ = request.Reply(true, nil)
								continue
							}
							_ = request.Reply(true, nil)
							_, _ = io.Copy(channel, channel)
							return
						}
					}()
				}
			}()
		}
	}()
	t.Cleanup(func() {
		close(unpause)
		listener.Close()
		<-stopped
		mu.Lock()
		for _, conn := range conns {
			conn.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	node := &Node{Host: listener.Addr().String(), User: "fake", KeyFile: keyfile}
	if err := node.Connect(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(node.Close)
	return node
}

func TestOwnedCommandConnectionInterruptsBlockedWriteAndIsolatesOtherJobs(t *testing.T) {
	node := commandSSHFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	blocked, err := node.OpenCommand(ctx, "blocked")
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Close()
	other, err := node.OpenCommand(ctx, "echo")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	done := make(chan error, 1)
	go func() { _, err := blocked.Write(make([]byte, 8<<20)); done <- err }()
	select {
	case <-done:
		t.Fatal("write did not block on the real SSH window")
	case <-time.After(50 * time.Millisecond):
	}
	_ = blocked.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked write succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("writer survived connection close")
	}
	if _, err := other.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	var got [1]byte
	if _, err := io.ReadFull(other, got[:]); err != nil || got[0] != 'x' {
		t.Fatalf("unrelated job damaged: %q %v", got, err)
	}
}

func TestOwnedCommandDoesNotUseSharedMaxSessionsAndSettlesCancelledCreation(t *testing.T) {
	node := commandSSHFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var pipes []*CommandPipe
	defer func() {
		for _, p := range pipes {
			p.Close()
		}
	}()
	for i := 0; i < 12; i++ {
		p, err := node.OpenCommand(ctx, "echo")
		if err != nil {
			t.Fatalf("job %d: %v", i, err)
		}
		pipes = append(pipes, p)
	}
	short, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	defer stop()
	if _, err := node.OpenCommand(short, "hang"); err == nil || errors.Is(err, ErrCommandNotStarted) {
		t.Fatal("unanswered exec was accepted or falsely marked NOT_STARTED")
	}
	if _, err := pipes[0].Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	var got [1]byte
	if _, err := io.ReadFull(pipes[0], got[:]); err != nil {
		t.Fatal(err)
	}
}

func TestOwnedCommandDialFailureIsProvablyNotStarted(t *testing.T) {
	node := commandSSHFixture(t)
	node.Host = "127.0.0.1:0"
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := node.OpenCommand(ctx, "never-dispatched"); !errors.Is(err, ErrCommandNotStarted) {
		t.Fatalf("dial failure lost its pre-dispatch proof: %v", err)
	}
}

func TestOwnedCommandCancelledHandshakeIsProvablyNotStarted(t *testing.T) {
	node := commandSSHFixture(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	node.Host = listener.Addr().String()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := node.OpenCommand(ctx, "never-dispatched"); !errors.Is(err, ErrCommandNotStarted) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled handshake lost its pre-dispatch proof: %v", err)
	}
}

func TestOwnedCommandCloseSettlesBlackholedSSHWriter(t *testing.T) {
	node := commandSSHFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	p, err := node.OpenCommand(ctx, "blocked")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := p.Write(make([]byte, 8<<20)); done <- err }()
	select {
	case <-done:
		t.Fatal("fixture did not expose a blocked channel write")
	case <-time.After(50 * time.Millisecond):
	}
	closed := make(chan struct{})
	go func() { _ = p.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("job transport close exceeded its own deadline")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Error("blocked write returned success")
		}
	case <-time.After(time.Second):
		t.Fatal("writer survived the job transport close")
	}
}
