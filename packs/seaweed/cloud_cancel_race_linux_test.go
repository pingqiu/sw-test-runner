//go:build linux

package seaweed

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/infra"
	"golang.org/x/crypto/ssh"
)

// Real encrypted loopback SSH, with both output streams active at cancellation.
// Connect deliberately has no connection-context watcher: Run itself must stop
// and join its writers, without waiting for a remote exit receipt.
func TestCloudCancelledPeerActiveSSHStreams(t *testing.T) {
	ready := make(chan struct{}, 2)
	address, pin, key, _ := cloudSSH(t, "", func(_ string, ch ssh.Channel) uint32 {
		var writers sync.WaitGroup
		writers.Add(2)
		for _, stderr := range []bool{false, true} {
			go func(stderr bool) {
				defer writers.Done()
				body := []byte(strings.Repeat("stdout-data\n", 4096))
				write := ch.Write
				if stderr {
					body = []byte(strings.Repeat("stderr-data\n", 4096))
					write = ch.Stderr().Write
				}
				// Exceed the channel window on each stream before cancellation,
				// ensuring both client-side output copiers have actually started.
				for range 64 {
					if _, err := write(body); err != nil {
						return
					}
				}
				ready <- struct{}{}
				for {
					if _, err := write(body); err != nil {
						return
					}
				}
			}(stderr)
		}
		writers.Wait()
		return 0
	})
	n := &infra.Node{Host: address, User: "fixture", KeyFile: key, HostKey: pin}
	if err := n.Connect(); err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type result struct {
		out, stderr string
		rc          int
		err         error
	}
	done := make(chan result, 1)
	go func() {
		out, stderr, rc, err := n.Run(ctx, "fixture-stream-until-cancel")
		done <- result{out, stderr, rc, err}
	}()
	for range 2 {
		select {
		case <-ready:
		case <-time.After(3 * time.Second):
			t.Fatal("SSH output did not start")
		}
	}
	cancel()
	select {
	case r := <-done:
		if r.rc != -1 || r.err == nil || !strings.Contains(r.err.Error(), context.Canceled.Error()) || r.out == "" || r.stderr == "" {
			t.Fatalf("cancellation lost outputs/result: rc=%d out=%d stderr=%d err=%v", r.rc, len(r.out), len(r.stderr), r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled SSH Run failed to join and return")
	}
}
