//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
	"golang.org/x/crypto/ssh"
)

// soakHost fakes one soak host over ssh: it records every command, reports
// a unit's main process (a different PID per generation), fails any command
// containing failStep and reports leftovers at postflight when dirty.
type soakHost struct {
	mu       sync.Mutex
	commands []string
	failStep string
	dirty    bool
	killMono float64 // the controller clock when the kill reached this host
}

func (h *soakHost) exec(cmd string, c ssh.Channel) uint32 {
	cmd = strings.NewReplacer(`'"'"'`, "", "'", "").Replace(cmd) // match words, not the shell quoting
	h.mu.Lock()
	h.commands = append(h.commands, cmd)
	h.mu.Unlock()
	switch {
	case h.failStep != "" && strings.Contains(cmd, h.failStep):
		return 1
	case strings.Contains(cmd, "systemctl kill"):
		time.Sleep(20 * time.Millisecond) // a stamp taken after the kill would land past killMono
		h.mu.Lock()
		h.killMono, _ = controllerMono()
		h.mu.Unlock()
	case strings.Contains(cmd, "MainPID") && strings.Contains(cmd, "volume-gen2"):
		fmt.Fprintln(c, "5151 77 boot-m02 "+strings.Repeat("b", 64))
	case strings.Contains(cmd, "MainPID"):
		fmt.Fprintln(c, "4242 66 boot-m02 "+strings.Repeat("a", 64))
	case strings.Contains(cmd, "systemctl list-units"):
		if h.dirty {
			fmt.Fprintln(c, "units=1 ports=2")
			return 1
		}
		fmt.Fprintln(c, "units=0 ports=0")
	}
	return 0
}

func (h *soakHost) index(part ...string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
next:
	for i, c := range h.commands {
		for _, p := range part {
			if !strings.Contains(c, p) {
				continue next
			}
		}
		return i
	}
	return -1
}

// directSoakFixture is a two-host direct soak over two fake ssh hosts, each
// reached with its inventory pin, with a small plan: an M01 preflight, M02
// master and volume deploys, an M02 ready and one volume fault (E1).
func directSoakFixture(t *testing.T, m01, m02 *soakHost) *directSoak {
	t.Helper()
	a1, pin1, key, _ := cloudSSH(t, "", m01.exec)
	a2, pin2, _, _ := cloudSSH(t, "", m02.exec)
	var d inventory.Document
	if err := json.Unmarshal(directInput(t, a1), &d); err != nil {
		t.Fatal(err)
	}
	second := d.Hosts[0]
	host, port, _ := strings.Cut(a2, ":")
	second.Management.Address = host
	fmt.Sscan(port, &second.Management.Port)
	d.Hosts = append(d.Hosts, second)
	for i, pin := range []string{pin1, pin2} {
		id := fmt.Sprintf("m0%d", i+1)
		d.Hosts[i].ID, d.Hosts[i].Management.HostKey = id, &pin
		d.Hosts[i].RunRoot = "/home/" + d.Hosts[i].Management.User + "/sw-runs/" + d.RunID
	}
	job := func(node string, argv ...string) tr.JobPin {
		return tr.JobPin{Node: node, Executable: "/usr/bin/python3.12", Argv: append([]string{"/opt/seaweed/worker/lab_profile.py"}, argv...)}
	}
	plan := Plan{Soak: &SoakPlan{},
		Preflight: []Step{{Host: "m01", Job: "preflight-m01"}},
		Deploy:    []Step{{Host: "m02", Role: "master", Job: "deploy-master"}, {Host: "m02", Role: "volume", Job: "deploy-volume"}},
		Ready:     []Step{{Host: "m02", Job: "ready-m02"}}}
	return &directSoak{
		soakWindow: soakWindow{root: t.TempDir(), runID: d.RunID, bootID: "boot-m01", roles: map[string]string{}},
		c:          CloudConfig{SSHKey: key, Output: t.TempDir()},
		d:          &d,
		b: ProfileBundle{Plan: plan, Jobs: map[string]tr.JobPin{
			"preflight-m01": job("m01", "preflight-step"), "ready-m02": job("m02", "ready-step"),
			"deploy-master": job("m02", "deploy", "master"), "deploy-volume": job("m02", "deploy", "volume"),
			"volume-gen2": job("m02", "deploy", "volume", "--replacement")},
			Restarts: map[string]tr.GenerationRestartPlan{"E1": {Victim: "deploy-volume", Replacement: "volume-gen2"}}},
		in: ProfileInput{Files: map[string]map[string]string{
			"m01": {"/opt/seaweed/worker/lab_profile.py": strings.Repeat("c", 64)},
			"m02": {"/opt/seaweed/worker/lab_profile.py": strings.Repeat("c", 64)}}},
		deadline: time.Now().Add(time.Minute),
	}
}

func soakCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// The kill receipt carries the controller clock read before the kill, the
// victim's identity read before it, and the replacement generation's own.
func TestDirectSoakRestartStampsPreKillMono(t *testing.T) {
	m01, m02 := &soakHost{}, &soakHost{}
	s := directSoakFixture(t, m01, m02)
	if err := s.open(soakCtx(t)); err != nil {
		t.Fatal(err)
	}
	before, _ := controllerMono()
	_, r, err := s.Restart(soakCtx(t), "E1", tr.JobHandle{})
	after, _ := controllerMono()
	if err != nil {
		t.Fatal(err)
	}
	if r.Kill.PreKillMono < before || r.Kill.PreKillMono >= m02.killMono || m02.killMono > after || r.Kill.Signal != "SIGKILL" || !r.Kill.AliveBefore {
		t.Fatalf("kill witness %+v not stamped before the kill (host saw it at %f) inside [%f, %f]", r.Kill, m02.killMono, before, after)
	}
	if r.Kill.Identity.PID != 4242 || r.Replacement.PID != 5151 || r.Replacement.ExecutableSHA256 != strings.Repeat("b", 64) ||
		r.Replacement.BootID != "boot-m02" || r.Replacement.StartTime != 77 || r.Replacement.Node != "m02" {
		t.Fatalf("identities: victim %+v replacement %+v", r.Kill.Identity, r.Replacement)
	}
	victim, repl := s.unit("deploy-volume"), s.unit("volume-gen2")
	id, kill, start, newID := m02.index("MainPID", victim), m02.index("systemctl kill -s KILL", victim), m02.index("systemd-run", repl), m02.index("MainPID", repl)
	if !(id >= 0 && id < kill && kill < start && start < newID) || m02.index("systemctl kill", repl) >= 0 || m01.index("systemctl kill") >= 0 || m01.index("systemd-run") >= 0 {
		t.Fatalf("order identity=%d kill=%d start=%d new=%d; m01 saw %v", id, kill, start, newID, m01.commands)
	}
}

// No kill goes out without a replace plan or without the victim's identity.
func TestDirectSoakRestartRefusesBeforeTheKill(t *testing.T) {
	for name, tc := range map[string]struct {
		episode, fail string
		hold          bool
	}{"no plan": {episode: "E9"}, "hold plan": {episode: "E1", hold: true}, "no identity": {episode: "E1", fail: "MainPID"}} {
		t.Run(name, func(t *testing.T) {
			m02 := &soakHost{failStep: tc.fail}
			s := directSoakFixture(t, &soakHost{}, m02)
			if err := s.open(soakCtx(t)); err != nil {
				t.Fatal(err)
			}
			if tc.hold {
				s.b.Restarts["E1"] = tr.GenerationRestartPlan{Hold: true, Victim: "deploy-volume", Replacement: "volume-gen2"}
			}
			if _, _, err := s.Restart(soakCtx(t), tc.episode, tr.JobHandle{}); err == nil || m02.index("systemctl kill") >= 0 {
				t.Fatalf("err=%v kill sent=%v", err, m02.index("systemctl kill") >= 0)
			}
		})
	}
}

// A failure at any point before the soak still cleans up and postflights BOTH
// hosts, each into its own output directory, and stops the run at the failure.
func TestDirectSoakFailureCleansUpBothHosts(t *testing.T) {
	for name, tc := range map[string]struct {
		dirty             bool   // m01 reports leftovers at postflight
		fail, ran, notRan string // m02 fails fail; it must (not) have seen ran (notRan)
		want              string
	}{
		"failed ready":  {fail: "ready-step", ran: "deploy-volume", notRan: "soak-setup"},
		"pin mismatch":  {fail: "sha256sum -c", ran: "sha256sum -c", notRan: "systemd-run"},
		"dirty m01":     {dirty: true, fail: "ready-step", ran: "deploy-volume", want: "postflight not clean"},
		"failed deploy": {fail: "deploy-master", ran: "deploy-master", notRan: "deploy-volume"},
	} {
		t.Run(name, func(t *testing.T) {
			m01, m02 := &soakHost{dirty: tc.dirty}, &soakHost{failStep: tc.fail}
			s := directSoakFixture(t, m01, m02)
			_, err := s.run(soakCtx(t))
			if err == nil || tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want a failure containing %q", err, tc.want)
			}
			for id, h := range map[string]*soakHost{"m01": m01, "m02": m02} {
				for _, part := range []string{"journalctl", "systemctl stop", "systemctl list-units"} {
					if h.index(part, "sw-cloud-"+s.d.RunID+"-") < 0 {
						t.Fatalf("%s never saw %q", id, part)
					}
				}
				if _, e := os.Stat(filepath.Join(s.c.Output, id, "cloud-postflight.json")); e != nil {
					t.Fatalf("%s postflight not journalled in its own dir: %v", id, e)
				}
			}
			if m02.index(tc.ran) < 0 || tc.notRan != "" && m02.index(tc.notRan) >= 0 {
				t.Fatalf("m02 ran %q=%v, not %q=%v", tc.ran, m02.index(tc.ran) >= 0, tc.notRan, m02.index(tc.notRan) >= 0)
			}
		})
	}
}

// Deploy jobs become transient units as the ssh user in the private run
// root; other jobs are bounded steps; every host's root is made private.
func TestDirectSoakStartsDeployJobsAsUnits(t *testing.T) {
	m01, m02 := &soakHost{}, &soakHost{failStep: "ready-step"}
	s := directSoakFixture(t, m01, m02)
	s.run(soakCtx(t))
	user, root := s.d.Hosts[1].Management.User, s.d.Hosts[1].RunRoot
	for _, label := range []string{"deploy-master", "deploy-volume"} {
		if m02.index("systemd-run", "--unit="+s.unit(label), "--uid="+user, "WorkingDirectory="+root, "file:"+root+"/"+label+".stdout") < 0 {
			t.Fatalf("%s not started as a unit: %v", label, m02.commands)
		}
	}
	if m01.index("systemd-run") >= 0 || m01.index("preflight-step") < 0 || m01.index("timeout -k 5 120", "preflight-step") < 0 {
		t.Fatalf("m01 preflight not a bounded step: %v", m01.commands)
	}
	for id, h := range map[string]*soakHost{"m01": m01, "m02": m02} {
		if h.index("install -d -m 0700", s.d.Hosts[0].RunRoot) < 0 && h.index("install -d -m 0700", root) < 0 {
			t.Fatalf("%s run root not made private", id)
		}
	}
}

// The kv_soak gate refuses every shape but a two-host onprem inventory with
// no other phase, before reaching any host.
func TestDirectSoakGate(t *testing.T) {
	h := &soakHost{}
	address, _, key, calls := cloudSSH(t, "", h.exec)
	const gate = "kv_soak requires a two-host onprem inventory"
	two := func(d *inventory.Document) { // a second valid host, so only the kv_soak gate can refuse
		second := d.Hosts[0]
		d.Hosts[0].ID, second.ID = "m01", "m02"
		second.Management.Port++
		second.Data.Address = "192.0.2.77"
		d.Hosts = append(d.Hosts, second)
	}
	for name, tc := range map[string]struct {
		mutate []func(*inventory.Document)
		cfg    func(*CloudConfig)
		want   string
	}{
		"one host":    {want: gate},
		"with files":  {mutate: []func(*inventory.Document){two}, cfg: func(c *CloudConfig) { c.Files = map[string]CloudFile{"weed": {}} }, want: gate},
		"with ec":     {mutate: []func(*inventory.Document){two}, cfg: func(c *CloudConfig) { c.ECStage1 = true }, want: gate},
		"with chaos":  {mutate: []func(*inventory.Document){two}, cfg: func(c *CloudConfig) { c.Chaos = true }, want: gate},
		"with repl":   {mutate: []func(*inventory.Document){two}, cfg: func(c *CloudConfig) { c.Replication = "001" }, want: gate},
		"no profile":  {mutate: []func(*inventory.Document){two}, cfg: func(c *CloudConfig) { c.SoakProfile = filepath.Join(t.TempDir(), "absent.json") }, want: "no such file"},
		"wrong input": {mutate: []func(*inventory.Document){two}, cfg: func(c *CloudConfig) { c.SoakProfile = writeProfile(t, `{"requested_inventory":"{}"}`) }, want: "not the soak profile's requested inventory"},
	} {
		t.Run(name, func(t *testing.T) {
			c := CloudConfig{KVSoak: true, SSHKey: key, Output: t.TempDir()}
			if tc.cfg != nil {
				tc.cfg(&c)
			}
			ctx := soakCtx(t)
			if _, err := RunCloudHTTP(ctx, directInput(t, address, tc.mutate...), c); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("a refused kv_soak reached the host %d times", calls.Load())
	}
}

func writeProfile(t *testing.T, body string) string {
	p := filepath.Join(t.TempDir(), "profile.json")
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
