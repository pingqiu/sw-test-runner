package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pingqiu/sw-test-runner/infra"
	"github.com/pingqiu/sw-test-runner/inventory"
)

// Cloud chaos runs the three read-only S0b cases against the cluster's own
// services. The controller drives every step over the pinned SSH sessions. Each
// marker is stamped on every load client's own CLOCK_MONOTONIC (the worker's
// clock), so each client's GETs are judged against its own P by the unchanged
// checkWindow. No clock sync, custody, policy or GO.
type chaosCase struct {
	Name, Victim, Role string
	Freeze             bool
}

var chaosCases = []chaosCase{{"volume-restart", "vol1", "volume", false}, {"filer-restart", "meta", "filer", false}, {"volume-freeze", "vol1", "volume", true}}

// Load runs 120 s; the fault starts after the settle and a freeze holds >=20 s.
var chaosSettle, chaosHold = 30 * time.Second, 20 * time.Second

var chaosClients = []string{"client1", "client2", "client3"}

const chaosStamp = "python3 -c 'import time;print(repr(time.monotonic()))'"

// Field 20 after "comm) " is starttime; field 1 is the state.
const chaosStat = `sed 's/.*) //' /proc/$pid/stat 2>/dev/null | cut -d' ' -f20`
const chaosThreads = `for f in /proc/$pid/task/*/stat; do s=$(sed 's/.*) //' "$f" | cut -d' ' -f1); `

type chaosRun struct {
	dir string
	mu  sync.Mutex
	seq int
}

func (r *chaosRun) step(ctx context.Context, n *infra.Node, name string, seconds int, cmd string) (string, error) {
	r.mu.Lock()
	r.seq++
	file := fmt.Sprintf("step-%03d.json", r.seq)
	r.mu.Unlock()
	command := fmt.Sprintf("timeout -k 5 %d sh -ec ", seconds) + cloudQuote(cmd)
	out, stderr, rc, e := n.Run(ctx, command)
	w := writeCloudStep(r.dir, file, name, command, out, stderr, rc, e)
	if e != nil || rc != 0 || w != nil {
		return out, fmt.Errorf("chaos step %s rc=%d stderr=%s: %w", name, rc, stderr, errors.Join(e, w, errors.New("failed")))
	}
	return out, nil
}

// each runs one step per client concurrently and returns every stdout.
func (r *chaosRun) each(ctx context.Context, nodes map[string]*infra.Node, name string, seconds int, cmd func(id string) string) (map[string]string, error) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	var failure error
	out := map[string]string{}
	for _, id := range chaosClients {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			s, e := r.step(ctx, nodes[id], name+"-"+id, seconds, cmd(id))
			mu.Lock()
			defer mu.Unlock()
			out[id], failure = s, errors.Join(failure, e)
		}(id)
	}
	wg.Wait()
	return out, failure
}

// stamp returns every client's own monotonic marker, or an error before any
// caller proceeds: a missing marker means the next fault step is not sent.
func (r *chaosRun) stamp(ctx context.Context, nodes map[string]*infra.Node, label string) (map[string]float64, error) {
	out, e := r.each(ctx, nodes, "stamp-"+label, 10, func(string) string { return chaosStamp })
	if e != nil {
		return nil, e
	}
	marks := map[string]float64{}
	for id, s := range out {
		v, e := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if e != nil || v <= 0 || math.IsInf(v, 0) {
			return nil, fmt.Errorf("client %s marker %s invalid", id, label)
		}
		marks[id] = v
	}
	return marks, nil
}

func runCloudChaos(ctx context.Context, d inventory.Document, c CloudConfig) error {
	hosts := map[string]inventory.Host{}
	for _, h := range d.Hosts {
		hosts[h.ID] = h
	}
	nodes := map[string]*infra.Node{}
	for _, id := range append([]string{"meta", "vol1"}, chaosClients...) {
		h, ok := hosts[id]
		if !ok || c.HostKeys[id] == "" {
			return fmt.Errorf("chaos host %s missing or unpinned", id)
		}
		n := &infra.Node{Host: net.JoinHostPort(h.Management.Address, strconv.Itoa(h.Management.Port)), User: h.Management.User, KeyFile: c.SSHKey, HostKey: c.HostKeys[id]}
		if e := n.ConnectContext(ctx); e != nil {
			return fmt.Errorf("chaos connect %s: %w", id, e)
		}
		defer n.Close()
		nodes[id] = n
	}
	for _, x := range chaosCases {
		dir := filepath.Join(c.Output, "chaos", x.Name)
		if e := os.MkdirAll(dir, 0700); e != nil {
			return e
		}
		if e := runChaosCase(ctx, d, x, hosts, nodes, &chaosRun{dir: dir}); e != nil {
			return fmt.Errorf("chaos %s: %w", x.Name, e)
		}
	}
	return nil
}

func runChaosCase(ctx context.Context, d inventory.Document, x chaosCase, hosts map[string]inventory.Host, nodes map[string]*infra.Node, r *chaosRun) error {
	meta, vol := hosts["meta"], hosts["vol1"]
	cmds, cfgs := map[string]map[string]string{}, map[string]SmokeConfig{}
	for _, id := range chaosClients {
		cmds[id] = map[string]string{}
		for _, stage := range []string{"seed", "load", "recovery"} {
			view := d
			view.State, view.Hosts = "resolved", []inventory.Host{meta, vol, hosts[id]}
			root := hosts[id].RunRoot
			args, e := ReadLoadArgs(&view, id, SmokeSettings{NativeDir: root, HelperDir: root + "/worker", Keys: 16, Length: 1 << 20}, stage, "http")
			if e != nil {
				return e
			}
			var cfg SmokeConfig
			if e = json.Unmarshal([]byte(args[3]), &cfg); e != nil {
				return e
			}
			// Fresh keys per case: the client name carries the case.
			cfg.Client = id + "-" + x.Name
			cfg.Root = "/testops/" + d.RunID + "/" + cfg.Client
			raw, _ := json.Marshal(cfg)
			args[3], cfgs[id] = string(raw), cfg
			cmd := "python3"
			for _, arg := range args {
				cmd += " " + cloudQuote(arg)
			}
			cmds[id][stage] = cmd
		}
	}
	save := func(name string, v any) error {
		b, _ := json.MarshalIndent(v, "", "  ")
		return os.WriteFile(filepath.Join(r.dir, name), b, 0600)
	}
	seeds, e := r.each(ctx, nodes, "seed", 120, func(id string) string { return cmds[id]["seed"] })
	if e != nil {
		return e
	}
	for id, out := range seeds {
		if _, e = checkReadOutput([]byte(out), cfgs[id], "seed", "http"); e != nil {
			return fmt.Errorf("%s seed: %w", id, e)
		}
	}

	loadCtx, cancelLoad := context.WithCancel(ctx)
	defer cancelLoad()
	var loads map[string]string
	var loadErr error
	loaded := make(chan struct{})
	go func() {
		defer close(loaded)
		loads, loadErr = r.each(loadCtx, nodes, "load", 160, func(id string) string { return cmds[id]["load"] })
	}()
	marks, witness := map[string]map[string]float64{}, map[string]string{}
	fault := func() error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(chaosSettle):
		}
		var e error
		// P on every client before the fault is sent.
		if marks["P"], e = r.stamp(ctx, nodes, "P"); e != nil {
			return e
		}
		victim, unit := nodes[x.Victim], "sw-cloud-"+d.RunID+"-"+x.Role
		if x.Freeze {
			return chaosFreeze(ctx, r, nodes, victim, unit, marks, witness)
		}
		return chaosRestart(ctx, r, d, x, hosts, victim, unit, witness)
	}
	faultErr := fault()
	if faultErr != nil {
		cancelLoad()
	}
	<-loaded
	if e = errors.Join(faultErr, save("markers.json", marks), save("fault.json", witness)); e != nil {
		return e
	}
	if loadErr != nil {
		return loadErr
	}
	for _, id := range chaosClients {
		observed, e := checkReadOutput([]byte(loads[id]), cfgs[id], "load", "http", ReadWindow{marks["P"][id]})
		if e == nil && x.Freeze {
			e = checkFreezeExposure(observed, freezeWindow{marks["P"][id], marks["S"][id], marks["C"][id], marks["R"][id]})
		}
		if e != nil {
			return fmt.Errorf("%s load: %w", id, e)
		}
	}
	recovered, e := r.each(ctx, nodes, "recovery", 60, func(id string) string { return cmds[id]["recovery"] })
	if e != nil {
		return e
	}
	for id, out := range recovered {
		if _, e = checkReadOutput([]byte(out), cfgs[id], "recovery", "http"); e != nil {
			return fmt.Errorf("%s recovery: %w", id, e)
		}
	}
	return nil
}

// chaosRestart kills the unit's exact MainPID, waits for that generation to be
// gone, and starts the same command as a new unit; readiness is the deploy URL.
func chaosRestart(ctx context.Context, r *chaosRun, d inventory.Document, x chaosCase, hosts map[string]inventory.Host, victim *infra.Node, unit string, witness map[string]string) error {
	kill := `pid=$(systemctl show -p MainPID --value ` + cloudQuote(unit) + `); test "$pid" -gt 0; start=$(` + chaosStat + `); echo "OLD $pid $start"; sudo -n kill -KILL "$pid"; ` +
		`for i in $(seq 300); do if [ "$(` + chaosStat + `)" != "$start" ]; then echo GONE; exit 0; fi; sleep 0.1; done; exit 1`
	old, e := r.step(ctx, victim, "kill-"+x.Role, 60, kill)
	witness["old"] = old
	if e != nil {
		return e
	}
	h, root := hosts[x.Victim], hosts[x.Victim].RunRoot
	if _, e = r.step(ctx, victim, "start-"+x.Role+"-g2", 60, cloudStart(d, root, x.Role+"-g2", cloudServices(h, hosts["meta"], root, true)[x.Role])); e != nil {
		return e
	}
	identity := `pid=$(systemctl show -p MainPID --value ` + cloudQuote(unit+"-g2") + `); test "$pid" -gt 0; echo "NEW $pid $(` + chaosStat + `)"`
	if witness["new"], e = r.step(ctx, victim, "identity-"+x.Role+"-g2", 30, identity); e != nil {
		return e
	}
	before, after := strings.Fields(old), strings.Fields(witness["new"])
	if len(before) < 4 || before[3] != "GONE" || len(after) != 3 || (before[1] == after[1] && before[2] == after[2]) {
		return errors.New("replacement generation identity did not change")
	}
	url := "http://" + endpoint(h, "volume_http") + "/status"
	if x.Role == "filer" {
		url = "http://" + endpoint(h, "filer_http") + "/"
	}
	_, e = r.step(ctx, victim, "ready-"+x.Role+"-g2", 45, "until curl -fsS --max-time 3 "+cloudQuote(url)+" >/dev/null; do sleep 1; done")
	return e
}

// chaosFreeze: S after every thread is T, C before CONT, R after none is T.
func chaosFreeze(ctx context.Context, r *chaosRun, nodes map[string]*infra.Node, victim *infra.Node, unit string, marks map[string]map[string]float64, witness map[string]string) error {
	stop := `pid=$(systemctl show -p MainPID --value ` + cloudQuote(unit) + `); test "$pid" -gt 0; start=$(` + chaosStat + `); echo "PID $pid $start"; sudo -n kill -STOP "$pid"; ` +
		`for i in $(seq 200); do all=1; ` + chaosThreads + `if [ "$s" != T ]; then all=0; fi; done; if [ $all = 1 ] && [ "$(` + chaosStat + `)" = "$start" ]; then echo STOPPED; exit 0; fi; sleep 0.05; done; exit 1`
	out, e := r.step(ctx, victim, "stop", 30, stop)
	witness["stop"] = out
	if e != nil {
		return e
	}
	f := strings.Fields(out)
	if len(f) != 4 || f[0] != "PID" || f[3] != "STOPPED" {
		return errors.New("stopped witness malformed")
	}
	if marks["S"], e = r.stamp(ctx, nodes, "S"); e != nil {
		return e
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(chaosHold):
	}
	if marks["C"], e = r.stamp(ctx, nodes, "C"); e != nil {
		return e
	}
	cont := `pid=` + f[1] + `; [ "$(` + chaosStat + `)" = ` + cloudQuote(f[2]) + ` ]; sudo -n kill -CONT "$pid"; ` +
		`for i in $(seq 200); do none=1; ` + chaosThreads + `if [ "$s" = T ]; then none=0; fi; done; if [ $none = 1 ]; then echo LIVE; exit 0; fi; sleep 0.05; done; exit 1`
	if witness["cont"], e = r.step(ctx, victim, "cont", 30, cont); e != nil {
		return e
	}
	if strings.TrimSpace(witness["cont"]) != "LIVE" {
		return errors.New("live witness malformed")
	}
	marks["R"], e = r.stamp(ctx, nodes, "R")
	return e
}
