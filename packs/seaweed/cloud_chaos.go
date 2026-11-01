package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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

// mainPID binds $pid/$start to the unit's live MainPID (systemd reports 0
// for a unit whose process is gone, which refuses).
func mainPID(unit string) string {
	return `pid=$(systemctl show -p MainPID --value ` + cloudQuote(unit) + `); test "$pid" -gt 0; start=$(` + chaosStat + `); `
}

func chaosKillScript(unit string) string {
	return mainPID(unit) + `echo "OLD $pid $start"; sudo -n kill -KILL "$pid"; ` +
		`for i in $(seq 300); do if [ "$(` + chaosStat + `)" != "$start" ]; then echo GONE; exit 0; fi; sleep 0.1; done; exit 1`
}

func chaosIdentityScript(unit string) string { return mainPID(unit) + `echo "NEW $pid $start"` }

func chaosStopScript(unit string) string {
	return mainPID(unit) + `echo "PID $pid $start"; sudo -n kill -STOP "$pid"; ` +
		`for i in $(seq 200); do all=1; ` + chaosThreads + `if [ "$s" != T ]; then all=0; fi; done; if [ $all = 1 ] && [ "$(` + chaosStat + `)" = "$start" ]; then echo STOPPED; exit 0; fi; sleep 0.05; done; exit 1`
}

func chaosContScript(pid, start string) string {
	return `pid=` + pid + `; [ "$(` + chaosStat + `)" = ` + cloudQuote(start) + ` ]; sudo -n kill -CONT "$pid"; ` +
		`for i in $(seq 200); do none=1; ` + chaosThreads + `if [ "$s" = T ]; then none=0; fi; done; if [ $none = 1 ]; then echo LIVE; exit 0; fi; sleep 0.05; done; exit 1`
}

type chaosRun struct {
	dir   string
	nodes map[string]*infra.Node
	mu    sync.Mutex
	seq   int
}

func (r *chaosRun) step(ctx context.Context, id, name string, seconds int, cmd string) (string, error) {
	r.mu.Lock()
	r.seq++
	file := fmt.Sprintf("cloud-step-%03d.json", r.seq)
	r.mu.Unlock()
	return cloudStep(ctx, r.nodes[id], r.dir, file, name, seconds, cmd)
}

// each runs one step per client concurrently and returns every stdout.
func (r *chaosRun) each(ctx context.Context, name string, seconds int, cmd func(id string) string) (map[string]string, error) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	var failure error
	out := map[string]string{}
	for _, id := range chaosClients {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			s, e := r.step(ctx, id, name+"-"+id, seconds, cmd(id))
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
func (r *chaosRun) stamp(ctx context.Context, label string) (map[string]float64, error) {
	out, e := r.each(ctx, "stamp-"+label, 10, func(string) string { return chaosStamp })
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
		n := cloudNode(h, c.SSHKey, c.HostKeys[id])
		if e := n.ConnectContext(ctx); e != nil {
			return fmt.Errorf("chaos connect %s: %w", id, e)
		}
		defer n.Close()
		nodes[id] = n
	}
	// A restart replaces a role's unit, so later cases target the live one.
	gens := map[string]int{"volume": 1, "filer": 1}
	for _, x := range chaosCases {
		dir := filepath.Join(c.Output, "chaos", x.Name)
		if e := os.MkdirAll(dir, 0700); e != nil {
			return e
		}
		if e := runChaosCase(ctx, d, x, hosts, gens, &chaosRun{dir: dir, nodes: nodes}); e != nil {
			return fmt.Errorf("chaos %s: %w", x.Name, e)
		}
	}
	return nil
}

// chaosUnit names generation gen of a role: deploy's unit, then <role>-g<gen>.
func chaosUnit(role string, gen int) string {
	if gen == 1 {
		return role
	}
	return role + "-g" + strconv.Itoa(gen)
}

func runChaosCase(ctx context.Context, d inventory.Document, x chaosCase, hosts map[string]inventory.Host, gens map[string]int, r *chaosRun) error {
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
	seeds, e := r.each(ctx, "seed", 120, func(id string) string { return cmds[id]["seed"] })
	if e != nil {
		return e
	}
	for id, out := range seeds {
		if _, e = checkReadOutput([]byte(out), cfgs[id], "seed", "http"); e != nil {
			return fmt.Errorf("%s seed: %w", id, e)
		}
	}
	// Freeze exposure counts only keys whose seeded fid is on the frozen
	// server; the filer may serve any other key from vol2..vol6.
	onVictim := map[string]map[string]bool{}
	if x.Freeze {
		status, e := r.step(ctx, x.Victim, "owners", 30, "curl -fsS --max-time 10 "+cloudQuote("http://"+endpoint(vol, "volume_http")+"/status"))
		if e != nil {
			return e
		}
		if onVictim, e = chaosOwnedKeys(seeds, status); e != nil {
			return e
		}
	}

	loadCtx, cancelLoad := context.WithCancel(ctx)
	defer cancelLoad()
	var loads map[string]string
	var loadErr error
	loaded := make(chan struct{})
	go func() {
		defer close(loaded)
		loads, loadErr = r.each(loadCtx, "load", 160, func(id string) string { return cmds[id]["load"] })
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
		if marks["P"], e = r.stamp(ctx, "P"); e != nil {
			return e
		}
		unit := "sw-cloud-" + d.RunID + "-" + chaosUnit(x.Role, gens[x.Role])
		if x.Freeze {
			return chaosFreeze(ctx, r, x.Victim, unit, marks, witness)
		}
		if e = chaosRestart(ctx, r, d, x, hosts, unit, chaosUnit(x.Role, gens[x.Role]+1), witness); e == nil {
			gens[x.Role]++
		}
		return e
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
	exposed := map[string]bool{}
	for _, id := range chaosClients {
		observed, e := checkReadOutput([]byte(loads[id]), cfgs[id], "load", "http", ReadWindow{marks["P"][id]})
		if e == nil && x.Freeze {
			exposed[id], e = checkCloudFreeze(observed, freezeWindow{marks["P"][id], marks["S"][id], marks["C"][id], marks["R"][id]}, onVictim[id])
		}
		if e != nil {
			return fmt.Errorf("%s load: %w", id, e)
		}
	}
	if x.Freeze {
		if e = save("exposure.json", exposed); e != nil {
			return e
		}
		if !exposed["client1"] && !exposed["client2"] && !exposed["client3"] {
			return errors.New("freeze NOT_COVERED: no client read a key held by the frozen server")
		}
	}
	recovered, e := r.each(ctx, "recovery", 60, func(id string) string { return cmds[id]["recovery"] })
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

// chaosOwnedKeys joins each client's seed ACK fids to the victim's volume ids.
func chaosOwnedKeys(seeds map[string]string, status string) (map[string]map[string]bool, error) {
	var s struct{ Volumes []struct{ Id uint64 } }
	if e := json.Unmarshal([]byte(status), &s); e != nil || len(s.Volumes) == 0 {
		return nil, errors.New("frozen server volume list missing")
	}
	held := map[string]bool{}
	for _, v := range s.Volumes {
		held[strconv.FormatUint(v.Id, 10)] = true
	}
	owned := map[string]map[string]bool{}
	for id, out := range seeds {
		owned[id] = map[string]bool{}
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			var row struct {
				Event, Key string
				FIDs       []string
			}
			if json.Unmarshal([]byte(line), &row) == nil && row.Event == "ACK" {
				for _, fid := range row.FIDs {
					volume, _, _ := strings.Cut(fid, ",")
					owned[id][row.Key] = owned[id][row.Key] || held[volume]
				}
			}
		}
	}
	return owned, nil
}

// checkCloudFreeze applies the shared exposure check to the GETs of keys on
// the frozen server. A client with no such GET is not exposed (no error);
// one with any must show a GET spanning [S,C] and one starting after R.
func checkCloudFreeze(observed map[string]any, w freezeWindow, onVictim map[string]bool) (bool, error) {
	var rows []any
	for _, v := range observed["outcomes"].([]any) {
		if key, _ := v.(map[string]any)["key"].(string); onVictim[key] {
			rows = append(rows, v)
		}
	}
	if len(rows) == 0 {
		return false, nil
	}
	return true, checkFreezeExposure(map[string]any{"outcomes": rows}, w)
}

// chaosRestart kills the unit's exact MainPID, waits for that generation to be
// gone, and starts the same command as unit next; readiness is the deploy URL.
func chaosRestart(ctx context.Context, r *chaosRun, d inventory.Document, x chaosCase, hosts map[string]inventory.Host, unit, next string, witness map[string]string) error {
	old, e := r.step(ctx, x.Victim, "kill-"+x.Role, 60, chaosKillScript(unit))
	witness["old"] = old
	if e != nil {
		return e
	}
	h, root := hosts[x.Victim], hosts[x.Victim].RunRoot
	if _, e = r.step(ctx, x.Victim, "start-"+next, 60, cloudStart(d, root, next, cloudServices(h, hosts["meta"], root, true)[x.Role])); e != nil {
		return e
	}
	if witness["new"], e = r.step(ctx, x.Victim, "identity-"+next, 30, chaosIdentityScript("sw-cloud-"+d.RunID+"-"+next)); e != nil {
		return e
	}
	before, after := strings.Fields(old), strings.Fields(witness["new"])
	if len(before) != 4 || before[0] != "OLD" || before[3] != "GONE" || len(after) != 3 || after[0] != "NEW" || (before[1] == after[1] && before[2] == after[2]) {
		return errors.New("replacement generation identity did not change")
	}
	url := "http://" + endpoint(h, "volume_http") + "/status"
	if x.Role == "filer" {
		url = "http://" + endpoint(h, "filer_http") + "/"
	}
	_, e = r.step(ctx, x.Victim, "ready-"+next, 45, "until curl -fsS --max-time 3 "+cloudQuote(url)+" >/dev/null; do sleep 1; done")
	return e
}

// chaosFreeze: S after every thread is T, C before CONT, R after none is T.
func chaosFreeze(ctx context.Context, r *chaosRun, victim, unit string, marks map[string]map[string]float64, witness map[string]string) error {
	out, e := r.step(ctx, victim, "stop", 30, chaosStopScript(unit))
	witness["stop"] = out
	if e != nil {
		return e
	}
	f := strings.Fields(out)
	if len(f) != 4 || f[0] != "PID" || f[3] != "STOPPED" {
		return errors.New("stopped witness malformed")
	}
	if marks["S"], e = r.stamp(ctx, "S"); e != nil {
		return e
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(chaosHold):
	}
	if marks["C"], e = r.stamp(ctx, "C"); e != nil {
		return e
	}
	if witness["cont"], e = r.step(ctx, victim, "cont", 30, chaosContScript(f[1], f[2])); e != nil {
		return e
	}
	if strings.TrimSpace(witness["cont"]) != "LIVE" {
		return errors.New("live witness malformed")
	}
	marks["R"], e = r.stamp(ctx, "R")
	return e
}
