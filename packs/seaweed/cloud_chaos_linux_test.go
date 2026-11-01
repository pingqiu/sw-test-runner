//go:build linux

package seaweed

import (
	"context"
	"crypto/sha256"
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

// Fake clients have unrelated monotonic clocks, so a GET judged against
// another client's P cannot pass: the per-client join is exercised.
var chaosBase = map[string]float64{"client1": 9000, "client2": 1000, "client3": 50000}

type chaosFake struct {
	mu      sync.Mutex
	variant string
	log     []string
	stamps  map[string]int
	killed  map[string]bool // like systemd: a killed unit reports MainPID 0
}

func (f *chaosFake) record(kind string) {
	f.mu.Lock()
	f.log = append(f.log, kind)
	f.mu.Unlock()
}

func chaosCounters(reads, writes, errs, length int) map[string]any {
	l := map[string]any{"transport": "http", "http_fallback_allowed": false}
	for _, p := range []string{"direct_dc", "direct_rc"} {
		l[p+"_reads"], l[p+"_bytes"] = 0, 0
	}
	l["read_objects"], l["read_bytes"], l["copied_reads"], l["copied_bytes"] = reads, reads*length, reads, reads*length
	l["write_objects"], l["write_bytes"] = writes, writes*length
	return map[string]any{"read_errors": errs, "loader": l}
}

// Seeded key i lives on volume 3 (vol1 in the fake) when even, else volume 4.
func chaosSeed(c SmokeConfig, volume func(int) int) string {
	var b strings.Builder
	for i := 0; i < c.Keys; i++ {
		key := fmt.Sprintf("%s-%d", c.Client, i)
		block := sha256.Sum256([]byte(c.RunID + "/" + key))
		h := sha256.New()
		for n := 0; n < c.Length; n += len(block) {
			h.Write(block[:min(len(block), c.Length-n)])
		}
		digest := fmt.Sprintf("%x", h.Sum(nil))
		for _, row := range []map[string]any{{"event": "ACK", "key": key, "sequence": i + 1, "length": c.Length, "sha256": digest, "fids": []string{fmt.Sprintf("%d,01ab", volume(i))}}, {"event": "EXACT", "key": key, "length": c.Length, "sha256": digest}} {
			raw, _ := json.Marshal(row)
			b.Write(append(raw, '\n'))
		}
	}
	raw, _ := json.Marshal(map[string]any{"status": "PASS", "ack": c.Keys, "exact": c.Keys, "wrong": 0, "witness": map[string]any{"read_transport": "http", "read_bytes": c.Keys * c.Length, "fallback": false, "loader_write_bytes": c.Keys * c.Length}, "before": chaosCounters(0, 0, 0, c.Length), "after": chaosCounters(c.Keys, c.Keys, 0, c.Length)})
	return b.String() + string(raw) + "\n"
}

// One GET per second from base+0.5; failed GETs are those fail() selects.
func chaosRead(c SmokeConfig, stage string, base float64, fail func(float64) bool) string {
	n := c.Keys
	if stage == "load" {
		n = 124
	}
	outcomes, faults, exact := []map[string]any{}, []map[string]any{}, 0
	var first, last any
	for i := 0; i < n; i++ {
		s := base + 0.5 + float64(i)
		ok := stage != "load" || !fail(s)
		key := fmt.Sprintf("%s-%d", c.Client, i%c.Keys)
		outcomes = append(outcomes, map[string]any{"operation_id": i, "key": key, "started": s, "ended": s + .5, "exact": ok})
		if ok {
			exact++
			if first == nil {
				first = s + .5
			}
			last = s + .5
		} else {
			faults = append(faults, map[string]any{"action": "get", "key": key, "operation_id": i, "target": "", "transport": "http", "started": s, "ended": s + .5, "ok": false, "error": "reset", "results": nil})
		}
	}
	ended := base + float64(n) + 1
	raw, _ := json.Marshal(map[string]any{"status": "OBSERVED", "stage": stage, "transport": "http", "run_id": c.RunID, "client": c.Client, "root": c.Root, "exact": exact, "attempts": n, "faults": faults, "outcomes": outcomes, "first_exact": first, "last_exact": last, "started": base, "ended": ended, "elapsed": ended - base, "before": chaosCounters(0, 0, 0, c.Length), "after": chaosCounters(n, 0, len(faults), c.Length)})
	return string(raw) + "\n"
}

func (f *chaosFake) handle(id string) func(string, ssh.Channel) uint32 {
	return func(cmd string, ch ssh.Channel) uint32 {
		inner := cmd
		if i := strings.Index(cmd, "sh -ec '"); i >= 0 {
			inner = strings.ReplaceAll(cmd[i+8:len(cmd)-1], `'"'"'`, `'`)
		}
		freeze := strings.Contains(inner, "-volume-freeze")
		unit := ""
		if i := strings.Index(inner, "--value '"); i >= 0 {
			unit = inner[i+9 : i+9+strings.Index(inner[i+9:], "'")]
		}
		f.mu.Lock()
		dead := f.killed[unit]
		f.mu.Unlock()
		if dead {
			f.record("dead-unit")
			return 1
		}
		switch {
		case strings.Contains(inner, "time.monotonic"):
			f.record("stamp")
			if f.variant == "stamp-fail" && id == "client3" {
				return 1
			}
			f.mu.Lock()
			n := f.stamps[id]
			f.stamps[id]++
			f.mu.Unlock()
			offsets := []float64{30, 31, 52, 53}
			if f.variant == "short-hold" {
				offsets = []float64{30, 31, 41, 42}
			}
			fmt.Fprintf(ch, "%.6f\n", chaosBase[id]+offsets[n%4])
		case strings.Contains(inner, "kv_smoke.py"):
			i := strings.Index(inner, `'--config' '`) + 12
			var c SmokeConfig
			json.Unmarshal([]byte(inner[i:i+strings.Index(inner[i:], `'`)]), &c)
			stage := strings.Fields(inner[strings.Index(inner, "'--stage'"):])[1]
			stage = strings.Trim(stage, "'")
			f.record(stage)
			base := chaosBase[id]
			if stage == "seed" {
				f.mu.Lock()
				f.stamps[id] = 0
				f.mu.Unlock()
				volume := func(i int) int { return 3 + i%2 }
				if f.variant == "one-client-unowned" && id == "client2" {
					volume = func(int) int { return 5 }
				}
				fmt.Fprint(ch, chaosSeed(c, volume))
				return 0
			}
			p := base + 30
			fail := func(s float64) bool { return !freeze && s > p && s < p+4 }
			if f.variant == "pre-P" && id == "client2" {
				fail = func(s float64) bool { return s > p-1 && s < p+3 }
			}
			fmt.Fprint(ch, chaosRead(c, stage, base, fail))
		case strings.Contains(inner, "kill -KILL"):
			f.record("kill")
			f.mu.Lock()
			f.killed[unit] = true
			f.mu.Unlock()
			switch f.variant {
			case "no-gone":
				fmt.Fprint(ch, "OLD 100 555\nWAITING\n")
			case "bad-old":
				fmt.Fprint(ch, "OID 100 555\nGONE\n")
			default:
				fmt.Fprint(ch, "OLD 100 555\nGONE\n")
			}
		case strings.Contains(inner, `echo "NEW`):
			f.record("identity")
			if f.variant == "same-pid" {
				fmt.Fprint(ch, "NEW 100 555\n")
			} else if f.variant == "bad-new" {
				fmt.Fprint(ch, "NEX 200 777\n")
			} else {
				fmt.Fprint(ch, "NEW 200 777\n")
			}
		case strings.Contains(inner, "kill -STOP"):
			f.record("stop")
			if f.variant == "not-stopped" {
				return 1
			}
			fmt.Fprint(ch, "PID 100 555\nSTOPPED\n")
		case strings.Contains(inner, "kill -CONT"):
			f.record("cont")
			if f.variant != "no-live" {
				fmt.Fprint(ch, "LIVE\n")
			}
		case strings.Contains(inner, "systemd-run"):
			f.record("start")
		case strings.Contains(inner, "curl") && !strings.Contains(inner, "until"):
			f.record("owners")
			if f.variant == "unowned" {
				fmt.Fprint(ch, `{"Volumes":[{"Id":7}]}`)
			} else {
				fmt.Fprint(ch, `{"Volumes":[{"Id":3},{"Id":9}]}`)
			}
		case strings.Contains(inner, "curl"):
			f.record("ready")
		default:
			f.record("unknown:" + inner)
			return 1
		}
		return 0
	}
}

func chaosInput(t *testing.T, variant string) (inventory.Document, CloudConfig, *chaosFake) {
	t.Helper()
	old := []time.Duration{chaosSettle, chaosHold}
	chaosSettle, chaosHold = 10*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { chaosSettle, chaosHold = old[0], old[1] })
	f := &chaosFake{variant: variant, stamps: map[string]int{}, killed: map[string]bool{}}
	var d inventory.Document
	address, _, _, _ := cloudSSH(t, "")
	json.Unmarshal(cloudInput(t, address), &d)
	d.State = "requested"
	base := d.Hosts[0]
	d.Hosts = nil
	c := CloudConfig{Output: t.TempDir(), HostKeys: map[string]string{}, Chaos: true}
	for i, id := range []string{"meta", "vol1", "client1", "client2", "client3"} {
		address, pin, key, _ := cloudSSH(t, "", f.handle(id))
		h := base
		h.ID, h.Data.Address = id, fmt.Sprintf("10.84.1.%d", 10+i)
		h.Management.Address, _, _ = net.SplitHostPort(address)
		_, port, _ := net.SplitHostPort(address)
		fmt.Sscan(port, &h.Management.Port)
		switch id {
		case "meta":
			h.Roles = []string{"master", "filer", "s3"}
			h.Services = []inventory.Service{{Name: "master_http", Port: 9333}, {Name: "master_grpc", Port: 19333}, {Name: "filer_http", Port: 8888}, {Name: "filer_grpc", Port: 18888}, {Name: "s3_http", Port: 8333}}
		case "vol1":
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

func runChaosFake(t *testing.T, variant string) (*chaosFake, CloudConfig, error) {
	d, c, f := chaosInput(t, variant)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return f, c, runCloudChaos(ctx, d, c)
}

// The stamp order is what makes each client's join sound: every P before the
// fault, S after the all-T witness, C before CONT, R after the LIVE witness.
func TestCloudChaosStampOrderAndPerClientJoin(t *testing.T) {
	f, c, e := runChaosFake(t, "")
	if e != nil {
		t.Fatal(e, f.log)
	}
	var faults []string
	for _, k := range f.log {
		if k != "seed" && k != "load" && k != "recovery" && k != "owners" {
			faults = append(faults, k)
		}
	}
	s3 := "stamp stamp stamp "
	restart := s3 + "kill start identity ready "
	want := restart + restart + s3 + "stop " + s3 + s3 + "cont " + s3
	if got := strings.Join(faults, " ") + " "; got != want {
		t.Fatalf("fault/stamp order:\n got %s\nwant %s", got, want)
	}
	for _, x := range chaosCases {
		for _, name := range []string{"markers.json", "fault.json"} {
			if _, e := os.Stat(filepath.Join(c.Output, "chaos", x.Name, name)); e != nil {
				t.Fatal(e)
			}
		}
	}
}

func TestCloudChaosRefuses(t *testing.T) {
	for _, variant := range []string{"pre-P", "stamp-fail", "same-pid", "short-hold", "not-stopped", "unowned", "no-gone", "bad-old", "bad-new", "no-live"} {
		t.Run(variant, func(t *testing.T) {
			f, _, e := runChaosFake(t, variant)
			if e == nil {
				t.Fatal("false PASS", f.log)
			}
			t.Log(e)
			if variant == "stamp-fail" && strings.Contains(strings.Join(f.log, " "), "kill") {
				t.Fatal("fault sent without every client's P", f.log)
			}
		})
	}
}

// Chaos is opt-in: without cloud.chaos the cluster run never reaches it, and
// the deploy commands are the same strings the run used before the refactor.
func TestCloudChaosIsOptIn(t *testing.T) {
	for _, chaos := range []bool{false, true} {
		t.Run(fmt.Sprint(chaos), func(t *testing.T) {
			calls := 0
			old := runChaos
			runChaos = func(context.Context, inventory.Document, CloudConfig) error { calls++; return nil }
			defer func() { runChaos = old }()
			raw, c, loads := clusterInput(t, false)
			c.Chaos = chaos
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, e := RunCloudHTTP(ctx, raw, c); e != nil || loads.Load() != 3 {
				t.Fatal(e, loads.Load())
			}
			if want := map[bool]int{false: 0, true: 1}[chaos]; calls != want {
				t.Fatalf("chaos calls %d want %d", calls, want)
			}
		})
	}
	h := inventory.Host{Data: inventory.DataNetwork{Address: "10.84.1.11"}, Services: []inventory.Service{{Name: "volume_http", Port: 8080}, {Name: "volume_grpc", Port: 18080}, {Name: "master_http", Port: 9333}, {Name: "filer_http", Port: 8888}, {Name: "filer_grpc", Port: 18888}, {Name: "s3_http", Port: 8333}}}
	meta := h
	meta.Data.Address = "10.84.1.10"
	got := cloudServices(h, meta, "/r", true)
	want := map[string]string{
		"master": `'/r/weed' master -ip=10.84.1.11 -port=9333 -mdir='/r/master' -volumeSizeLimitMB=256 -defaultReplication=000 -peers=none`,
		"volume": `'/r/weed-volume' --master=10.84.1.10:9333 --ip=10.84.1.11 --port=8080 --port.grpc=18080 --dir='/r/volume' --max=16`,
		"filer":  `'/r/weed' -config_dir='/r' filer -master=10.84.1.10:9333 -ip=10.84.1.11 -port=8888 -port.grpc=18888`,
		"s3":     `'/r/weed' s3 -filer=10.84.1.10:8888 -port=8333`,
	}
	for role, cmd := range want {
		if got[role] != cmd {
			t.Fatalf("%s command changed:\n got %s\nwant %s", role, got[role], cmd)
		}
	}
	if s := cloudStart(inventory.Document{RunID: "r1"}, "/r", "volume", "X"); s != `sudo systemd-run --unit='sw-cloud-r1-volume' --property=WorkingDirectory='/r' X` {
		t.Fatal(s)
	}
}

// A client with no seeded key on the frozen server is recorded, not failed;
// the others must still show a frozen-server GET spanning the hold.
func TestCloudChaosFreezeCoverageIsPerOwner(t *testing.T) {
	if f, _, e := runChaosFake(t, "one-client-unowned"); e != nil {
		t.Fatal(e, f.log)
	}
	var outcomes []any
	for s := 0.5; s < 120; s += 0.75 { // claude03's control: serial unblocked 0.5 s GETs
		outcomes = append(outcomes, map[string]any{"key": "k-1", "started": s, "ended": s + 0.5})
	}
	w := freezeWindow{PreStop: 30, Stopped: 31, PreCont: 52, Resumed: 53}
	observed := map[string]any{"outcomes": outcomes}
	if exposed, e := checkCloudFreeze(observed, w, map[string]bool{"k-0": true}); e != nil || exposed {
		t.Fatal("GETs of keys not on the frozen server counted", exposed, e)
	}
	if exposed, e := checkCloudFreeze(observed, w, map[string]bool{"k-1": true}); e != nil || !exposed {
		t.Fatal("frozen-server GETs spanning the hold refused", exposed, e)
	}
}
