//go:build linux

package infra

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A native node (the controller's own host) connects without ssh, as Connect
// does, and then runs locally; before, ConnectContext dialled ":22".
func TestNativeConnectContextRunsLocally(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n := &Node{IsNative: true}
	if err := n.ConnectContext(ctx); err != nil {
		t.Fatal(err)
	}
	out, _, rc, err := n.Run(ctx, "echo native-ok")
	if err != nil || rc != 0 || strings.TrimSpace(out) != "native-ok" {
		t.Fatalf("out=%q rc=%d err=%v", out, rc, err)
	}
	n.Close()
}
