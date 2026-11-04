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
	hangKV   time.Duration // the kv_smoke step blocks this long before answering
	hangLog  time.Duration // the journal capture blocks this long before answering
	journals int           // journal captures answered so far
	out      string        // the run's local output directory
}

func (h *directHost) exec(cmd string, c ssh.Channel) uint32 {
	h.mu.Lock()
	h.commands = append(h.commands, cmd)
	h.mu.Unlock()
	switch {
	case strings.HasPrefix(cmd, "cat >"):
		io.Copy(io.Discard, c)
	case strings.Contains(cmd, "journalctl"):
		time.Sleep(h.hangLog)
		h.mu.Lock()
		h.journals++ // each capture is distinguishable, so an overwrite shows
		n := h.journals
		h.mu.Unlock()
		fmt.Fprintf(c, "JOURNAL-MARKER %d\n", n)
	case h.failStep != "" && strings.Contains(cmd, h.failStep):
		return 1
	case strings.Contains(cmd, "systemctl list-units"):
		if h.dirty {
			fmt.Fprintln(c, "units=1 ports=2")
			return 1
		}
		fmt.Fprintln(c, "units=0 ports=0")
	case strings.Contains(cmd, "kv_smoke.py"):
		time.Sleep(h.hangKV)
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
	return directRunWithin(t, h, 10*time.Second, mutate...)
}

func directRunWithin(t *testing.T, h *directHost, within time.Duration, mutate ...func(*inventory.Document)) error {
	t.Helper()
	address, pin, key, _ := cloudSSH(t, "", h.exec)
	cfg := CloudConfig{SSHKey: key, HostKey: pin, Output: t.TempDir(), Files: map[string]CloudFile{}}
	h.out = cfg.Output
	for _, name := range []string{"weed", "weed-volume", "native.tar"} {
		p := filepath.Join(t.TempDir(), name)
		os.WriteFile(p, []byte(name), 0600)
		cfg.Files[name] = CloudFile{Path: p, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(name)))}
	}
	ctx, cancel := context.WithTimeout(context.Background(), within)
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
	if !strings.Contains(h.commands[stop], "sw-cloud-") || !strings.Contains(h.commands[post], ":(9333|19333|8888|18888|8080|18080|8333)[[:space:]]") {
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
	collect, stop, post := h.index("journalctl"), h.index("systemctl stop"), h.index("systemctl list-units")
	if stop < 0 || post < 0 {
		t.Fatal("a failed start left the host without cleanup")
	}
	if collect < 0 || collect > stop {
		t.Fatalf("a failed start's logs were not read before teardown: journal %d, stop %d", collect, stop)
	}
	// codex01 review c8a23688 R4: no later fetch overwrites the captured journal.
	var captured struct {
		Stdout string `json:"stdout"`
	}
	raw, _ := os.ReadFile(filepath.Join(h.out, "cloud-collect.json"))
	b, err := os.ReadFile(filepath.Join(h.out, "cloud-services.log"))
	if json.Unmarshal(raw, &captured) != nil || !strings.Contains(captured.Stdout, "JOURNAL-MARKER") || err != nil || string(b) != captured.Stdout {
		t.Fatalf("the pre-teardown service log was lost or overwritten: log %q, captured %q, %v", b, captured.Stdout, err)
	}
}

// codex01 review c8a23688 R1: a stalled journal capture is cut at its own
// controller-side bound, on its own connection, and never spends the teardown.
func TestDirectStalledJournalCannotSpendTheTeardown(t *testing.T) {
	collect, teardown := directCollectBound, directTeardownBound
	directCollectBound, directTeardownBound = time.Second, 5*time.Second
	t.Cleanup(func() { directCollectBound, directTeardownBound = collect, teardown })
	h := &directHost{hangLog: 10 * time.Second}
	started := time.Now()
	err := directRun(t, h)
	if err == nil || !strings.Contains(err.Error(), "collect-services") {
		t.Fatalf("a stalled journal capture did not fail the run: %v", err)
	}
	if h.index("systemctl stop") < 0 || h.index("systemctl list-units") < 0 {
		t.Fatal("a stalled journal capture left the host without teardown")
	}
	if elapsed := time.Since(started); elapsed > 8*time.Second {
		t.Fatalf("the stalled capture held the cleanup for %v", elapsed)
	}
}

// codex01 review R1: a cancelled run's connection closes with its context, so
// cleanup must reach the host on its own connection, never read as clean.
func TestDirectCleanupReachesTheHostAfterCancellation(t *testing.T) {
	h := &directHost{hangKV: 3 * time.Second}
	err := directRunWithin(t, h, 1500*time.Millisecond)
	if err == nil {
		t.Fatal("a cancelled run passed")
	}
	stop, post := h.index("systemctl stop"), h.index("systemctl list-units")
	if stop < 0 || post < 0 || stop < h.index("kv_smoke.py") {
		t.Fatalf("cleanup did not reach the host after cancellation: stop %d, postflight %d", stop, post)
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
	if !strings.Contains(post, "sudo -n systemctl list-units") {
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
		{"awk fails", "", "0", "LISTEN 0 4096 0.0.0.0:9333 0.0.0.0:*", "0", false},
		{"grep fails", "", "0", "", "0", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			bin := t.TempDir()
			stub := func(name, body string) {
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			stub("sudo", `test "$1" = -n || exit 90; shift; exec "$@"`)
			stub("systemctl", "printf '%s' '"+c.units+"'; exit "+c.unitsRC)
			stub("ss", "printf '%s\n' '"+c.listen+"'; exit "+c.ssRC)
			if c.name == "awk fails" {
				stub("awk", "exit 2")
			}
			if c.name == "grep fails" {
				stub("grep", "exit 2")
			}
			cmd := exec.Command("sh", "-c", post)
			cmd.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin")
			out, err := cmd.CombinedOutput()
			if c.clean != (err == nil) {
				t.Fatalf("clean=%v but err=%v, output %q", c.clean, err, out)
			}
		})
	}
}

// Run the exact capture shell with real find: a failed cat must fail capture,
// while the independent SSH teardown still stops units and checks all ports.
func TestDirectECJournalReadFailureStillTearsDown(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("failed-cat-", fail), func(t *testing.T) {
			bin, root := t.TempDir(), t.TempDir()
			for name, body := range map[string]string{
				"sudo":       `exec "$@"`,
				"journalctl": "echo service-journal",
				"cat": func() string {
					if fail {
						return "echo unreadable >&2; exit 23"
					}
					return `exec /bin/cat "$@"`
				}(),
			} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(root, "journal space.jsonl"), []byte("outcome\n"), 0600); err != nil {
				t.Fatal(err)
			}
			h := &directHost{}
			execute := func(command string, channel ssh.Channel) uint32 {
				if !strings.Contains(command, "journalctl") {
					return h.exec(command, channel)
				}
				h.mu.Lock()
				h.commands = append(h.commands, command)
				h.mu.Unlock()
				cmd := exec.Command("sh", "-c", command)
				cmd.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin")
				out, err := cmd.CombinedOutput()
				channel.Write(out)
				if err != nil {
					if exit, ok := err.(*exec.ExitError); ok {
						return uint32(exit.ExitCode())
					}
					return 255
				}
				return 0
			}
			address, pin, key, _ := cloudSSH(t, "", execute)
			var d inventory.Document
			if err := json.Unmarshal(directInput(t, address), &d); err != nil {
				t.Fatal(err)
			}
			d.Hosts[0].RunRoot = root // disposable tree, otherwise unchanged exact shell
			out := t.TempDir()
			err := directCleanup(CloudConfig{ECStage1: true, SSHKey: key, HostKey: pin, Output: out}, d, d.Hosts[0])
			if (err != nil) != fail {
				t.Fatalf("failed cat=%v, capture error=%v", fail, err)
			}
			collect, stop, post := h.index("journalctl"), h.index("systemctl stop"), h.index("systemctl list-units")
			if collect < 0 || stop <= collect || post <= stop {
				t.Fatalf("teardown lost: %d %d %d", collect, stop, post)
			}
			if fail {
				raw, readErr := os.ReadFile(filepath.Join(out, "cloud-collect.json"))
				if readErr != nil || !strings.Contains(string(raw), "unreadable") || !strings.Contains(string(raw), `"exit_code": 1`) {
					t.Fatalf("failed cat evidence missing: %s %v", raw, readErr)
				}
			}
		})
	}
}
