//go:build linux

package seaweed

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/pingqiu/sw-test-runner/inventory"
	"golang.org/x/crypto/ssh"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDirectClusterNetworkCasesNotRun(t *testing.T) {
	h := &directHost{}
	address, pin, key, _ := cloudSSH(t, "", h.exec)
	for _, name := range []string{"C3-partition", "C6-netem"} {
		t.Run(name, func(t *testing.T) {
			var c CloudConfig
			if err := json.Unmarshal([]byte(`{"cluster_chaos_cases":["`+name+`"]}`), &c); err != nil {
				t.Fatal(err)
			}
			c.SSHKey, c.HostKey, c.Output = key, pin, t.TempDir()
			c.Replication = "001"
			c.Files = map[string]CloudFile{}
			for _, n := range []string{"weed", "weed-volume", "native.tar"} {
				p := filepath.Join(t.TempDir(), n)
				os.WriteFile(p, []byte(n), 0600)
				c.Files[n] = CloudFile{Path: p, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(n)))}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := RunCloudHTTP(ctx, directInput(t, address), c)
			if err == nil || !strings.Contains(err.Error(), "NOT_RUN") {
				t.Fatalf("network selector did not refuse NOT_RUN: %v", err)
			}
		})
	}
	if h.index("systemd-run") >= 0 {
		t.Fatal("unsupported network case started a service")
	}
}

func TestProcessPlacementKeepsPhysicalInventory(t *testing.T) {
	var d inventory.Document
	json.Unmarshal(directInput(t, "127.0.0.1:22"), &d)
	original := string(processJSON(d))
	v, docs, physical, err := processPlacement(d)
	if err != nil {
		t.Fatal(err)
	}
	if string(processJSON(d)) != original || len(v.Hosts) != 10 {
		t.Fatal("physical inventory mutated")
	}
	roots := map[string]bool{}
	for _, h := range v.Hosts {
		if physical[h.ID] != d.Hosts[0].ID || roots[h.RunRoot] || h.RunRoot != cloudRunRoot(docs[h.ID], h) {
			t.Fatal("role identity/root aliased", h)
		}
		roots[h.RunRoot] = true
	}
	two := d
	second := d.Hosts[0]
	second.ID = "node2"
	second.Management.Address = "127.0.0.2"
	second.Data.Address = "10.84.1.11"
	two.Hosts = append(append([]inventory.Host{}, d.Hosts...), second)
	_, _, mapped, err := processPlacement(two)
	if err != nil || mapped["vol4"] != "node2" || mapped["client2"] != "node2" || mapped["meta"] != d.Hosts[0].ID || mapped["vol3"] != d.Hosts[0].ID {
		t.Fatal("two physical host role placement", mapped, err)
	}
	d.Hosts[0].Services = append(d.Hosts[0].Services, inventory.Service{Name: "unused", Port: 1})
	for i := range d.Hosts[0].Services {
		if d.Hosts[0].Services[i].Name == "volume_http" {
			d.Hosts[0].Services[i].Port = 65535
		}
	}
	if _, _, _, err = processPlacement(d); err == nil {
		t.Fatal("overflow ports accepted")
	}
}

// One real SSH endpoint, ten process roles. It exercises the public direct
// caller, not a parallel controller; markers share this host's client clock.
type processFake struct {
	directHost
	mu      sync.Mutex
	variant string
	stamps  int
	stage   *chaosFake
	mixed   *chaos2Fake
	ports   map[string]int
	data    string
}

func (f *processFake) exec(cmd string, ch ssh.Channel) uint32 {
	if strings.HasPrefix(cmd, "cat >") {
		io.Copy(io.Discard, ch)
		return 0
	}
	f.directHost.mu.Lock()
	f.commands = append(f.commands, cmd)
	f.directHost.mu.Unlock()
	if strings.Contains(cmd, "journalctl") && regexp.MustCompile(`client[1-3]-`).MatchString(cmd) {
		return 1
	} // no unit journal exists for SSH-only clients
	if f.variant == "collect-fail" && strings.Contains(cmd, "journalctl") {
		return 23
	}
	if f.variant == "start-fail" && strings.Contains(cmd, "systemd-run") {
		return 1
	}
	if strings.Contains(cmd, "time.monotonic") {
		f.mu.Lock()
		n := f.stamps / 3
		f.stamps++
		f.mu.Unlock()
		offsets := []int{30, 31, 52, 53}
		if f.mixed != nil {
			offsets = []int{30, 55, 80}
		}
		fmt.Fprintln(ch, 1000+offsets[n%len(offsets)])
		return 0
	}
	if strings.Contains(cmd, "/dir/status") && !strings.Contains(cmd, "until") {
		nodes := []map[string]string{}
		for _, port := range f.ports {
			nodes = append(nodes, map[string]string{"Url": fmt.Sprintf("%s:%d", f.data, port)})
		}
		b, _ := json.Marshal(map[string]any{"Topology": map[string]any{"DataCenters": []any{map[string]any{"Racks": []any{map[string]any{"DataNodes": nodes}}}}}})
		fmt.Fprint(ch, string(b))
		return 0
	}
	if strings.Contains(cmd, "/dir/lookup?") {
		vid := 1
		fmt.Sscanf(strings.Split(cmd, "volumeId=")[1], "%d", &vid)
		a, b := 2*(vid-2)-1, 2*(vid-2)
		if f.variant == "foreign-lookup" {
			b = 6
		}
		fmt.Fprintf(ch, `{"locations":[{"url":"%s:%d"},{"url":"%s:%d"}]}`, f.data, f.ports[fmt.Sprint(a)], f.data, f.ports[fmt.Sprint(b)])
		return 0
	}
	if strings.Contains(cmd, "curl") && strings.Contains(cmd, "/status") && !strings.Contains(cmd, "until") {
		for id, port := range f.ports {
			if strings.Contains(cmd, fmt.Sprintf(":%d/status", port)) {
				i := 0
				fmt.Sscan(id, &i)
				vid := 2 + (i+1)/2
				if f.variant == "single-copy" && i == 2 {
					vid = 99
				}
				fmt.Fprintf(ch, `{"Volumes":[{"Id":%d,"ReadOnly":false}]}`, vid)
				return 0
			}
		}
	}
	if strings.Contains(cmd, "kv_smoke.py") || strings.Contains(cmd, "chaos_load.py") || strings.Contains(cmd, "kill -") || strings.Contains(cmd, `echo "NEW`) {
		id := "vol1"
		if m := regexp.MustCompile(`client[1-3]`).FindString(cmd); m != "" {
			id = m
		}
		if strings.Contains(cmd, "-meta-") {
			id = "meta"
		}
		if f.mixed != nil && strings.Contains(cmd, "chaos_load.py") {
			return f.mixed.handle(id)(cmd, ch)
		}
		if strings.Contains(cmd, "kv_smoke.py") && strings.Contains(cmd, "--stage") && strings.Contains(cmd, "seed") {
			inner := cmd
			if i := strings.Index(cmd, "sh -ec '"); i >= 0 {
				inner = strings.ReplaceAll(cmd[i+8:len(cmd)-1], `'"'"'`, `'`)
			}
			i := strings.Index(inner, `'--config' '`) + 12
			var cfg SmokeConfig
			if err := json.Unmarshal([]byte(inner[i:i+strings.Index(inner[i:], `'`)]), &cfg); err != nil {
				panic(err)
			}
			fmt.Fprint(ch, chaosSeed(cfg, func(i int) int {
				if f.variant == "idle-owner" {
					return 3
				}
				return 3 + i%3
			}))
			return 0
		}
		return f.stage.handle(id)(cmd, ch)
	}
	if strings.Contains(cmd, "systemctl list-units") && f.variant == "dirty" {
		fmt.Fprintln(ch, "units=1 ports=1")
		return 1
	}
	if strings.Contains(cmd, "systemctl list-units") {
		fmt.Fprintln(ch, "units=0 ports=0")
	}
	return 0
}
func processRun(t *testing.T, name, variant string) (*processFake, string, error) {
	t.Helper()
	oldBase := chaosBase
	chaosBase = map[string]float64{"client1": 1000, "client2": 1000, "client3": 1000}
	oldSettle, oldHold, oldDown, old2 := chaosSettle, chaosHold, chaos2Down, chaos2Settle
	chaosSettle, chaosHold, chaos2Down, chaos2Settle = time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond
	t.Cleanup(func() {
		chaosBase = oldBase
		chaosSettle, chaosHold, chaos2Down, chaos2Settle = oldSettle, oldHold, oldDown, old2
	})
	f := &processFake{variant: variant, ports: map[string]int{}, stage: &chaosFake{stamps: map[string]int{}, killed: map[string]bool{}}}
	if variant == "pre-P" {
		f.stage.variant = "pre-P"
	}
	if name == "C1-single-kill" {
		f.mixed = &chaos2Fake{stamps: map[string]int{}, killed: map[string]bool{}, cases: map[string]string{}}
	}
	address, pin, key, _ := cloudSSH(t, "", f.exec)
	raw := directInput(t, address)
	var d inventory.Document
	json.Unmarshal(raw, &d)
	f.data = d.Hosts[0].Data.Address
	for i := 1; i <= 6; i++ {
		f.ports[fmt.Sprint(i)] = servicePort(d.Hosts[0], "volume_http") + i - 1
	}
	c := CloudConfig{ClusterChaosCases: []string{name}, Replication: "001", SSHKey: key, HostKey: pin, Output: t.TempDir(), Files: map[string]CloudFile{}}
	for _, n := range []string{"weed", "weed-volume", "native.tar"} {
		p := filepath.Join(t.TempDir(), n)
		os.WriteFile(p, []byte(n), 0600)
		c.Files[n] = CloudFile{Path: p, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(n)))}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := RunCloudHTTP(ctx, raw, c)
	return f, c.Output, err
}
func TestDirectProcessCases(t *testing.T) {
	for _, name := range []string{"volume-restart", "filer-restart", "volume-freeze", "C1-single-kill"} {
		t.Run(name, func(t *testing.T) {
			f, out, err := processRun(t, name, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = os.Stat(filepath.Join(out, "copies.json")); err != nil {
				t.Fatal(err)
			}
			if f.index("systemctl stop") < 0 || f.index("systemctl list-units") < 0 {
				t.Fatal("no teardown/postflight")
			}
			if f.index("iptables") >= 0 || f.index("tc qdisc") >= 0 {
				t.Fatal("network fault on colocated host")
			}
			if name == "volume-restart" && f.index("sw-cloud-pack-cpu-vol1-volume-g2") < 0 {
				t.Fatal("replacement generation not namespaced")
			}
		})
	}
}
func TestDirectProcessRefusalsStillClean(t *testing.T) {
	for _, v := range []string{"foreign-lookup", "single-copy", "start-fail", "pre-P", "dirty", "collect-fail", "idle-owner"} {
		t.Run(v, func(t *testing.T) {
			f, _, err := processRun(t, "volume-restart", v)
			if err == nil {
				t.Fatal("negative passed")
			}
			if f.index("systemctl stop") < 0 || f.index("systemctl list-units") < 0 {
				t.Fatal("failure skipped cleanup")
			}
			if (v == "foreign-lookup" || v == "single-copy" || v == "start-fail" || v == "idle-owner") && f.index("kill -KILL") >= 0 {
				t.Fatal("fault preceded owner proof")
			}
		})
	}
}

// LIVE must describe the same still-live generation, not an empty task glob
// after a process dies while CONT is being dispatched.
func TestProcessResumeCannotWitnessADeadVictim(t *testing.T) {
	r := newScriptRig(t, `[ "$1" = -n ] && shift; if [ "$1" = kill ] && [ "$2" = -CONT ]; then kill -KILL "$3"; sleep 0.1; else exec "$@"; fi`)
	r.victim("u")
	out, err := r.run(chaosStopScript("u"))
	f := strings.Fields(out)
	if err != nil || len(f) != 4 {
		t.Fatal("stop prerequisite", out, err)
	}
	if out, err = r.run(chaosContScript(f[1], f[2])); err == nil || strings.Contains(out, "LIVE") {
		t.Fatal("dead victim earned LIVE", out, err)
	}
}
