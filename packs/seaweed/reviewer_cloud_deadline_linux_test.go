//go:build linux

package seaweed

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestReviewerCloudSilentSSHHandshakeHonorsWorkDeadline(t *testing.T) {
	_, pin, key, _ := cloudSSH(t, "")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, e := listener.Accept()
		if e == nil {
			accepted <- conn
		}
	}()
	cfg := CloudConfig{SSHKey: key, HostKey: pin, Output: t.TempDir(), Files: map[string]CloudFile{}}
	for _, name := range []string{"weed", "weed-volume", "native.tar"} {
		path := filepath.Join(t.TempDir(), name)
		if err = os.WriteFile(path, []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
		cfg.Files[name] = CloudFile{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(name)))}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	started := time.Now()
	go func() { _, e := RunCloudHTTP(ctx, cloudInput(t, listener.Addr().String()), cfg); done <- e }()
	var peer net.Conn
	select {
	case peer = <-accepted:
	case <-time.After(time.Second):
		t.Fatal("no loopback SSH connection")
	}
	defer peer.Close()
	<-ctx.Done()
	select {
	case err = <-done:
		if err == nil {
			t.Fatal("silent SSH unexpectedly passed")
		}
		t.Logf("bounded refusal after %s: %v", time.Since(started), err)
	case <-time.After(250 * time.Millisecond):
		// End only our own loopback peer so the failing control leaves no
		// lingering handshake goroutine; this is not a product recovery.
		_ = peer.Close()
		select {
		case err = <-done:
		case <-time.After(time.Second):
			t.Fatal("handshake survived peer close")
		}
		t.Fatalf("work deadline expired but RunCloudHTTP stayed in SSH handshake; returned only after test peer close at %s: %v", time.Since(started), err)
	}
}

func TestReviewerCloudSilentSessionOpenHonorsWorkDeadline(t *testing.T) {
	_, pin, key, _ := cloudSSH(t, "")
	private, err := os.ReadFile(key)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	server := &ssh.ServerConfig{NoClientAuth: true}
	server.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	sessionRequested := make(chan struct{}, 1)
	go func() {
		conn, e := listener.Accept()
		if e != nil {
			return
		}
		accepted <- conn
		sc, channels, requests, e := ssh.NewServerConn(conn, server)
		if e != nil {
			conn.Close()
			return
		}
		defer sc.Close()
		go ssh.DiscardRequests(requests)
		for range channels {
			// Deliberately never accept/reject the channel request. The
			// controller's work context must still close this connection.
			sessionRequested <- struct{}{}
		}
	}()
	cfg := CloudConfig{SSHKey: key, HostKey: pin, Output: t.TempDir(), Files: map[string]CloudFile{}}
	for _, name := range []string{"weed", "weed-volume", "native.tar"} {
		path := filepath.Join(t.TempDir(), name)
		if err = os.WriteFile(path, []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
		cfg.Files[name] = CloudFile{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(name)))}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	started := time.Now()
	go func() { _, e := RunCloudHTTP(ctx, cloudInput(t, listener.Addr().String()), cfg); done <- e }()
	var peer net.Conn
	select {
	case peer = <-accepted:
	case <-time.After(time.Second):
		t.Fatal("no connection")
	}
	defer peer.Close()
	select {
	case <-sessionRequested:
	case <-time.After(time.Second):
		t.Fatal("no SSH session request")
	}
	<-ctx.Done()
	select {
	case err = <-done:
		if err == nil {
			t.Fatal("silent channel unexpectedly passed")
		}
	case <-time.After(250 * time.Millisecond):
		_ = peer.Close()
		select {
		case err = <-done:
		case <-time.After(time.Second):
			t.Fatal("session survived peer close")
		}
		t.Fatalf("work deadline expired but session-open and cancellation close stayed blocked; returned after test peer close at %s: %v", time.Since(started), err)
	}
}
