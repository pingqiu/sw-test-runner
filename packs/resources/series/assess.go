package series

import (
	"fmt"
	"math"
	"sort"

	"github.com/pingqiu/sw-test-runner/packs/resources/gomemory"
)

func median(v []float64) float64 {
	v = append([]float64{}, v...)
	sort.Float64s(v)
	if len(v)%2 == 1 {
		return v[len(v)/2]
	}
	return (v[len(v)/2-1] + v[len(v)/2]) / 2
}
func selectMetric(rows []row, metric func(Process) *float64) ([]float64, bool) {
	v := make([]float64, len(rows))
	for i, r := range rows {
		n, ok := number(metric(r.p))
		if !ok {
			return nil, false
		}
		v[i] = n
	}
	return v, true
}
func times(rows []row) []float64 {
	v := make([]float64, len(rows))
	for i, r := range rows {
		v[i] = r.t
	}
	return v
}
func sortedIDs(m map[Identity][]row) []Identity {
	var ids []Identity
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := ids[i], ids[j]
		if a.Role != b.Role {
			return a.Role < b.Role
		}
		if a.PID != b.PID {
			return a.PID < b.PID
		}
		return a.Start < b.Start
	})
	return ids
}
func aggregate(states []string) string {
	status := "PASS"
	if len(states) == 0 {
		return "NOT_OBSERVED"
	}
	for _, s := range states {
		if s == "FAIL" {
			return "FAIL"
		}
		if s != "PASS" {
			status = "NOT_OBSERVED"
		}
	}
	return status
}
func slope(rows []row, rss []float64) float64 {
	var mt, mr float64
	for i, r := range rows {
		mt += r.t / 60
		mr += rss[i] / gomemory.MiB
	}
	mt /= float64(len(rows))
	mr /= float64(len(rows))
	var num, den float64
	for i, r := range rows {
		dt := r.t/60 - mt
		num += dt * (rss[i]/gomemory.MiB - mr)
		den += dt * dt
	}
	if den == 0 {
		return math.Inf(1)
	}
	return num / den
}

// Assess requires an explicit recorded policy. Policy is a trusted source-pin
// choice, never a request flag or a fallback selected after seeing the verdict.
func Assess(in Input) Result {
	checks := map[string]Check{}
	fail := func(reason string) Result {
		return Result{Status: "FAIL", Checks: map[string]Check{"input": {Status: "FAIL", Reason: reason}}}
	}
	if in.Policy != Startup54d && in.Policy != Replacement9b5 {
		return fail("unrecognized recorded assessor policy")
	}
	for _, f := range in.Faults {
		a, ok := number(f.Body)
		s, ok2 := number(f.Started)
		e, ok3 := number(f.Finished)
		if !ok || !ok2 || !ok3 || e < s || f.Kind == "" || !finite(a+e-s) {
			return fail("malformed fault interval")
		}
	}
	groups, raw := map[Identity][]row{}, map[Identity][]row{}
	for _, sample := range in.Samples {
		t, ok := number(sample.Body)
		if !ok {
			return fail("missing sample body time")
		}
		ps := append(append([]Process{}, sample.Server.Processes...), sample.Clients...)
		for _, p := range ps {
			if gone(p) {
				continue
			}
			id := normalized(p)
			if id.Role == "" || id.PID <= 0 || id.Start <= 0 || id.Start == math.MaxInt64 {
				return fail("invalid process identity")
			}
			r := row{t, p, sample.BodyBracket}
			raw[id] = append(raw[id], r)
			if t >= 300 && !inFault(t, in.Faults) {
				groups[id] = append(groups[id], r)
			}
		}
	}
	replaced, required := replacements(in.Faults)
	windows := map[Identity]window{}
	observed := true
	if in.Policy == Replacement9b5 {
		for id, rows := range raw {
			w := replacementWindow(id, rows, in.Final, in.Faults, replaced)
			windows[id] = w
			if w.invalid {
				observed = false
			}
		}
		for id := range required {
			if _, ok := raw[id]; !ok {
				observed = false
			}
		}
	}
	growth := Check{}
	okay := true
	for _, id := range sortedIDs(groups) {
		rows := groups[id]
		d := Detail{Identity: id}
		if len(rows) < 6 {
			d.Status = "NOT_OBSERVED"
			d.Samples = len(rows)
			growth.Details = append(growth.Details, d)
			observed = false
			continue
		}
		rss, ok := selectMetric(rows, func(p Process) *float64 { return p.RSS })
		_, good := selectMetric(rows, func(p Process) *float64 { return p.FDs })
		if !ok || !good {
			d.Status = "NOT_OBSERVED"
			growth.Details = append(growth.Details, d)
			observed = false
			continue
		}
		baseline, tail := median(rss[:3]), median(rss[len(rss)-3:])
		sl := slope(rows, rss)
		rssOK := tail-baseline <= math.Max(64*gomemory.MiB, .1*baseline) && sl <= 2
		w := windows[id]
		warm, fdrows := rows[:3], rows
		if w.active {
			warm, fdrows = w.warm, w.suffix
		}
		if len(warm) == 0 {
			d.Status = "NOT_OBSERVED"
			if !rssOK {
				d.Status = "FAIL"
				okay = false
			}
			d.Reason = "replacement warm witness"
			growth.Details = append(growth.Details, d)
			observed = false
			continue
		}
		fw, ok := selectMetric(warm, func(p Process) *float64 { return p.FDs })
		fm, good := selectMetric(fdrows, func(p Process) *float64 { return p.FDs })
		if !ok || !good || len(fm) == 0 {
			d.Status = "NOT_OBSERVED"
			growth.Details = append(growth.Details, d)
			observed = false
			continue
		}
		fd0 := median(fw)
		fdmax := fm[0]
		for _, v := range fm {
			fdmax = math.Max(fdmax, v)
		}
		passed := rssOK && fdmax <= fd0+16
		if id.Role == "master" || id.Role == "filer" {
			live := false
			for _, p := range in.Final.Processes {
				if normalized(p) == id && !gone(p) {
					live = true
				}
			}
			key := fmt.Sprintf("%s-%d-%d", id.Role, id.PID, id.Start)
			g := gomemory.Group(id, &baseline, &fd0, &fdmax, in.GoEvidence[key], live)
			d.GoMemory = &g
			passed = g.Status == "PASS" || g.Status == "CLOSED"
		}
		d.Status = "PASS"
		if !passed {
			d.Status = "FAIL"
			okay = false
		}
		d.Samples = len(rows)
		d.RSSBaseline = baseline
		d.RSSTail = tail
		d.RSSGrowth = tail - baseline
		d.Slope = sl
		d.FDBaseline = fd0
		d.FDMax = fdmax
		if in.Policy == Replacement9b5 {
			d.FDWindow = times(warm)
			d.FDFrom = fdrows[0].t
		}
		growth.Details = append(growth.Details, d)
	}
	growth.Status = "NOT_OBSERVED"
	if !okay {
		growth.Status = "FAIL"
	} else if observed && len(groups) > 0 {
		growth.Status = "PASS"
	}
	checks["rss_fd_growth"] = growth
	pinning := Check{}
	final := allFinal(in.Final)
	if in.Policy == Replacement9b5 {
		missing := map[Identity][]row{}
		for id := range required {
			if _, ok := raw[id]; !ok {
				missing[id] = nil
			}
		}
		for id, w := range windows {
			if !w.invalid {
				continue
			}
			found := false
			for _, p := range final {
				if normalized(p) == id {
					found = true
				}
			}
			if !found {
				missing[id] = nil
			}
		}
		for _, id := range sortedIDs(missing) {
			pinning.Details = append(pinning.Details, Detail{Identity: id, Status: "NOT_OBSERVED"})
		}
	}
	for _, p := range final {
		if p.Status == "GONE" {
			continue
		}
		id := normalized(p)
		rows := groups[id]
		if w := windows[id]; w.active {
			rows = w.warm
		}
		d := Detail{Identity: id, Status: "NOT_OBSERVED"}
		if len(rows) < 3 {
			pinning.Details = append(pinning.Details, d)
			continue
		}
		pins, ok := selectMetric(rows[:3], func(p Process) *float64 { return p.Pin })
		locks, good := selectMetric(rows[:3], func(p Process) *float64 { return p.Lock })
		lastPin, ok2 := number(p.Pin)
		lastLock, ok3 := number(p.Lock)
		if !ok || !good || !ok2 || !ok3 {
			pinning.Details = append(pinning.Details, d)
			continue
		}
		d.Baseline = map[string]float64{"VmPin_bytes": median(pins), "VmLck_bytes": median(locks)}
		d.Final = map[string]float64{"VmPin_bytes": lastPin, "VmLck_bytes": lastLock}
		if in.Policy == Replacement9b5 {
			d.PinWindow = times(rows[:3])
		}
		d.Status = "PASS"
		if lastPin > d.Baseline["VmPin_bytes"] || lastLock > d.Baseline["VmLck_bytes"] {
			d.Status = "FAIL"
		}
		pinning.Details = append(pinning.Details, d)
	}
	var states []string
	for _, d := range pinning.Details {
		states = append(states, d.Status)
	}
	pinning.Status = aggregate(states)
	checks["fixed_pinning_returns_to_baseline"] = pinning
	for k, c := range bounds(in) {
		checks[k] = c
	}
	states = nil
	for _, c := range checks {
		states = append(states, c.Status)
	}
	return Result{Status: aggregate(states), Checks: checks}
}
