package infra

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestMissingHostKeyRefusesBothSSHPathsBeforeExec(t *testing.T) {
	var execs atomic.Int64
	n := commandSSHFixtureKeys(t, nil, &execs)
	n.Close()
	n.HostKey = ""
	if err := n.Connect(); err == nil {
		n.Close()
		t.Error("Node.Connect accepted an unpinned host")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if pipe, err := n.OpenCommand(ctx, "must-not-execute"); err == nil {
		pipe.Close()
		t.Error("independent OpenCommand accepted an unpinned host")
	}
	if got := execs.Load(); got != 0 {
		t.Fatalf("unapproved server received %d exec requests", got)
	}
}

func TestApprovedHostKeyConnectsAndOpensIndependentCommand(t *testing.T) {
	var execs atomic.Int64
	n := commandSSHFixtureKeys(t, nil, &execs) // Connect verifies the approved key.
	n.Close()
	if err := n.Connect(); err != nil {
		t.Fatalf("approved reconnect: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	p, err := n.OpenCommand(ctx, "echo")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	var result [2]byte
	if _, err := io.ReadFull(p, result[:]); err != nil || string(result[:]) != "ok" {
		t.Fatalf("approved independent command: %q %v", result, err)
	}
	if execs.Load() != 1 {
		t.Fatal("approved command was not dispatched exactly once")
	}
}

func TestChangedHostKeyRefusesFreshCommandAndReconnectBeforeExec(t *testing.T) {
	var keys atomic.Pointer[ssh.ServerConfig]
	var execs atomic.Int64
	n := commandSSHFixtureKeys(t, &keys, &execs)
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	replacement := &ssh.ServerConfig{NoClientAuth: true}
	replacement.AddHostKey(signer)
	keys.Store(replacement) // Same endpoint; the original Node connection remains open.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if p, err := n.OpenCommand(ctx, "must-not-execute"); err == nil {
		p.Close()
		t.Error("OpenCommand reused old trust/connection after host key changed")
	} else if !errors.Is(err, ErrCommandNotStarted) {
		t.Errorf("host-key refusal lost pre-dispatch proof: %v", err)
	}
	n.Close()
	if err := n.Connect(); err == nil {
		n.Close()
		t.Error("reconnect accepted changed host key")
	}
	if execs.Load() != 0 {
		t.Fatalf("changed-key host received %d exec requests", execs.Load())
	}
}

func TestInvalidHostKeyCannotFallbackToUnverifiedSSH(t *testing.T) {
	for _, kind := range []string{"malformed", "multiple", "options", "leading_garbage"} {
		t.Run(kind, func(t *testing.T) {
			var execs atomic.Int64
			n := commandSSHFixtureKeys(t, nil, &execs)
			n.Close()
			switch kind {
			case "malformed":
				n.HostKey = "SHA256:fingerprint-is-not-a-public-key"
			case "multiple":
				n.HostKey += n.HostKey
			case "leading_garbage":
				n.HostKey = "not-a-key\n" + n.HostKey
			case "options":
				n.HostKey = "no-pty " + n.HostKey
			}
			if err := n.Connect(); err == nil {
				n.Close()
				t.Error("invalid host-key policy admitted Connect")
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if p, err := n.OpenCommand(ctx, "must-not-execute"); err == nil {
				p.Close()
				t.Error("invalid host-key policy admitted OpenCommand")
			}
			if execs.Load() != 0 {
				t.Fatal("invalid policy reached remote exec")
			}
		})
	}
}
