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
	mu          sync.Mutex
	variant     string
	stamps      int
	stage       *chaosFake
	mixed       *chaos2Fake
	ports       map[string]int
	data        string
	baseRoot    string
	journalRead bool
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
		if vid >= 13 {
			vid -= 10
		}
		a, b := 2*(vid-2)-1, 2*(vid-2)
		if f.variant == "c5-zero-pair" {
			if vid == 3 {
				b = 3
			}
			if vid == 4 {
				a = 2
			}
		}
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
				if f.variant == "c5-zero-pair" {
					if i == 2 {
						vid = 4
					}
					if i == 3 {
						vid = 3
					}
				}
				if f.variant == "single-copy" && i == 2 {
					vid = 99
				}
				root := f.baseRoot + "-vol" + id
				dir := root + "/volume"
				if f.variant == "foreign-vol6" && i == 6 {
					dir = "/unrelated/foreign-volume"
				}
				if strings.Contains(cmd, "/proc/$pid/cmdline") {
					if f.variant == "foreign-identity-vol6" && i == 6 {
						return 1
					}
					fmt.Fprintf(ch, "OWNER %d 555 %s:%d %s\n", 1000+i, f.data, port, root)
				}
				mixed := vid + 10
				if f.variant == "mixed-s3-one-copy" && i == 2 {
					mixed = 99
				}
				f.mu.Lock()
				after := f.journalRead
				f.mu.Unlock()
				volumes := []map[string]any{{"Id": vid, "ReadOnly": false}}
				if after {
					volumes = append(volumes, map[string]any{"Id": mixed, "ReadOnly": false})
				}
				raw, _ := json.Marshal(map[string]any{"DiskStatuses": []map[string]string{{"dir": dir}}, "ProcessID": 999999, "Volumes": volumes})
				fmt.Fprint(ch, string(raw))
				return 0
			}
		}
	}
	if strings.Contains(cmd, "cat ") && strings.Contains(cmd, ".journal") && !strings.Contains(cmd, "journalctl") {
		f.mu.Lock()
		f.journalRead = true
		f.mu.Unlock()
		for _, vid := range []int{3, 4, 5, 13, 14, 15} {
			fid := fmt.Sprintf("%d,abc", vid)
			if f.variant == "mixed-s3-unknown" && vid == 13 {
				fid = "99,abc"
			}
			if f.variant == "mixed-s3-no-fid" && vid == 13 {
				fmt.Fprintln(ch, `{"event":"ACK","kind":"s3","key":"s","fids":null}`)
				continue
			}
			key := fmt.Sprintf("k%d", vid)
			if strings.HasPrefix(f.variant, "c5-") && vid == 4 {
				key = "kU"
			}
			kind := "kv"
			if vid >= 13 {
				kind = "s3"
			}
			if f.variant == "c5-mixed-unknown" && vid == 4 {
				fmt.Fprintf(ch, `{"event":"ACK","kind":%q,"key":%q,"fids":[%q,"99,unknown"]}`+"\n", kind, key, fid)
			} else {
				fmt.Fprintf(ch, `{"event":"ACK","kind":%q,"key":%q,"fids":[%q]}`+"\n", kind, key, fid)
			}
		}
		return 0
	}
	if strings.Contains(cmd, "kv_smoke.py") || strings.Contains(cmd, "chaos_load.py") || strings.Contains(cmd, "kill -") || strings.Contains(cmd, `echo "NEW`) {
		id := "vol1"
		if m := regexp.MustCompile(`vol[1-6]|client[1-3]`).FindString(cmd); m != "" {
			id = m
		}
		if !strings.HasPrefix(id, "client") && strings.Contains(cmd, "-meta-") {
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
	if strings.HasPrefix(name, "C") {
		f.mixed = &chaos2Fake{variant: variant, stamps: map[string]int{}, killed: map[string]bool{}, cases: map[string]string{}}
		if variant == "c5-zero-pair" {
			f.mixed.variant = ""
		}
	}
	address, pin, key, _ := cloudSSH(t, "", f.exec)
	raw := directInput(t, address)
	var d inventory.Document
	json.Unmarshal(raw, &d)
	f.data = d.Hosts[0].Data.Address
	f.baseRoot = d.Hosts[0].RunRoot
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
	for _, name := range []string{"volume-restart", "filer-restart", "volume-freeze", "C1-single-kill", "C2-rolling", "C4-meta-restart", "C5-two-kill"} {
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

func TestDirectProcessReviewRemainders(t *testing.T) {
	for _, v := range []string{"mixed-s3-unknown", "mixed-s3-one-copy", "mixed-s3-no-fid", "foreign-vol6", "foreign-identity-vol6"} {
		t.Run(v, func(t *testing.T) {
			f, _, err := processRun(t, "C1-single-kill", v)
			if err == nil {
				t.Fatal("unqualified mixed workload passed", v)
			}
			if strings.HasPrefix(v, "foreign-") && f.index("kill -KILL") >= 0 {
				t.Fatal("foreign non-victim owner reached fault")
			}
			if f.index("systemctl stop") < 0 || f.index("systemctl list-units") < 0 {
				t.Fatal("refusal skipped cleanup")
			}
		})
	}
}

func TestProcessOwnerRejectsAnActualForeignProcess(t *testing.T) {
	f, _, err := processRun(t, "C1-single-kill", "foreign-identity-vol6")
	if err == nil {
		t.Fatal("foreign owner fixture passed")
	}
	var cmd string
	for _, s := range f.commands {
		if strings.Contains(s, "/proc/$pid/cmdline") && strings.Contains(s, "-vol6/volume") {
			cmd = s
			break
		}
	}
	if cmd == "" {
		t.Fatal("role was never bound to its actual process")
	}
	rig := newScriptRig(t, `[ "$1" = -n ] && shift; exec "$@"`)
	rig.victim("sw-cloud-pack-cpu-vol6-volume")
	if out, err := rig.run(cmd); err == nil || strings.Contains(out, "OWNER") {
		t.Fatal("foreign executable/argv earned owner proof", out, err)
	}
}

func TestDirectProcessFollowupSchedules(t *testing.T) {
	for _, x := range []struct {
		name           string
		seconds, kills int
	}{{"C2-rolling", 360, 6}, {"C4-meta-restart", 150, 2}, {"C5-two-kill", 210, 2}} {
		t.Run(x.name, func(t *testing.T) {
			f, _, err := processRun(t, x.name, "")
			if err != nil {
				t.Fatal(err)
			}
			kills := 0
			for _, cmd := range f.commands {
				if strings.Contains(cmd, "kill -KILL") {
					kills++
				}
				if strings.Contains(cmd, "--stage load") && !strings.Contains(cmd, fmt.Sprintf("--seconds %d", x.seconds)) {
					t.Fatal("schedule changed", cmd)
				}
			}
			if kills != x.kills {
				t.Fatal("wrong kill count", kills)
			}
			if x.name == "C2-rolling" {
				for i := 1; i <= 6; i++ {
					if f.index(fmt.Sprintf("-vol%d-volume-g2", i)) < 0 {
						t.Fatal("missing rolling replacement", i)
					}
				}
			}
			if x.name == "C4-meta-restart" {
				for _, role := range []string{"master", "filer"} {
					if f.index("-meta-"+role+"-g2") < 0 {
						t.Fatal("missing meta replacement", role)
					}
				}
			}
		})
	}
}
func TestDirectProcessC5CopyAttribution(t *testing.T) {
	for _, v := range []string{"c5-unavailable", "c5-mixed-unknown"} {
		t.Run(v, func(t *testing.T) {
			f, _, err := processRun(t, "C5-two-kill", v)
			if v == "c5-unavailable" && err != nil {
				t.Fatal("known killed pair refused", err)
			}
			if v == "c5-mixed-unknown" && (err == nil || !strings.Contains(err.Error(), "marker+60s")) {
				t.Fatal("unknown copy earned outage credit", err)
			}
			if f.index("systemctl list-units") < 0 {
				t.Fatal("missing cleanup")
			}
		})
	}
}

func TestDirectProcessC5PersistsUnavailableKeyCoverage(t *testing.T) {
	for _, v := range []string{"c5-zero-pair", "c5-eligible-only", "c5-unavailable", "c5-sweep-shape"} {
		t.Run(v, func(t *testing.T) {
			_, out, err := processRun(t, "C5-two-kill", v)
			if err != nil {
				t.Fatal(err)
			}
			var row struct {
				Status          string
				ProcessRecovery string `json:"process_recovery"`
				UnavailableKeys struct {
					Status        string
					Reason        string
					EligibleKeys  int `json:"eligible_keys"`
					AffectedReads int `json:"affected_reads"`
				} `json:"unavailable_keys"`
			}
			raw, err := os.ReadFile(filepath.Join(out, "process-result.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(raw, &row); err != nil {
				t.Fatal(err)
			}
			t.Log(string(raw))
			if row.Status != "PASS" || row.ProcessRecovery != "PASS" {
				t.Fatal("ordinary recovery changed", string(raw))
			}
			want := "NOT_COVERED"
			eligible := 0
			if v != "c5-zero-pair" {
				eligible = 3
			}
			affected := v == "c5-unavailable" || v == "c5-sweep-shape"
			reason := ""
			if affected {
				reason = "no GONE clock"
			}
			if row.UnavailableKeys.Status != want || row.UnavailableKeys.Reason != reason || row.UnavailableKeys.EligibleKeys != eligible || (row.UnavailableKeys.AffectedReads > 0) != affected {
				t.Fatal("coverage omitted or eligibility mistaken for observation", string(raw))
			}
			if v == "c5-sweep-shape" && row.UnavailableKeys.AffectedReads != 1 {
				t.Fatal("sweep-shaped single failure was not exercised", string(raw))
			}
		})
	}
}
