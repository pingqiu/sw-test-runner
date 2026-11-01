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

// Only a successful, complete rule listing can prove the chain absent; a
// failed listing is UNKNOWN and refuses (codex01 #49 R3).
func chaosPartitionHealScript() string {
	return `sudo -n sh -c ` + cloudQuote(chaosHeal) + `; rules=$(sudo -n iptables -S) || exit 1; if printf '%s\n' "$rules" | grep -q SWCHAOS; then exit 1; fi; echo HEALED`
}

const chaosNetemProbe = `echo "KERNEL $(uname -r)"; if out=$(modprobe -n -v sch_netem 2>&1); then echo "SCH_NETEM AVAILABLE $out"; else echo "SCH_NETEM MISSING $out"; fi; true`

func chaosNetemScript(metaIP, timer string) string {
	return chaosDev(metaIP) + `modinfo sch_netem >/dev/null; sudo -n systemd-run --unit=` + cloudQuote(timer) + ` --on-active=150 /bin/sh -c "tc qdisc del dev $dev root || true"; ` +
		`sudo -n tc qdisc add dev "$dev" root netem delay 50ms 10ms loss 1%; q=$(tc qdisc show dev "$dev"); printf '%s\n' "$q" | grep -q netem; echo "NETEM $dev"`
}

// As for the partition: only a successful qdisc listing can prove netem gone.
func chaosNetemClearScript(metaIP string) string {
	return chaosDev(metaIP) + `sudo -n tc qdisc del dev "$dev" root; q=$(tc qdisc show dev "$dev") || exit 1; if printf '%s\n' "$q" | grep -q netem; then exit 1; fi; echo CLEARED`
}

func runCloudChaos2(ctx context.Context, d inventory.Document, c CloudConfig) error {
	set, ok := chaos2Sets[c.ChaosSet]
	if !ok || c.Replication != "001" {
		return errors.New("chaos set A|B needs replication 001")
	}
	hosts, nodes, release, err := chaosConnect(ctx, d, c, append([]string{"meta", "vol1", "vol2", "vol3", "vol4", "vol5", "vol6"}, chaosClients...))
	if err != nil {
		return err
	}
	defer release()
	// Read-only probe so run B knows before C6 whether netem exists (Claude01
	// 6ae71cae): kernel version and a dry-run modprobe; never installs, never fails.
	if c.ChaosSet == "A" {
		dir := filepath.Join(c.Output, "chaos", "netem-probe")
		if e := os.MkdirAll(dir, 0700); e != nil {
			return e
		}
		if _, e := (&chaosRun{dir: dir, nodes: nodes}).step(ctx, "vol1", "netem-probe", 30, chaosNetemProbe); e != nil {
			return e
		}
	}
	g := &chaosGens{gens: map[string]int{}, witness: map[string]string{}}
	for _, x := range set {
		dir := filepath.Join(c.Output, "chaos", x.Name)
		if e := os.MkdirAll(dir, 0700); e != nil {
			return e
		}
		if e := runChaos2Case(ctx, d, x, hosts, g, &chaosRun{dir: dir, rep: c.Replication, nodes: nodes}); e != nil {
			return fmt.Errorf("chaos %s: %w", x.Name, e)
		}
	}
	return nil
}

// chaosGens tracks each host/role's live unit generation (1 = deploy's unit)
// and the last kill witness. C5 kills two hosts in parallel, so every access
// takes the lock (codex01 #49 R4).
type chaosGens struct {
	mu      sync.Mutex
	gens    map[string]int
	witness map[string]string
}

func (g *chaosGens) unit(d inventory.Document, id, role string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.gens[id+"/"+role] == 0 {
		g.gens[id+"/"+role] = 1
	}
	return "sw-cloud-" + d.RunID + "-" + chaosUnit(role, g.gens[id+"/"+role])
}

func (g *chaosGens) next(id, role string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return chaosUnit(role, max(g.gens[id+"/"+role], 1)+1)
}

func (g *chaosGens) advance(id, role string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gens[id+"/"+role] = max(g.gens[id+"/"+role], 1) + 1
}

func (g *chaosGens) note(key, value string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.witness[key] = value
}

func (g *chaosGens) noted(key string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.witness[key]
}

// chaos2Commands builds each client's load and read-back command from the
// typed SmokeConfig, with every error propagated.
func chaos2Commands(d inventory.Document, x chaos2Case, hosts map[string]inventory.Host, rep string) (map[string]map[string]string, error) {
	s3 := "http://" + endpoint(hosts["meta"], "s3_http")
	cmds := map[string]map[string]string{}
	for _, id := range chaosClients {
		root := hosts[id].RunRoot
		view := d
		view.State, view.Hosts = "resolved", []inventory.Host{hosts["meta"], hosts["vol1"], hosts[id]}
		args, e := SmokeArgs(&view, id, SmokeSettings{NativeDir: root, HelperDir: root + "/worker", Keys: 16, Length: 1 << 20, Replication: rep})
		if e != nil {
			return nil, e
		}
		var cfg SmokeConfig
		if e = json.Unmarshal([]byte(args[3]), &cfg); e != nil {
			return nil, e
		}
		cfg.Client, cfg.Root = id+"-"+x.Name, "/testops/"+d.RunID+"/"+id+"-"+x.Name
		raw, e := json.Marshal(cfg)
		if e != nil {
			return nil, e
		}
		cmds[id] = map[string]string{}
		for _, stage := range []string{"load", "readback"} {
			cmds[id][stage] = "python3 -I " + cloudQuote(root+"/worker/chaos_load.py") + " --config " + cloudQuote(string(raw)) + " --s3 " + cloudQuote(s3) +
				" --stage " + stage + " --seconds " + strconv.Itoa(x.Seconds) + " --journal " + cloudQuote(root+"/chaos-"+x.Name+".journal")
		}
	}
	return cmds, nil
}

func runChaos2Case(ctx context.Context, d inventory.Document, x chaos2Case, hosts map[string]inventory.Host, g *chaosGens, r *chaosRun) error {
	cmds, err := chaos2Commands(d, x, hosts, r.rep)
	if err != nil {
		return err
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
	if _, e := r.step(ctx, "meta", "s3-ready", 30, "until curl -fsS --max-time 3 "+cloudQuote("http://"+endpoint(hosts["meta"], "s3_http")+"/")+" >/dev/null; do sleep 1; done"); e != nil {
		return e
	}
	cancelLoad, waitLoad := r.startLoad(ctx, x.Seconds+60, func(id string) string { return cmds[id]["load"] })
	defer cancelLoad()
	f := &chaos2Fault{r: r, d: d, x: x, hosts: hosts, g: g, witness: map[string]string{}}
	faultErr := f.run(ctx)
	if faultErr != nil {
		cancelLoad()
	}
	loads, loadErr := waitLoad()
	if e := errors.Join(faultErr, r.save("markers.json", map[string]any{"marks": f.marks, "ready": f.ready}), r.save("fault.json", f.witness), r.save("placement.json", placement)); e != nil {
		return e
	}
	if loadErr != nil {
		// Run 7f lost which op hung: keep each client's SUBMIT/ACK journal
		// (saved as this case's step records) before the error ends the run.
		for _, id := range chaosClients {
			_, _ = r.step(ctx, id, "journal-"+id, 30, "cat "+cloudQuote(hosts[id].RunRoot+"/chaos-"+x.Name+".journal"))
		}
		return loadErr
	}
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
		w := chaosWindows{Unavailable: map[string]bool{}, ReadyMono: f.ready[id]}
		for _, m := range f.marks {
			w.Marks = append(w.Marks, m[id])
		}
		// C5: keys whose every copy sat on the killed pair may be unavailable.
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
	back, e := r.each(ctx, "readback", 300, func(id string) string { return cmds[id]["readback"] })
	if e != nil {
		return e
	}
	for id, out := range back {
		if !strings.Contains(out, `"status": "PASS"`) {
			return fmt.Errorf("%s read-back did not PASS", id)
		}
	}
	return r.save("result.json", results)
}

// chaos2Fault runs one case's fault actions in strict order. Every step is
// checked before the next is sent, so a failed marker or kill stops the case
// before any later action (codex01 #49 R1). Only the independent victims of
// one action run in parallel, after their marker succeeded.
type chaos2Fault struct {
	r       *chaosRun
	d       inventory.Document
	x       chaos2Case
	hosts   map[string]inventory.Host
	g       *chaosGens
	marks   []map[string]float64
	ready   map[string]float64
	witness map[string]string
}

func (f *chaos2Fault) mark(ctx context.Context, label string) error {
	m, e := f.r.stamp(ctx, label)
	if e == nil {
		f.marks = append(f.marks, m)
	}
	return e
}

func (f *chaos2Fault) each(ids []string, act func(id string) error) error {
	var wg sync.WaitGroup
	errs := make([]error, len(ids))
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) { defer wg.Done(); errs[i] = act(id) }(i, id)
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (f *chaos2Fault) kill(ctx context.Context, id, role string) error {
	old, e := chaosKill(ctx, f.r, id, role, f.g.unit(f.d, id, role))
	f.g.note("kill-"+id+"-"+role, old)
	return e
}

func (f *chaos2Fault) start(ctx context.Context, id, role string) error {
	identity, e := chaosStart(ctx, f.r, f.d, f.hosts, id, role, f.g.next(id, role), f.g.noted("kill-"+id+"-"+role))
	f.g.note("start-"+id+"-"+role, identity)
	if e == nil {
		f.g.advance(id, role)
	}
	return e
}

func (f *chaos2Fault) wait(ctx context.Context, t time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(t):
		return nil
	}
}

// step runs one witnessed action on the victim and requires its witness line.
func (f *chaos2Fault) step(ctx context.Context, name, script, want string) error {
	out, e := f.r.step(ctx, f.x.Victims[0], name, 60, script)
	f.witness[name] = out
	if e != nil {
		return e
	}
	if got := strings.TrimSpace(out); got != want && !(want == "NETEM" && strings.HasPrefix(got, "NETEM ")) {
		return fmt.Errorf("%s witness missing: %q", name, got)
	}
	return nil
}

// sequence runs each step only after the previous one succeeded.
func sequence(steps ...func() error) error {
	for _, step := range steps {
		if e := step(); e != nil {
			return e
		}
	}
	return nil
}

func (f *chaos2Fault) run(ctx context.Context) error {
	role := func(id string) string {
		if id == "meta" {
			return "master"
		}
		return "volume"
	}
	mark := func(label string) func() error { return func() error { return f.mark(ctx, label) } }
	wait := func(t time.Duration) func() error { return func() error { return f.wait(ctx, t) } }
	if e := f.wait(ctx, chaos2Settle); e != nil {
		return e
	}
	switch f.x.Kind {
	case "kill-hold":
		return sequence(mark("kill"),
			func() error { return f.each(f.x.Victims, func(id string) error { return f.kill(ctx, id, role(id)) }) },
			wait(chaos2Down), mark("restart"),
			func() error { return f.each(f.x.Victims, func(id string) error { return f.start(ctx, id, role(id)) }) },
			func() (e error) { f.ready, e = f.r.stamp(ctx, "ready"); return e })
	case "rolling", "meta":
		for _, id := range f.x.Victims {
			roles, settle := []string{"volume"}, chaos2Settle
			if f.x.Kind == "meta" {
				roles, settle = []string{"master", "filer"}, 0
			}
			for _, rl := range roles {
				if e := sequence(mark("kill-"+id+"-"+rl), func() error { return f.kill(ctx, id, rl) },
					mark("restart-"+id+"-"+rl), func() error { return f.start(ctx, id, rl) }, wait(settle)); e != nil {
					return e
				}
			}
		}
	case "partition", "netem":
		timer := "sw-cloud-" + f.d.RunID + "-autoheal-" + f.x.Name
		on, off, onWant, offWant, hold := chaosPartitionScript(timer), chaosPartitionHealScript(), "PARTITIONED", "HEALED", chaos2Down
		if f.x.Kind == "netem" {
			ip := f.hosts["meta"].Data.Address
			on, off, onWant, offWant, hold = chaosNetemScript(ip, timer), chaosNetemClearScript(ip), "NETEM", "CLEARED", chaos2Netem
		}
		return sequence(mark(f.x.Kind), func() error { return f.step(ctx, f.x.Kind, on, onWant) },
			wait(hold), mark("heal"), func() error { return f.step(ctx, "heal", off, offWant) })
	}
	return nil
}
