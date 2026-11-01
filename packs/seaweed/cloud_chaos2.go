package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pingqiu/sw-test-runner/infra"
	"github.com/pingqiu/sw-test-runner/inventory"
)

// Stage 2 (contract claude01 7c52e26a, rulings 6ae6e92a): replication 001,
// mixed chaos_load.py on three clients, a marker on every client's own clock
// before each fault action, per-op excusal, then exact read-back of every ACK.
type chaos2Case struct {
	Name    string
	Kind    string // kill-hold, rolling, partition, meta, netem
	Victims []string
	Seconds int // load duration; outlasts the last action by >= 60 s
}

var chaos2Sets = map[string][]chaos2Case{
	"A": {{"C1-single-kill", "kill-hold", []string{"vol1"}, 210}, {"C2-rolling", "rolling", []string{"vol1", "vol2", "vol3", "vol4", "vol5", "vol6"}, 360}, {"C3-partition", "partition", []string{"vol2"}, 180}},
	"B": {{"C4-meta-restart", "meta", []string{"meta"}, 150}, {"C5-two-kill", "kill-hold", []string{"vol3", "vol4"}, 210}, {"C6-netem", "netem", []string{"vol1"}, 240}},
}

// Fault holds; tests shorten them.
var chaos2Down, chaos2Settle, chaos2Netem = 60 * time.Second, 30 * time.Second, 120 * time.Second

// chaosHeal removes the partition chain; it is idempotent and also the
// on-node auto-heal timer's command, installed before the partition.
const chaosHeal = `iptables -D INPUT -j SWCHAOS 2>/dev/null || true; iptables -D OUTPUT -j SWCHAOS 2>/dev/null || true; iptables -F SWCHAOS 2>/dev/null || true; iptables -X SWCHAOS 2>/dev/null || true`

// chaosDev binds $dev to the NIC that carries cluster data to meta.
func chaosDev(metaIP string) string {
	return `dev=$(ip -o route get ` + cloudQuote(metaIP) + ` | sed -n 's/.* dev \([^ ]*\).*/\1/p'); test -n "$dev"; `
}

func chaosPartitionScript(timer string) string {
	return `sudo -n systemd-run --unit=` + cloudQuote(timer) + ` --on-active=150 /bin/sh -c ` + cloudQuote(chaosHeal) + `; ` +
		`sudo -n iptables -N SWCHAOS; for r in "-i lo" "-o lo" "-p tcp --dport 22" "-p tcp --sport 22"; do sudo -n iptables -A SWCHAOS $r -j RETURN; done; sudo -n iptables -A SWCHAOS -j DROP; ` +
		`sudo -n iptables -I INPUT 1 -j SWCHAOS; sudo -n iptables -I OUTPUT 1 -j SWCHAOS; sudo -n iptables -C INPUT -j SWCHAOS; sudo -n iptables -C OUTPUT -j SWCHAOS; echo PARTITIONED`
}

func chaosPartitionHealScript() string {
	return `sudo -n sh -c ` + cloudQuote(chaosHeal) + `; if sudo -n iptables -S SWCHAOS >/dev/null 2>&1; then exit 1; fi; echo HEALED`
}

const chaosNetemProbe = `echo "KERNEL $(uname -r)"; if out=$(modprobe -n -v sch_netem 2>&1); then echo "SCH_NETEM AVAILABLE $out"; else echo "SCH_NETEM MISSING $out"; fi; true`

func chaosNetemScript(metaIP, timer string) string {
	return chaosDev(metaIP) + `modinfo sch_netem >/dev/null; sudo -n systemd-run --unit=` + cloudQuote(timer) + ` --on-active=150 /bin/sh -c "tc qdisc del dev $dev root || true"; ` +
		`sudo -n tc qdisc add dev "$dev" root netem delay 50ms 10ms loss 1%; tc qdisc show dev "$dev" | grep -q netem; echo "NETEM $dev"`
}

func chaosNetemClearScript(metaIP string) string {
	return chaosDev(metaIP) + `sudo -n tc qdisc del dev "$dev" root; if tc qdisc show dev "$dev" | grep -q netem; then exit 1; fi; echo CLEARED`
}

func runCloudChaos2(ctx context.Context, d inventory.Document, c CloudConfig) error {
	set, ok := chaos2Sets[c.ChaosSet]
	if !ok || c.Replication != "001" {
		return errors.New("chaos set A|B needs replication 001")
	}
	hosts := map[string]inventory.Host{}
	for _, h := range d.Hosts {
		hosts[h.ID] = h
	}
	nodes := map[string]*infra.Node{}
	for _, id := range append([]string{"meta", "vol1", "vol2", "vol3", "vol4", "vol5", "vol6"}, chaosClients...) {
		h, ok := hosts[id]
		if !ok || c.HostKeys[id] == "" {
			return fmt.Errorf("chaos host %s missing or unpinned", id)
		}
		n := cloudNode(h, c.SSHKey, c.HostKeys[id])
		if e := n.ConnectContext(ctx); e != nil {
			return fmt.Errorf("chaos connect %s: %w", id, e)
		}
		defer n.Close()
		nodes[id] = n
	}
	// Read-only probe so run B knows before C6 whether netem exists (Claude01
	// 6ae71cae): kernel version and a dry-run modprobe; never installs, never fails.
	if c.ChaosSet == "A" {
		dir := filepath.Join(c.Output, "chaos", "netem-probe")
		if e := os.MkdirAll(dir, 0700); e != nil {
			return e
		}
		probe := &chaosRun{dir: dir, nodes: nodes}
		if _, e := probe.step(ctx, "vol1", "netem-probe", 30, chaosNetemProbe); e != nil {
			return e
		}
	}
	gens := map[string]int{} // host/role -> live generation (1 = deploy's unit)
	for _, x := range set {
		dir := filepath.Join(c.Output, "chaos", x.Name)
		if e := os.MkdirAll(dir, 0700); e != nil {
			return e
		}
		if e := runChaos2Case(ctx, d, x, hosts, gens, &chaosRun{dir: dir, rep: c.Replication, nodes: nodes}); e != nil {
			return fmt.Errorf("chaos %s: %w", x.Name, e)
		}
	}
	return nil
}

func runChaos2Case(ctx context.Context, d inventory.Document, x chaos2Case, hosts map[string]inventory.Host, gens map[string]int, r *chaosRun) error {
	meta, s3 := hosts["meta"], "http://"+endpoint(hosts["meta"], "s3_http")
	save := func(name string, v any) error {
		b, _ := json.MarshalIndent(v, "", "  ")
		return os.WriteFile(filepath.Join(r.dir, name), b, 0600)
	}
	// Volume placement before the fault: volume id -> the hosts holding a copy.
	placement := map[string][]string{}
	for _, id := range []string{"vol1", "vol2", "vol3", "vol4", "vol5", "vol6"} {
		out, e := r.step(ctx, id, "placement-"+id, 30, "curl -fsS --max-time 10 "+cloudQuote("http://"+endpoint(hosts[id], "volume_http")+"/status"))
		var st struct{ Volumes []struct{ Id uint64 } }
		if e != nil || json.Unmarshal([]byte(out), &st) != nil {
			return errors.Join(e, errors.New("volume placement unreadable on "+id))
		}
		for _, v := range st.Volumes {
			vid := strconv.FormatUint(v.Id, 10)
			placement[vid] = append(placement[vid], id)
		}
	}
	cmd := func(id, stage string) string {
		root := hosts[id].RunRoot
		view := d
		view.State, view.Hosts = "resolved", []inventory.Host{meta, hosts["vol1"], hosts[id]}
		args, _ := SmokeArgs(&view, id, SmokeSettings{NativeDir: root, HelperDir: root + "/worker", Keys: 16, Length: 1 << 20, Replication: r.rep})
		var cfg map[string]any
		json.Unmarshal([]byte(args[3]), &cfg)
		cfg["client"], cfg["root"] = id+"-"+x.Name, "/testops/"+d.RunID+"/"+id+"-"+x.Name
		raw, _ := json.Marshal(cfg)
		return "python3 -I " + cloudQuote(root+"/worker/chaos_load.py") + " --config " + cloudQuote(string(raw)) + " --s3 " + cloudQuote(s3) +
			" --stage " + stage + " --seconds " + strconv.Itoa(x.Seconds) + " --journal " + cloudQuote(root+"/chaos-"+x.Name+".journal")
	}
	if _, e := r.step(ctx, "meta", "s3-ready", 30, "until curl -fsS --max-time 3 "+cloudQuote(s3+"/")+" >/dev/null; do sleep 1; done"); e != nil {
		return e
	}
	loadCtx, cancelLoad := context.WithCancel(ctx)
	defer cancelLoad()
	var loads map[string]string
	var loadErr error
	loaded := make(chan struct{})
	go func() {
		defer close(loaded)
		loads, loadErr = r.each(loadCtx, "load", x.Seconds+60, func(id string) string { return cmd(id, "load") })
	}()
	var marks []map[string]float64
	ready := map[string]float64{}
	witness := map[string]string{}
	wait := func(t time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(t):
			return nil
		}
	}
	mark := func(label string) error { // before every fault action, on every client
		m, e := r.stamp(ctx, label)
		marks = append(marks, m)
		return e
	}
	parallel := func(ids []string, f func(id string) error) error {
		var wg sync.WaitGroup
		errs := make([]error, len(ids))
		for i, id := range ids {
			wg.Add(1)
			go func(i int, id string) { defer wg.Done(); errs[i] = f(id) }(i, id)
		}
		wg.Wait()
		return errors.Join(errs...)
	}
	role := func(id string) string {
		if id == "meta" {
			return "master"
		}
		return "volume"
	}
	unit := func(id, rl string) string {
		if gens[id+"/"+rl] == 0 {
			gens[id+"/"+rl] = 1
		}
		return "sw-cloud-" + d.RunID + "-" + chaosUnit(rl, gens[id+"/"+rl])
	}
	var mu sync.Mutex
	kill := func(id, rl string) error {
		old, e := chaosKill(ctx, r, id, rl, unit(id, rl))
		mu.Lock()
		witness["kill-"+id+"-"+rl] = old
		mu.Unlock()
		return e
	}
	start := func(id, rl string) error {
		mu.Lock()
		old, next := witness["kill-"+id+"-"+rl], chaosUnit(rl, gens[id+"/"+rl]+1)
		mu.Unlock()
		identity, e := chaosStart(ctx, r, d, hosts, id, rl, next, old)
		mu.Lock()
		witness["start-"+id+"-"+rl] = identity
		if e == nil {
			gens[id+"/"+rl]++
		}
		mu.Unlock()
		return e
	}
	fault := func() error {
		if e := wait(chaos2Settle); e != nil {
			return e
		}
		switch x.Kind {
		case "kill-hold":
			if e := errors.Join(mark("kill"), parallel(x.Victims, func(id string) error { return kill(id, role(id)) })); e != nil {
				return e
			}
			if e := errors.Join(wait(chaos2Down), mark("restart"), parallel(x.Victims, func(id string) error { return start(id, role(id)) })); e != nil {
				return e
			}
			m, e := r.stamp(ctx, "ready")
			ready = m
			return e
		case "rolling":
			for _, id := range x.Victims {
				if e := errors.Join(mark("kill-"+id), kill(id, "volume"), mark("restart-"+id), start(id, "volume"), wait(chaos2Settle)); e != nil {
					return e
				}
			}
		case "meta":
			for _, rl := range []string{"master", "filer"} {
				if e := errors.Join(mark("kill-"+rl), kill("meta", rl), mark("restart-"+rl), start("meta", rl)); e != nil {
					return e
				}
			}
		case "partition":
			if e := mark("partition"); e != nil {
				return e
			}
			out, e := r.step(ctx, x.Victims[0], "partition", 60, chaosPartitionScript("sw-cloud-"+d.RunID+"-autoheal-"+x.Name))
			if witness["partition"] = out; e != nil || strings.TrimSpace(out) != "PARTITIONED" {
				return errors.Join(e, errors.New("partition witness missing"))
			}
			if e = errors.Join(wait(chaos2Down), mark("heal")); e != nil {
				return e
			}
			out, e = r.step(ctx, x.Victims[0], "heal", 60, chaosPartitionHealScript())
			if witness["heal"] = out; e != nil || strings.TrimSpace(out) != "HEALED" {
				return errors.Join(e, errors.New("heal witness missing"))
			}
		case "netem":
			ip := meta.Data.Address
			if e := mark("netem"); e != nil {
				return e
			}
			out, e := r.step(ctx, x.Victims[0], "netem", 60, chaosNetemScript(ip, "sw-cloud-"+d.RunID+"-autoheal-"+x.Name))
			if witness["netem"] = out; e != nil || !strings.HasPrefix(strings.TrimSpace(out), "NETEM ") {
				return errors.Join(e, errors.New("netem witness missing"))
			}
			if e = errors.Join(wait(chaos2Netem), mark("clear")); e != nil {
				return e
			}
			out, e = r.step(ctx, x.Victims[0], "clear", 60, chaosNetemClearScript(ip))
			if witness["clear"] = out; e != nil || strings.TrimSpace(out) != "CLEARED" {
				return errors.Join(e, errors.New("netem clear witness missing"))
			}
		}
		return nil
	}
	faultErr := fault()
	if faultErr != nil {
		cancelLoad()
	}
	<-loaded
	if e := errors.Join(faultErr, save("markers.json", map[string]any{"marks": marks, "ready": ready}), save("fault.json", witness), save("placement.json", placement)); e != nil {
		return e
	}
	if loadErr != nil {
		return loadErr
	}
	// C5: keys whose every copy sat on the killed pair may be unavailable.
	victims := append([]string{}, x.Victims...)
	sort.Strings(victims)
	results := map[string]any{}
	for _, id := range chaosClients {
		lines := strings.Split(strings.TrimSpace(loads[id]), "\n")
		var out struct {
			Status   string
			Outcomes []chaosOp
		}
		if json.Unmarshal([]byte(lines[len(lines)-1]), &out) != nil || out.Status != "OBSERVED" {
			return fmt.Errorf("%s load output missing", id)
		}
		w := chaosWindows{Unavailable: map[string]bool{}, ReadyMono: ready[id]}
		for _, m := range marks {
			w.Marks = append(w.Marks, m[id])
		}
		if x.Kind == "kill-hold" && len(victims) == 2 {
			journal, e := r.step(ctx, id, "journal-"+id, 30, "cat "+cloudQuote(hosts[id].RunRoot+"/chaos-"+x.Name+".journal"))
			if e != nil {
				return e
			}
			for _, line := range strings.Split(strings.TrimSpace(journal), "\n") {
				var a struct {
					Key  string
					FIDs []string
				}
				if json.Unmarshal([]byte(line), &a) != nil || len(a.FIDs) == 0 {
					continue // unknown placement is never excused
				}
				vid, _, _ := strings.Cut(a.FIDs[0], ",")
				held := append([]string{}, placement[vid]...)
				sort.Strings(held)
				w.Unavailable[a.Key] = strings.Join(held, ",") == strings.Join(victims, ",")
			}
		}
		if e := checkChaosOps(out.Outcomes, w); e != nil {
			return fmt.Errorf("%s: %w", id, e)
		}
		failed := 0
		for _, o := range out.Outcomes {
			if !o.Exact {
				failed++
			}
		}
		results[id] = map[string]int{"ops": len(out.Outcomes), "failed": failed}
	}
	back, e := r.each(ctx, "readback", 300, func(id string) string { return cmd(id, "readback") })
	if e != nil {
		return e
	}
	for id, out := range back {
		if !strings.Contains(out, `"status": "PASS"`) {
			return fmt.Errorf("%s read-back did not PASS", id)
		}
	}
	return save("result.json", results)
}
