// Package gomemory evaluates frozen r5 master / r6r3 filer evidence without I/O.
package gomemory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
)

type Identity struct {
	Role  string `json:"role"`
	PID   int64  `json:"pid"`
	Start int64  `json:"starttime"`
}
type Capture struct {
	Identity
	Kind          string   `json:"kind"`
	Age           *float64 `json:"age"`
	Anchor        *float64 `json:"anchor_monotonic"`
	RequestStart  *float64 `json:"request_start_monotonic"`
	ProfileEnd    *float64 `json:"profile_end_monotonic"`
	RequestEpoch  *float64 `json:"request_start_epoch"`
	ProfileEpoch  *float64 `json:"profile_end_epoch"`
	Quiesce       *float64 `json:"quiesce_seconds"`
	GCRequests    *int     `json:"gc_requests"`
	ListenerOwned bool     `json:"listener_owned"`
	RSS           *float64 `json:"rss_bytes"`
	RSSStart      *float64 `json:"rss_observation_start_monotonic"`
	RSSEnd        *float64 `json:"rss_observation_end_monotonic"`
	RSSEpochStart *float64 `json:"rss_observation_start_epoch"`
	RSSEpochEnd   *float64 `json:"rss_observation_end_epoch"`
	SHA256        string   `json:"sha256"`
	File          string   `json:"file"`
	Raw           []byte   `json:"raw"`
}
type Closure struct {
	PID    int64    `json:"pid"`
	Start  int64    `json:"starttime"`
	Absent bool     `json:"absent_or_replaced_or_zombie"`
	Epoch  *float64 `json:"epoch"`
}
type Evidence struct {
	Warm   *Capture `json:"warm"`
	End    *Capture `json:"end"`
	Closed *Closure `json:"closed"`
}
type Result struct {
	Status      string  `json:"status"`
	Reason      string  `json:"reason,omitempty"`
	RSSOK       bool    `json:"rss_ok"`
	HeapOK      bool    `json:"heap_ok"`
	FDsOK       bool    `json:"fds_ok"`
	RSSBaseline float64 `json:"rss_baseline"`
	RSSFinal    float64 `json:"rss_final"`
	InuseStart  int64   `json:"inuse_start"`
	InuseEnd    int64   `json:"inuse_end"`
	InuseGrowth int64   `json:"inuse_growth"`
}

const MiB = 1048576

func number(v *float64) (float64, error) {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0 {
		return 0, fmt.Errorf("missing/nonfinite/negative measurement")
	}
	return *v, nil
}
func identity(i Identity) bool {
	return (i.Role == "master" || i.Role == "filer") && i.PID > 0 && i.Start > 0
}
func timing(c Capture) (float64, float64, error) {
	vals := []*float64{c.Anchor, c.RequestStart, c.ProfileEnd, c.Age, c.RequestEpoch, c.ProfileEpoch}
	v := make([]float64, len(vals))
	for i, p := range vals {
		n, e := number(p)
		if e != nil {
			return 0, 0, e
		}
		v[i] = n
	}
	if v[2] < v[1] || v[1] < v[0] || v[3] != v[1]-v[0] || v[5] < v[4] {
		return 0, 0, fmt.Errorf("profile actual timing mismatch")
	}
	if c.Kind == "warm" && !(600 <= v[1]-v[0] && v[2]-v[0] <= 635) {
		return 0, 0, fmt.Errorf("ten-minute capture missed")
	}
	return v[1], v[2], nil
}

// Evaluate preserves the r5 gates. For filer, r6r3 selects the RSS observed
// inside the same generation's warm capture; caller's initial RSS is diagnostic.
func Evaluate(baseline, fd0, fdmax *float64, warm, end Capture) Result {
	fail := func(e any) Result { return Result{Status: "FAIL", Reason: fmt.Sprint(e)} }
	if warm.Identity != end.Identity || !identity(warm.Identity) {
		return fail("Go generation mismatch")
	}
	if warm.Kind != "warm" || end.Kind != "quiescent_gc" {
		return fail("capture kind")
	}
	if warm.Role == "filer" {
		vals := []*float64{warm.RSS, warm.Anchor, warm.RSSStart, warm.RSSEnd, warm.ProfileEnd, warm.RSSEpochStart, warm.RSSEpochEnd}
		v := make([]float64, len(vals))
		for i, p := range vals {
			n, e := number(p)
			if e != nil {
				return fail(e)
			}
			v[i] = n
		}
		if !(600 <= v[2]-v[1] && v[2] <= v[3] && v[3]-v[1] <= 635 && v[2] >= v[4] && v[6] >= v[5]) {
			return fail("warm RSS observation missed/reversed window")
		}
		baseline = warm.RSS
	}
	b, e := number(baseline)
	if e != nil {
		return fail(e)
	}
	f, e := number(fd0)
	if e != nil {
		return fail(e)
	}
	m, e := number(fdmax)
	if e != nil {
		return fail(e)
	}
	_, we, e := timing(warm)
	if e != nil {
		return fail(e)
	}
	es, _, e := timing(end)
	if e != nil {
		return fail(e)
	}
	if *warm.Anchor != *end.Anchor || es <= we || *warm.Age < 600 || *warm.Age > 635 || *end.Age <= *warm.Age {
		return fail("generation anchor/profile order")
	}
	q, e := number(end.Quiesce)
	if e != nil || q < 60 || end.GCRequests == nil || *end.GCRequests != 1 || warm.GCRequests == nil || *warm.GCRequests != 0 {
		return fail("quiesce/GC evidence")
	}
	if !warm.ListenerOwned || !end.ListenerOwned {
		return fail("unowned profiler")
	}
	for _, c := range []Capture{warm, end} {
		h := sha256.Sum256(c.Raw)
		if hex.EncodeToString(h[:]) != c.SHA256 {
			return fail("profile hash mismatch")
		}
	}
	first, e := InuseSpace(warm.Raw)
	if e != nil {
		return fail(e)
	}
	last, e := InuseSpace(end.Raw)
	if e != nil {
		return fail(e)
	}
	rss, e := number(end.RSS)
	if e != nil {
		return fail(e)
	}
	r := Result{Status: "PASS", RSSOK: rss <= b+math.Max(64*MiB, .1*b), HeapOK: last-first < 32*MiB, FDsOK: m <= f+16, RSSBaseline: b, RSSFinal: rss, InuseStart: first, InuseEnd: last, InuseGrowth: last - first}
	if !r.RSSOK || !r.HeapOK || !r.FDsOK {
		r.Status = "FAIL"
	}
	return r
}

// Group joins captures to an enrolled generation. CLOSED is never live-memory PASS.
func Group(id Identity, baseline, fd0, fdmax *float64, e Evidence, live bool) Result {
	fail := func(s string) Result { return Result{Status: "FAIL", Reason: s} }
	if !identity(id) {
		return fail("invalid process identity")
	}
	// Mirrors the frozen archive_evidence join, including profiles attached to
	// generations that were later closed. The caller supplies archived bytes.
	for kind, c := range map[string]*Capture{"warm": e.Warm, "quiescent_gc": e.End} {
		if c == nil {
			continue
		}
		want := fmt.Sprintf("%s-%d-%d-%s.heap", id.Role, id.PID, id.Start, kind)
		h := sha256.Sum256(c.Raw)
		if c.Identity != id || c.Kind != kind || c.File != want || c.SHA256 != hex.EncodeToString(h[:]) {
			return fail("archived profile identity/phase/hash mismatch")
		}
	}
	if e.Closed != nil {
		c := e.Closed
		if live || c.PID != id.PID || c.Start != id.Start || !c.Absent {
			return fail("closed/live identity contradiction")
		}
		f, err := number(fd0)
		if err != nil {
			return fail(err.Error())
		}
		m, err := number(fdmax)
		if err != nil {
			return fail(err.Error())
		}
		t, err := number(c.Epoch)
		if err != nil || t <= 0 || m > f+16 {
			return fail("closure FD/epoch evidence")
		}
		return Result{Status: "CLOSED"}
	}
	if !live {
		return fail("prior Go generation closure unproven")
	}
	if e.Warm == nil || e.End == nil || e.Warm.Identity != id || e.End.Identity != id {
		return fail("group/profile identity mismatch")
	}
	return Evaluate(baseline, fd0, fdmax, *e.Warm, *e.End)
}
