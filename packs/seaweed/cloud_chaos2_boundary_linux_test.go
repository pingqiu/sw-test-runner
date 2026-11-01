//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func reviewer49Client(t *testing.T, id string, f *chaos2Fake, execute func(string, ssh.Channel) uint32) (string, int, string, string) {
	t.Helper()
	address, pin, key, _ := cloudSSH(t, "", execute)
	host, rawPort, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	var port int
	if _, err := fmt.Sscan(rawPort, &port); err != nil {
		t.Fatal(err)
	}
	return host, port, pin, key
}

func TestChaos2MissingMarkerCannotDispatchKill(t *testing.T) {
	d, c, f := chaos2Input(t, "", "A")
	delegate := f.handle("client2")
	host, port, pin, key := reviewer49Client(t, "client2", f, func(cmd string, ch ssh.Channel) uint32 {
		if strings.Contains(cmd, "time.monotonic") {
			return 23
		}
		return delegate(cmd, ch)
	})
	for i := range d.Hosts {
		if d.Hosts[i].ID == "client2" {
			d.Hosts[i].Management.Address, d.Hosts[i].Management.Port = host, port
		}
	}
	c.HostKeys["client2"], c.SSHKey = pin, key
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := runCloudChaos2(ctx, d, c)
	if err == nil {
		t.Fatal("failed marker did not refuse the run")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.log {
		if strings.Contains(call, ":kill ") {
			t.Fatalf("fault dispatched after marker exit 23: %s; run error=%v", call, err)
		}
	}
}

func TestChaos2CompletedLoadBeforeFaultCannotPass(t *testing.T) {
	d, c, f := chaos2Input(t, "", "B")
	for _, id := range chaosClients {
		id := id
		delegate := f.handle(id)
		host, port, pin, key := reviewer49Client(t, id, f, func(cmd string, ch ssh.Channel) uint32 {
			if strings.Contains(cmd, "chaos_load.py") && strings.Contains(cmd, "--stage load") {
				f.mu.Lock()
				f.stamps[id] = 0
				f.log = append(f.log, id+":early-completed-load")
				f.mu.Unlock()
				ops := []chaosOp{}
				for i, kind := range []string{"kv_set", "kv_get", "s3_put", "s3_get"} {
					s := chaosBase[id] + 0.1 + float64(i)
					ops = append(ops, chaosOp{Op: i, Kind: kind, Key: "kA", Started: s, Ended: s + 0.1, Exact: true})
				}
				json.NewEncoder(ch).Encode(map[string]any{"status": "OBSERVED", "stage": "load", "outcomes": ops})
				return 0
			}
			return delegate(cmd, ch)
		})
		for i := range d.Hosts {
			if d.Hosts[i].ID == id {
				d.Hosts[i].Management.Address, d.Hosts[i].Management.Port = host, port
			}
		}
		c.HostKeys[id], c.SSHKey = pin, key
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runCloudChaos2(ctx, d, c); err == nil {
		t.Fatal("all three cases falsely PASS although every GET completed before the first P marker")
	}
}

func TestChaos2UnknownHealReadbackCannotWitnessAbsent(t *testing.T) {
	t.Run("partition", func(t *testing.T) {
		dir, run, _ := chaos2Rig(t, "")
		if _, err := run(chaosPartitionScript("reviewer-timer")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "bin", "iptables"), []byte("#!/bin/sh\nexit 2\n"), 0700); err != nil {
			t.Fatal(err)
		}
		out, err := run(chaosPartitionHealScript())
		if _, statErr := os.Stat(filepath.Join(dir, "chain")); statErr != nil {
			t.Fatal("control lost the live chain", statErr)
		}
		if err == nil || strings.Contains(out, "HEALED") {
			t.Fatalf("unknown listing certified HEALED while chain remains: out=%q err=%v", out, err)
		}
	})
	t.Run("netem", func(t *testing.T) {
		dir, run, _ := chaos2Rig(t, "")
		if _, err := run(chaosNetemScript("10.84.1.10", "reviewer-timer")); err != nil {
			t.Fatal(err)
		}
		body := "#!/bin/sh\ncase \"$2\" in del) rm -f \"$RIG/qdisc\"; exit 0;; show) exit 2;; esac\n"
		if err := os.WriteFile(filepath.Join(dir, "bin", "tc"), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
		out, err := run(chaosNetemClearScript("10.84.1.10"))
		if err == nil || strings.Contains(out, "CLEARED") {
			t.Fatalf("unknown qdisc readback certified CLEARED: out=%q err=%v", out, err)
		}
	})
}

func TestChaos2TimerInstallFailureHasNoFaultEffect(t *testing.T) {
	for _, kind := range []string{"partition", "netem"} {
		t.Run(kind, func(t *testing.T) {
			dir, run, calls := chaos2Rig(t, "")
			if err := os.WriteFile(filepath.Join(dir, "bin", "systemd-run"), []byte("#!/bin/sh\nexit 29\n"), 0700); err != nil {
				t.Fatal(err)
			}
			cmd := chaosPartitionScript("reviewer-timer")
			if kind == "netem" {
				cmd = chaosNetemScript("10.84.1.10", "reviewer-timer")
			}
			if _, err := run(cmd); err == nil {
				t.Fatal("timer failure accepted")
			}
			for _, call := range calls() {
				if strings.HasPrefix(call, "iptables -N") || strings.HasPrefix(call, "tc qdisc add") {
					t.Fatal("fault before timer installed", call)
				}
			}
		})
	}
}
