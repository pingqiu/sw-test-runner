package seaweed

import (
	"errors"
	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
	"math"
	"slices"
	"strconv"
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

func (x ReadRestart) loadStage() string {
	if x.Writes {
		return "write-load"
	}
	return "load"
}

func bindRestart(p Plan, d *inventory.Document, in tr.PackInputs, used map[string]bool) error {
	x := p.Restart
	role := x.targetRole()
	if (role != "volume" && role != "filer" && role != "master") || (role == "master") != x.Writes {
		return errors.New("unsupported restart role")
	}
	ep, ok := in.Restarts[x.Episode]
	if x.Freeze {
		ok = in.Pause != nil && in.Pause.Name == x.Episode && in.Pause.AllowWorkResume && role == "volume" && len(in.Restarts) == 0 && x.RecoverHTTP == ""
		if ok {
			ep.Victim = in.Pause.Victim
		}
	}
	if p.Profile != "m01-m02-v1" || d.Transport != "dc" || len(p.Workload) != 1 || p.Workload[0].Host != "m01" || !ok {
		return errors.New("restart needs exact local M01 DC profile/episode")
	}
	victim := false
	for _, s := range p.Deploy {
		victim = victim || s.Job == ep.Victim && s.Role == role && s.Host == "m02"
	}
	j := in.Jobs[ep.Replacement]
	want, e := ProfileArgs(d, "m02", "deploy", role, p.profileSettings(false), in.Files["m02"])
	if !victim || (!x.Freeze && (used[ep.Replacement] || e != nil || !slices.Equal(j.Argv, append(want, "--replacement-"+role)) || j.Node != "m02" || j.Executable != in.Jobs[ep.Victim].Executable || j.SHA256 != in.Jobs[ep.Victim].SHA256)) {
		return errors.New("replacement not exact private volume helper")
	}
	used[ep.Replacement] = true
	for _, v := range []struct{ label, stage, route string }{{x.DC, x.loadStage(), "dc"}, {x.RecoverDC, "recovery", "dc"}, {x.RecoverHTTP, "recovery", "http"}} {
		if x.Freeze && v.route == "http" {
			continue
		}
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
