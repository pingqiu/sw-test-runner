package infra

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"io"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestPinnedHostKeyNegotiationWithMultipleServerKeys(t *testing.T) {
	for _, kind := range []string{"ed25519", "rsa"} {
		t.Run(kind, func(t *testing.T) {
			var keys atomic.Pointer[ssh.ServerConfig]
			var execs atomic.Int64
			n := commandSSHFixtureKeys(t, &keys, &execs)
			n.Close()
			newSigner := func() ssh.Signer {
				t.Helper()
				var key any
				var err error
				if kind == "rsa" {
					key, err = rsa.GenerateKey(rand.Reader, 2048)
				} else {
					_, key, err = ed25519.GenerateKey(rand.Reader)
				}
				if err != nil {
					t.Fatal(err)
				}
				signer, err := ssh.NewSignerFromKey(key)
				if err != nil {
					t.Fatal(err)
				}
				return signer
			}
			pinned := newSigner()
			ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			ecdsaSigner, err := ssh.NewSignerFromKey(ecdsaKey)
			if err != nil {
				t.Fatal(err)
			}
			server := &ssh.ServerConfig{NoClientAuth: true}
			server.AddHostKey(ecdsaSigner)
			server.AddHostKey(pinned)
			keys.Store(server)
			n.HostKey = string(ssh.MarshalAuthorizedKey(pinned.PublicKey()))
			want := []string{ssh.KeyAlgoED25519}
			if kind == "rsa" {
				want = []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
			}
			config, err := n.sshConfig()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(config.HostKeyAlgorithms, want) {
				t.Errorf("negotiation algorithms = %v, want %v", config.HostKeyAlgorithms, want)
			}
			if err := n.Connect(); err != nil {
				t.Fatalf("multi-key Connect: %v", err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			pipe, err := n.OpenCommand(ctx, "echo")
			if err != nil {
				t.Fatalf("multi-key OpenCommand: %v", err)
			}
			if _, err := pipe.Write([]byte("ok")); err != nil {
				t.Fatal(err)
			}
			var got [2]byte
			if _, err := io.ReadFull(pipe, got[:]); err != nil || string(got[:]) != "ok" {
				t.Fatalf("command output %q: %v", got, err)
			}
			pipe.Close()
			n.Close()
			if execs.Load() != 1 {
				t.Fatal("approved command not executed exactly once")
			}
			n.HostKey = string(ssh.MarshalAuthorizedKey(newSigner().PublicKey()))
			if err := n.Connect(); err == nil {
				n.Close()
				t.Error("wrong same-algorithm pin connected")
			}
			if p, err := n.OpenCommand(ctx, "must-not-execute"); err == nil {
				p.Close()
				t.Error("wrong pin executed")
			}
			if execs.Load() != 1 {
				t.Fatal("wrong pin reached exec")
			}
		})
	}
}
