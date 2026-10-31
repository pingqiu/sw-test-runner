package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
	"math"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

type GETOutcome struct {
	ID             *int `json:"operation_id"`
	Key            string
	Started, Ended float64
	Exact          *bool
}
type ReadWindow struct{ PreKill float64 }

// Only client-local ordered outcomes earn credit. M02 timestamps are not used.
func checkWindow(rows []GETOutcome, faults map[int]GETOutcome, attempts int, loadStart, loadEnd float64, c SmokeConfig, w ReadWindow) error {
	if len(rows) != attempts || math.IsNaN(w.PreKill) || math.IsInf(w.PreKill, 0) || w.PreKill <= 0 {
		return errors.New("outage outcomes/pre-Kill marker missing")
	}
	prior, begin, end := 0., 0., 0.
	for i, r := range rows {
		fault, failed := faults[i]
		if r.ID == nil || *r.ID != i || r.Key != c.Client+"-"+strconv.Itoa(i%c.Keys) || r.Exact == nil || *r.Exact == failed || r.Started < loadStart || r.Ended > loadEnd || (failed && (r.Started != fault.Started || r.Ended != fault.Ended)) || r.Started < prior || r.Ended < r.Started {
			return errors.New("outage outcome binding/order differs")
		}
		if failed {
			if i == 0 || end != 0 || r.Started < w.PreKill {
				return errors.New("pre-Kill failed op, unbounded or second outage window")
			}
			if begin == 0 {
				begin = prior
			}
		} else if begin != 0 && end == 0 {
			end = r.Ended
		}
		prior = r.Ended
	}
	if begin != 0 && (end <= begin || end > w.PreKill+60) {
		return errors.New("outage outside pre-Kill 60s window")
	}
	if rows[0].Ended >= w.PreKill || rows[len(rows)-1].Started <= w.PreKill {
		return errors.New("load did not span restart")
	}
	return nil
}

func (x ReadRestart) targetRole() string {
	if x.Role == "" {
		return "volume"
	}
	return x.Role
}

func bindRestart(p Plan, d *inventory.Document, in tr.PackInputs, used map[string]bool) error {
	x := p.Restart
	role := x.targetRole()
	if role != "volume" && role != "filer" {
		return errors.New("unsupported restart role")
	}
	ep, ok := in.Restarts[x.Episode]
	if p.Profile != "m01-m02-v1" || d.Transport != "dc" || len(p.Workload) != 1 || p.Workload[0].Host != "m01" || !ok {
		return errors.New("restart needs exact local M01 DC profile/episode")
	}
	victim := false
	for _, s := range p.Deploy {
		victim = victim || s.Job == ep.Victim && s.Role == role && s.Host == "m02"
	}
	j := in.Jobs[ep.Replacement]
	want, e := ProfileArgs(d, "m02", "deploy", role, p.Smoke, in.Files["m02"])
	if !victim || used[ep.Replacement] || e != nil || !slices.Equal(j.Argv, append(want, "--replacement-"+role)) || j.Node != "m02" || j.Executable != in.Jobs[ep.Victim].Executable || j.SHA256 != in.Jobs[ep.Victim].SHA256 {
		return errors.New("replacement not exact private volume helper")
	}
	used[ep.Replacement] = true
	for _, v := range []struct{ label, stage, route string }{{x.DC, "load", "dc"}, {x.RecoverDC, "recovery", "dc"}, {x.RecoverHTTP, "recovery", "http"}} {
		if used[v.label] {
			return errors.New("reused read job")
		}
		used[v.label] = true
		if e = checkReadTask(in.Jobs[v.label], in, d, "m01", p.Smoke, v.stage, v.route); e != nil {
			return e
		}
	}
	return nil
}

func readTask(ctx context.Context, a *tr.ActionContext, in tr.PackInputs, label, stage, route string) (map[string]any, error) {
	var c SmokeConfig
	if err := json.Unmarshal([]byte(in.Jobs[label].Argv[3]), &c); err != nil {
		return nil, err
	}
	_, e, err := a.RunApprovedPackJob(ctx, label, false)
	if err != nil {
		return nil, err
	}
	return CheckReadTaskOutput(e, c, stage, route)
}

func (r *runtimePack) restart(ctx context.Context, a *tr.ActionContext, p Plan, d *inventory.Document, in tr.PackInputs, s *session) (map[string]string, error) {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	for _, host := range d.Observation.Hosts {
		if host.HostID == "m01" && (err != nil || strings.TrimSpace(string(boot)) != host.BootID) {
			return nil, errors.New("runner is not the admitted M01 clock")
		}
	}
	x := p.Restart
	for _, v := range []struct{ label, stage, route string }{{p.Workload[0].Job, "seed", "dc"}, {x.RecoverHTTP, "recovery", "http"}} {
		if _, err := readTask(ctx, a, in, v.label, v.stage, v.route); err != nil {
			return nil, err
		}
	}
	load, err := a.StartApprovedPackTask(ctx, x.DC)
	if err != nil {
		return nil, err
	}
	pause := time.After(30 * time.Second)
	if r.pause != nil {
		pause = r.pause()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-pause:
	}
	_, receipt, err := a.RestartApprovedPackGeneration(ctx, x.Episode, s.jobs[in.Restarts[x.Episode].Victim])
	if err != nil {
		return nil, err
	}
	readyMono := 0.
	for _, step := range p.Ready {
		_, e, err := a.RunApprovedPackJob(ctx, step.Job, false)
		if err != nil {
			return nil, err
		}
		raw, _, err := e.Output.Decode()
		if err != nil {
			return nil, err
		}
		var ready struct {
			Status     string
			ClientMono float64 `json:"client_mono"`
			Processes  map[string]struct {
				Identity tr.ProcessIdentity `json:"scope_identity"`
			}
		}
		if err = json.Unmarshal(raw, &ready); err != nil || ready.Status != "READY" {
			return nil, errors.New("replacement probe not READY")
		}
		if step.Host == "m01" {
			readyMono = ready.ClientMono
		}
		if step.Host == "m02" {
			role := x.targetRole()
			i := ready.Processes[role].Identity
			binary := "weed"
			if role == "volume" {
				binary = "weed-volume"
			}
			if i.PID != receipt.Replacement.PID || i.StartTime != receipt.Replacement.StartTime || i.BootID != receipt.Replacement.BootID || i.ExecutableSHA256 != in.Files["m02"][path.Join(p.Smoke.HelperDir, "../bin/"+binary)] {
				return nil, errors.New("replacement readiness identity differs")
			}
		}
	}
	if readyMono <= 0 {
		return nil, errors.New("client readiness observation missing")
	}

	// Load remains active until its fixed duration; no Wait watcher touches victim.
	e, err := a.WaitApprovedPackTask(ctx, load)
	if err != nil {
		return nil, err
	}

	var c SmokeConfig
	_ = json.Unmarshal([]byte(in.Jobs[x.DC].Argv[3]), &c)
	if _, err = CheckReadTaskOutput(e, c, "load", "dc", ReadWindow{receipt.Kill.PreKillMono}); err != nil {
		return nil, err
	}
	for _, v := range []struct{ label, route string }{{x.RecoverDC, "dc"}, {x.RecoverHTTP, "http"}} {
		if _, err = readTask(ctx, a, in, v.label, "recovery", v.route); err != nil {
			return nil, err
		}
	}
	return admittedResult("completed", "OK", "inspect DC window and exact DC/HTTP recovery receipts; HTTP outage DEFERRED", "seaweed_read_restart", d), nil
}
