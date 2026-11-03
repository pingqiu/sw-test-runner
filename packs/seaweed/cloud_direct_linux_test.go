//go:build linux

package seaweed

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/inventory"
	"golang.org/x/crypto/ssh"
)

// directHost fakes an existing ssh host. It records every command and answers
// like a healthy host, except where fail names a step to fail or postflight
// is told to report leftovers.
type directHost struct {
	mu       sync.Mutex
	commands []string
	failStep string
	dirty    bool
}

func (h *directHost) exec(cmd string, c ssh.Channel) uint32 {
	h.mu.Lock()
	h.commands = append(h.commands, cmd)
	h.mu.Unlock()
	switch {
	case strings.HasPrefix(cmd, "cat >"):
		io.Copy(io.Discard, c)
	case h.failStep != "" && strings.Contains(cmd, h.failStep):
		return 1
	case strings.Contains(cmd, "systemctl list-units"):
		if h.dirty {
			fmt.Fprintln(c, "units=1 ports=2")
			return 1
		}
		fmt.Fprintln(c, "units=0 ports=0")
	case strings.Contains(cmd, "kv_smoke.py"):
		fmt.Fprintln(c, `{"status":"PASS","ack":16,"exact":16,"wrong":0}`)
	}
	return 0
}

func (h *directHost) index(part string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, c := range h.commands {
		if strings.Contains(c, part) {
			return i
		}
	}
	return -1
}

// directInput is the HTTP fixture as an onprem inventory: no cloud resources,
// one host, the run root under the ssh user's home.
func directInput(t *testing.T, address string, mutate ...func(*inventory.Document)) []byte {
	t.Helper()
	var d inventory.Document
	if err := json.Unmarshal(cloudInput(t, address), &d); err != nil {
		t.Fatal(err)
	}
	d.Target, d.ManagedResourceIDs = "onprem", []string{}
	d.Hosts[0].ResourceID = ""
	d.Hosts[0].RunRoot = "/home/" + d.Hosts[0].Management.User + "/sw-runs/" + d.RunID
	for _, m := range mutate {
		m(&d)
	}
	raw, _ := json.Marshal(d)
	return raw
}

func directRun(t *testing.T, h *directHost, mutate ...func(*inventory.Document)) error {
	t.Helper()
	address, pin, key, _ := cloudSSH(t, "", h.exec)
	cfg := CloudConfig{SSHKey: key, HostKey: pin, Output: t.TempDir(), Files: map[string]CloudFile{}}
	for _, name := range []string{"weed", "weed-volume", "native.tar"} {
		p := filepath.Join(t.TempDir(), name)
		os.WriteFile(p, []byte(name), 0600)
		cfg.Files[name] = CloudFile{Path: p, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(name)))}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := RunCloudHTTP(ctx, directInput(t, address, mutate...), cfg)
	return err
}

func TestDirectOnpremRunsWithoutCloudInitAndCleansUp(t *testing.T) {
	h := &directHost{}
	if err := directRun(t, h); err != nil {
		t.Fatal(err)
	}
	if h.index("cloud-init") >= 0 {
		t.Fatal("an existing host was asked for cloud-init")
	}
	collect, stop, post := h.index("journalctl"), h.index("systemctl stop"), h.index("systemctl list-units")
	if collect < 0 || stop < 0 || post < 0 || !(collect < stop && stop < post) {
		t.Fatalf("want service logs, then unit stop, then postflight; got %d %d %d", collect, stop, post)
	}
	if !strings.Contains(h.commands[stop], "sw-cloud-") || !strings.Contains(h.commands[post], ":(9333|19333|8888|18888|8080|18080|8333)$") {
		t.Fatalf("cleanup is not bound to this run's units and ports: %q / %q", h.commands[stop], h.commands[post])
	}
}

func TestDirectUncleanPostflightFails(t *testing.T) {
	err := directRun(t, &directHost{dirty: true})
	if err == nil || !strings.Contains(err.Error(), "postflight not clean (units=1 ports=2)") {
		t.Fatalf("leftover units/ports did not fail the run: %v", err)
	}
}

func TestDirectCleanupRunsAfterAFailedStart(t *testing.T) {
	h := &directHost{failStep: "systemd-run"}
	if err := directRun(t, h); err == nil {
		t.Fatal("a failed service start passed")
	}
	if h.index("systemctl stop") < 0 || h.index("systemctl list-units") < 0 {
		t.Fatal("a failed start left the host without cleanup")
	}
}

func TestDirectTargetGates(t *testing.T) {
	// Control: the unmutated direct input runs, so each refusal below is its gate's.
	if err := directRun(t, &directHost{}); err != nil {
		t.Fatalf("control did not run: %v", err)
	}
	cases := []struct {
		name, want string
		mutate     func(*inventory.Document)
	}{
		{"onprem with the VM data root", "unexpected cloud run root", func(d *inventory.Document) { d.Hosts[0].RunRoot = "/data/seaweed-runs/" + d.RunID }},
		{"onprem under another home", "unexpected cloud run root", func(d *inventory.Document) { d.Hosts[0].RunRoot = "/home/other/sw-runs/" + d.RunID }},
		{"onprem with two hosts", "a direct ssh target runs one host", func(d *inventory.Document) {
			h := d.Hosts[0]
			h.ID, h.Management.Port, h.Data.Address = "second", h.Management.Port+1, "10.9.9.9"
			d.Hosts = append(d.Hosts, h)
		}},
		{"aws is not a direct target", "one Azure, onprem or docker HTTP host", func(d *inventory.Document) {
			d.Target, d.ManagedResourceIDs, d.Hosts[0].ResourceID = "aws", []string{"r1"}, "r1"
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &directHost{}
			err := directRun(t, h, c.mutate)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want refusal %q, got %v", c.want, err)
			}
			if h.index("sudo install") >= 0 {
				t.Fatal("a refused shape touched the host")
			}
		})
	}
}

func TestCloudRunRootPerTarget(t *testing.T) {
	for _, c := range []struct{ target, user, want string }{
		{"azure", "azureuser", "/data/seaweed-runs/r1"},
		{"onprem", "testdev", "/home/testdev/sw-runs/r1"},
		{"docker", "root", "/root/sw-runs/r1"},
	} {
		h := inventory.Host{}
		h.Management.User = c.user
		if got := cloudRunRoot(inventory.Document{Target: c.target, RunID: "r1"}, h); got != c.want {
			t.Fatalf("%s/%s: %s, want %s", c.target, c.user, got, c.want)
		}
	}
}

// The postflight's shell is judged by running the exact command the pack sends
// in a real sh, with stub sudo/systemctl/ss on PATH: a probe that cannot run
// must fail the postflight, never read as zero.
func TestDirectPostflightShellSemantics(t *testing.T) {
	h := &directHost{}
	if err := directRun(t, h); err != nil {
		t.Fatal(err)
	}
	post := h.commands[h.index("systemctl list-units")]
	if !strings.Contains(post, "sudo systemctl list-units") {
		t.Fatalf("postflight lists units without sudo: %q", post)
	}
	for _, c := range []struct {
		name, units, unitsRC, listen, ssRC string
		clean                              bool
	}{
		{"clean", "", "0", "LISTEN 0 4096 0.0.0.0:22 0.0.0.0:*", "0", true},
		{"a unit left", "sw-cloud-x-master.service loaded active running", "0", "", "0", false},
		{"a port left", "", "0", "LISTEN 0 4096 0.0.0.0:9333 0.0.0.0:*", "0", false},
		{"systemctl fails", "", "1", "", "0", false},
		{"ss fails", "", "0", "", "1", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			bin := t.TempDir()
			stub := func(name, body string) {
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			stub("sudo", `exec "$@"`)
			stub("systemctl", "printf '%s' '"+c.units+"'; exit "+c.unitsRC)
			stub("ss", "printf '%s\n' '"+c.listen+"'; exit "+c.ssRC)
			cmd := exec.Command("sh", "-c", post)
			cmd.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin")
			out, err := cmd.CombinedOutput()
			if c.clean != (err == nil) {
				t.Fatalf("clean=%v but err=%v, output %q", c.clean, err, out)
			}
		})
	}
}
