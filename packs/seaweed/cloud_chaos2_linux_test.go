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
	"sync"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/inventory"
	"golang.org/x/crypto/ssh"
)

// Fake ten-node cluster for stage 2. Each client's n-th marker in a case is
// base+30+25n on its own clock; ops run one per second from base+0.5. vol_i
// holds volumes i and i-1, so volume 3 has both copies on {vol3, vol4}.
type chaos2Fake struct {
	mu      sync.Mutex
	variant string
	log     []string
	stamps  map[string]int
	killed  map[string]bool
	pid     int
	cases   map[string]string // client -> current case name
}

func (f *chaos2Fake) handle(id string) func(string, ssh.Channel) uint32 {
	return func(cmd string, ch ssh.Channel) uint32 {
		inner := cmd
		if i := strings.Index(cmd, "sh -ec '"); i >= 0 {
			inner = strings.ReplaceAll(cmd[i+8:len(cmd)-1], `'"'"'`, `'`)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		rec := func(k string) { f.log = append(f.log, id+":"+k) }
		unit := ""
		if i := strings.Index(inner, "--value '"); i >= 0 {
			unit = inner[i+9 : i+9+strings.Index(inner[i+9:], "'")]
		}
		if f.killed[id+"/"+unit] {
			rec("dead-unit " + unit)
			return 1
		}
		base := chaosBase[id]
		switch {
		case strings.Contains(inner, "time.monotonic"):
			rec("stamp")
			fmt.Fprintf(ch, "%.6f\n", base+30+25*float64(f.stamps[id]))
			f.stamps[id]++
		case strings.Contains(inner, "chaos_load.py") && strings.Contains(inner, "--stage load"):
			f.stamps[id] = 0
			name := inner[strings.Index(inner, "/chaos-")+7:]
			f.cases[id] = name[:strings.Index(name, ".journal")]
			rec("load " + f.cases[id])
			if f.variant == "load-fail" {
				fmt.Fprintln(ch, `{"status": "FAIL", "code": "CHAOS_LOAD_FAILED", "error": "TimeoutError: native completion deadline"}`)
				return 1
			}
			fmt.Fprint(ch, f.load(id, base))
		case strings.Contains(inner, "--stage readback"):
			rec("readback")
			if f.variant == "readback-fail" {
				fmt.Fprintln(ch, `{"status": "FAIL"}`)
				return 1
			}
			fmt.Fprintln(ch, `{"status": "PASS", "stage": "readback"}`)
		case strings.HasPrefix(inner, "cat "):
			rec("journal")
			fmt.Fprintln(ch, `{"event": "ACK", "kind": "kv", "key": "kU", "fids": ["3,ab"]}`)
			fmt.Fprintln(ch, `{"event": "ACK", "kind": "kv", "key": "kA", "fids": ["1,ab"]}`)
			fmt.Fprintln(ch, `{"event": "ACK", "kind": "kv", "key": "kN", "fids": null}`)
		case strings.Contains(inner, "SWCHAOS") && strings.Contains(inner, "-N SWCHAOS"):
			rec("partition")
			if f.variant != "no-partition-witness" {
				fmt.Fprintln(ch, "PARTITIONED")
			}
		case strings.Contains(inner, "SWCHAOS"):
			rec("heal")
			fmt.Fprintln(ch, "HEALED")
		case strings.Contains(inner, "modprobe -n -v sch_netem"):
			rec("netem-probe")
			fmt.Fprintln(ch, "KERNEL 6.8.0-1015-azure\nSCH_NETEM AVAILABLE insmod /lib/modules/6.8.0-1015-azure/kernel/net/sched/sch_netem.ko.zst")
		case strings.Contains(inner, "netem delay"):
			rec("netem")
			if f.variant == "netem-missing" {
				return 1
			}
			fmt.Fprintln(ch, "NETEM eth0")
		case strings.Contains(inner, "qdisc del"):
			rec("clear")
			fmt.Fprintln(ch, "CLEARED")
		case strings.Contains(inner, "kill -KILL"):
			rec("kill " + unit)
			f.killed[id+"/"+unit] = true
			fmt.Fprint(ch, "OLD 100 555\nGONE\n")
		case strings.Contains(inner, `echo "NEW`):
			f.pid++
			rec("identity " + unit)
			fmt.Fprintf(ch, "NEW %d 777\n", 1000+f.pid)
		case strings.Contains(inner, "systemd-run"):
			rec("start")
		case strings.Contains(inner, "/status") && !strings.Contains(inner, "until"):
			rec("placement")
			var i int
			fmt.Sscanf(id, "vol%d", &i)
			fmt.Fprintf(ch, `{"Volumes":[{"Id":%d},{"Id":%d}]}`, i, (i+4)%6+1)
		case strings.Contains(inner, "curl"):
			rec("ready")
		default:
			rec("unknown " + inner)
			return 1
		}
		return 0
	}
}

func (f *chaos2Fake) load(id string, base float64) string {
	seconds := 0
	for _, set := range chaos2Sets {
		for _, x := range set {
			if x.Name == f.cases[id] {
				seconds = x.Seconds
			}
		}
	}
	kinds := []string{"kv_set", "kv_get", "s3_put", "s3_get"}
	ops := []chaosOp{}
	for i := 0; i < seconds; i++ {
		s := base + 0.5 + float64(i)
		o := chaosOp{Op: i, Kind: kinds[i%4], Key: fmt.Sprintf("k%d", i), Started: s, Ended: s + .5, Exact: true}
		if o.Kind == "kv_get" {
			o.Key = "kA"
		}
		switch {
		case f.variant == "outside-window" && id == "client2" && s > base+125 && s < base+126:
			o.Exact = false // latest P=base+55 (restart), fails at base+125.5: past P+60
		case strings.HasPrefix(f.variant, "c5-") && f.cases[id] == "C5-two-kill" && o.Kind == "kv_get" && s > base+117 && s < base+125:
			o.Exact = false // past restart(base+55)+60, before ready(base+80)+60
			if f.variant == "c5-unavailable" {
				o.Key = "kU" // both copies on {vol3, vol4}
			}
		}
		ops = append(ops, o)
	}
	raw, _ := json.Marshal(map[string]any{"status": "OBSERVED", "stage": "load", "outcomes": ops})
	return string(raw) + "\n"
}

func chaos2Input(t *testing.T, variant, set string) (inventory.Document, CloudConfig, *chaos2Fake) {
	t.Helper()
	old := []time.Duration{chaos2Down, chaos2Settle, chaos2Netem}
	chaos2Down, chaos2Settle, chaos2Netem = 10*time.Millisecond, 10*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { chaos2Down, chaos2Settle, chaos2Netem = old[0], old[1], old[2] })
	f := &chaos2Fake{variant: variant, stamps: map[string]int{}, killed: map[string]bool{}, cases: map[string]string{}}
	var d inventory.Document
	address, _, _, _ := cloudSSH(t, "")
	json.Unmarshal(cloudInput(t, address), &d)
	base := d.Hosts[0]
	d.Hosts = nil
	c := CloudConfig{Output: t.TempDir(), HostKeys: map[string]string{}, Replication: "001", ChaosSet: set}
	for i, id := range []string{"meta", "vol1", "vol2", "vol3", "vol4", "vol5", "vol6", "client1", "client2", "client3"} {
		address, pin, key, _ := cloudSSH(t, "", f.handle(id))
		h := base
		h.ID, h.Data.Address = id, fmt.Sprintf("10.84.1.%d", 10+i)
		h.Management.Address, _, _ = net.SplitHostPort(address)
		_, port, _ := net.SplitHostPort(address)
		fmt.Sscan(port, &h.Management.Port)
		switch {
		case id == "meta":
			h.Roles = []string{"master", "filer", "s3"}
			h.Services = []inventory.Service{{Name: "master_http", Port: 9333}, {Name: "master_grpc", Port: 19333}, {Name: "filer_http", Port: 8888}, {Name: "filer_grpc", Port: 18888}, {Name: "s3_http", Port: 8333}}
		case strings.HasPrefix(id, "vol"):
			h.Roles = []string{"volume"}
			h.Services = []inventory.Service{{Name: "volume_http", Port: 8080}, {Name: "volume_grpc", Port: 18080}}
		default:
			h.Roles, h.Services = []string{"client"}, []inventory.Service{}
		}
		c.SSHKey, c.HostKeys[id] = key, pin
		d.Hosts = append(d.Hosts, h)
	}
	return d, c, f
}

func runChaos2Fake(t *testing.T, variant, set string) (*chaos2Fake, error) {
	d, c, f := chaos2Input(t, variant, set)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return f, runCloudChaos2(ctx, d, c)
}

func TestChaos2SetsRunEveryCase(t *testing.T) {
	for _, set := range []string{"A", "B"} {
		t.Run(set, func(t *testing.T) {
			f, e := runChaos2Fake(t, "", set)
			if e != nil {
				t.Fatal(e, f.log)
			}
			log := strings.Join(f.log, " ")
			for _, want := range map[string][]string{
				// C2 restarts vol1 again after C1: it must kill C1's -g2 unit.
				"A": {"vol1:netem-probe", "vol1:kill sw-cloud-pack-cpu-volume ", "vol1:kill sw-cloud-pack-cpu-volume-g2 ", "vol2:partition", "vol2:heal", "vol6:identity sw-cloud-pack-cpu-volume-g2"},
				"B": {"meta:kill sw-cloud-pack-cpu-master ", "meta:kill sw-cloud-pack-cpu-filer ", "vol3:kill", "vol4:kill", "vol1:netem", "vol1:clear"},
			}[set] {
				if !strings.Contains(log+" ", want) {
					t.Fatalf("missing %q in %s", want, log)
				}
			}
			if (set == "B") == strings.Contains(log, "netem-probe") {
				t.Fatal("netem probe must run in set A only:", log)
			}
			if strings.Contains(log, "dead-unit") || strings.Contains(log, "unknown") {
				t.Fatal(log)
			}
		})
	}
}

// codex01 #56: a failed load and a failed markers.json save must both reach
// the error; the save error alone hid which client's load failed.
func TestChaos2FailedLoadSurvivesSaveError(t *testing.T) {
	d, c, f := chaos2Input(t, "load-fail", "A")
	if e := os.MkdirAll(filepath.Join(c.Output, "chaos", "C1-single-kill", "markers.json"), 0700); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	e := runCloudChaos2(ctx, d, c)
	if e == nil || !strings.Contains(e.Error(), "load-client") || !strings.Contains(e.Error(), "markers.json") {
		t.Fatal("want both the load and the save error:", e, f.log)
	}
}

func TestChaos2Refuses(t *testing.T) {
	for _, tc := range []struct{ variant, set string }{
		{"outside-window", "A"}, {"c5-available-key", "B"}, {"no-partition-witness", "A"}, {"netem-missing", "B"}, {"readback-fail", "A"},
	} {
		t.Run(tc.variant, func(t *testing.T) {
			if f, e := runChaos2Fake(t, tc.variant, tc.set); e == nil {
				t.Fatal("false PASS", f.log)
			} else {
				t.Log(e)
			}
		})
	}
	// The same late failures on a key whose both copies were on {vol3, vol4}
	// are excused until ready+60 (ruling Q6).
	if f, e := runChaos2Fake(t, "c5-unavailable", "B"); e != nil {
		t.Fatal(e, f.log)
	}
}
