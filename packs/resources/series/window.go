package series

import (
	"fmt"
	"math"
	"strings"
)

type row struct {
	t       float64
	p       Process
	bracket []float64
}
type domain struct {
	host, boot, namespace, clock string
	hz                           int64
}
type window struct {
	active, invalid bool
	warm, suffix    []row
}

func normalized(p Process) Identity {
	i := p.Identity
	i.Role = strings.Split(i.Role, "-restart-")[0]
	return i
}
func gone(p Process) bool {
	return p.Status == "GONE" || p.State == "Z" || p.State == "X" || p.State == "x"
}
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func number(p *float64) (float64, bool) {
	if p == nil || !finite(*p) || *p < 0 {
		return 0, false
	}
	return *p, true
}
func faultEnd(f Fault) float64 { return *f.Body + *f.Finished - *f.Started }
func inFault(t float64, fs []Fault) bool {
	for _, f := range fs {
		if *f.Body <= t && t <= faultEnd(f) {
			return true
		}
	}
	return false
}
func overlaps(a, b float64, fs []Fault) bool {
	for _, f := range fs {
		if a <= faultEnd(f) && b >= *f.Body {
			return true
		}
	}
	return false
}
func allFinal(f Final) []Process { return append(append([]Process{}, f.Processes...), f.Clients...) }
func dom(c *Capture) domain      { return domain{c.Host, c.Boot, c.Namespace, c.Domain, c.HZ} }
func capture(c *Capture) (float64, float64, bool) {
	if c == nil || c.Host == "" || c.Boot == "" || c.Namespace == "" || c.Domain != "CLOCK_BOOTTIME" || c.HZ <= 0 {
		return 0, 0, false
	}
	a, ok := number(c.Start)
	b, good := number(c.End)
	return a, b, ok && good && a <= b
}
func replacements(fs []Fault) (map[Identity]bool, map[Identity]bool) {
	ids, required := map[Identity]bool{}, map[Identity]bool{}
	add := func(role string, p Identity, req bool) {
		p.Role = role
		ids[p] = true
		if req {
			required[p] = true
		}
	}
	for _, f := range fs {
		if f.Kind == "client_kill" && f.Victim != nil && f.Replacement != nil {
			add(fmt.Sprint("client-", *f.Victim), *f.Replacement, false)
		}
		if f.Kind == "pair_restart" && f.Result != nil && f.Result.Pair != nil && f.Result.Pair.New != nil {
			add("volume", f.Result.Pair.New.Identity, false)
		}
		if (f.Kind == "vkill" || f.Kind == "overlap") && f.Result != nil && f.Result.New != nil {
			add("volume", f.Result.New.Identity, true)
		}
		for _, r := range f.Clients {
			add(r.Role, r.New, true)
		}
	}
	return ids, required
}
func replacementWindow(id Identity, rows []row, final Final, fs []Fault, replaced map[Identity]bool) window {
	if id.Role == "master" || id.Role == "filer" {
		return window{}
	}
	bad := window{active: true, invalid: true}
	for _, f := range fs {
		if f.Kind == "client_kill" && strings.HasPrefix(id.Role, "client-") && (f.Victim == nil || *f.Victim < 0 || id.Role == fmt.Sprint("client-", *f.Victim)) {
			if f.Victim == nil || *f.Victim < 0 || f.Replacement == nil || f.Replacement.PID <= 0 || f.Replacement.Start <= 0 {
				return bad
			}
		}
	}
	if !replaced[id] {
		return window{}
	}
	type observed struct {
		r      row
		lo, hi float64
	}
	var valid []observed
	var clock domain
	previous, previousBody := math.Inf(-1), math.Inf(-1)
	for _, r := range rows {
		a, b, ok := capture(r.p.Capture)
		if !ok || len(r.bracket) != 2 {
			return bad
		}
		ba, bb := r.bracket[0], r.bracket[1]
		if !finite(ba) || !finite(bb) || !(previousBody < ba && ba <= r.t && r.t <= bb) || a <= previous {
			return bad
		}
		if clock.hz != 0 && clock != dom(r.p.Capture) {
			return bad
		}
		clock = dom(r.p.Capture)
		previous = b
		previousBody = bb
		lo, hi := a-float64(id.Start+1)/float64(clock.hz), b-float64(id.Start)/float64(clock.hz)
		// Chronology is validated BEFORE startup/fault exclusion (the accepted R2 fix).
		if r.t >= 300 && !overlaps(ba, bb, fs) {
			valid = append(valid, observed{r, lo, hi})
		}
	}
	w := window{active: true}
	var post []observed
	for _, v := range valid {
		if 600 <= v.lo && v.lo <= v.hi && v.hi <= 690 && len(w.warm) < 3 {
			w.warm = append(w.warm, v.r)
		}
		if v.lo > 690 {
			post = append(post, v)
		}
	}
	if len(w.warm) != 3 || len(post) < 6 {
		return bad
	}
	if *post[len(post)-1].r.p.Capture.Start-*post[0].r.p.Capture.End < 300 {
		return bad
	}
	var terminal []Process
	for _, p := range allFinal(final) {
		if normalized(p) == id {
			terminal = append(terminal, p)
		}
	}
	if len(terminal) != 1 {
		return bad
	}
	p := terminal[0]
	if p.Status != "GONE" {
		a, b, ok := capture(p.Capture)
		if !ok || dom(p.Capture) != clock || !(previous < a && a <= b) {
			return bad
		}
	}
	for _, v := range valid {
		if *v.r.p.Capture.Start >= *w.warm[2].p.Capture.Start {
			w.suffix = append(w.suffix, v.r)
		}
	}
	return w
}
