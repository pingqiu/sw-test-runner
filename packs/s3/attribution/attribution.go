// Package attribution evaluates the frozen S3 amendment-A1 error rule without I/O.
package attribution

import (
	"math"
	"reflect"
)

type Mark struct {
	From  *float64 `json:"from"`
	To    *float64 `json:"to"`
	Fault *int     `json:"fault"`
}
type Witness struct {
	PID int64 `json:"pid"`
}
type Operation struct {
	ID         int64    `json:"op"`
	Kind       string   `json:"kind"`
	Key        *string  `json:"key"`
	Start      *float64 `json:"epoch0"`
	End        *float64 `json:"epoch1"`
	OK         bool     `json:"ok"`
	Status     *int     `json:"status"`
	Class      string   `json:"error_class"`
	Designated bool     `json:"designated"`
	Refusal    bool     `json:"refusal"`
	Mark       *Mark    `json:"excused"`
	Late       bool     `json:"late_classification"`
	Witness    *Witness `json:"witness"`
}
type Victim struct {
	PID int64  `json:"pid"`
	Key string `json:"key"`
}
type Fault struct {
	Role   string    `json:"role"`
	Window []float64 `json:"window"`
	Victim *Victim   `json:"victim"`
}
type Failure struct {
	ID    int64   `json:"op"`
	Kind  string  `json:"kind"`
	Key   *string `json:"key"`
	Class string  `json:"error_class"`
}
type Report struct {
	Status    string    `json:"status"`
	Reason    string    `json:"reason,omitempty"`
	Unexcused []Failure `json:"unexcused"`
	Failed    int       `json:"failed"`
	Refusals  int       `json:"refusals"`
}

func finite(v float64) bool  { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }
func mutation(k string) bool { return k == "put" || k == "complete" || k == "delete" }
func read(k string) bool {
	switch k {
	case "http_get", "range_get", "dc_get", "range_dc", "head", "list":
		return true
	}
	return false
}
func role(role string) bool {
	switch role {
	case "master", "pair", "freeze", "crash", "s3", "objsvc", "client":
		return true
	}
	return false
}
func validFault(f Fault) bool {
	return role(f.Role) && len(f.Window) == 2 && finite(f.Window[0]) && finite(f.Window[1]) && f.Window[0] <= f.Window[1]
}

// Eligible is A1's independent timing/role/class rule. A runtime mark is not
// considered here. Being eligible alone never excuses an error in Assess.
func Eligible(r Operation, f Fault) bool {
	if !validFault(f) || r.Start == nil || r.End == nil || !finite(*r.Start) || !finite(*r.End) || *r.End < *r.Start {
		return false
	}
	kinds, classes := false, false
	switch f.Role {
	case "master":
		kinds = read(r.Kind) || mutation(r.Kind)
		classes = r.Class == "conn" || r.Class == "timeout" || r.Class == "http500" || r.Class == "http503"
	case "pair", "freeze", "crash":
		kinds = read(r.Kind) || mutation(r.Kind)
		classes = r.Class == "timeout" || r.Class == "http500" || r.Class == "http503"
	case "s3":
		kinds = read(r.Kind) || mutation(r.Kind)
		classes = r.Class == "conn" || r.Class == "timeout"
	case "objsvc":
		kinds = r.Kind == "dc_get" || r.Kind == "range_dc"
		classes = r.Class == "http500"
	case "client":
		kinds = r.Kind == "dc_get"
		classes = r.Class == "client_kill"
		if f.Victim == nil || r.Key == nil || f.Victim.Key != *r.Key || f.Victim.PID <= 0 || r.Witness == nil || r.Witness.PID != f.Victim.PID {
			return false
		}
	}
	limit := 35.
	if r.Designated {
		limit = 185
	}
	return kinds && classes && f.Window[0] <= *r.End && *r.End <= *r.Start+limit && *r.Start <= f.Window[1] && (!mutation(r.Kind) || r.Designated)
}
func refusal(r Operation) bool {
	if !r.OK && r.Status != nil && *r.Status == 416 && r.Kind == "http_416" {
		return true
	}
	return !r.OK && r.Status != nil && *r.Status == 404 && (r.Kind == "http_get" || r.Kind == "range_get" || r.Kind == "head")
}
func sameAttempt(a, b Operation) bool {
	a.Mark = nil
	b.Mark = nil
	a.Late = false
	b.Late = false
	return reflect.DeepEqual(a, b)
}

// Assess preserves ordered last-classification handling, but refuses a rewritten
// operation and binds the runtime mark to the same independently eligible fault.
// Missing input is NOT_PROVEN; successful attribution says nothing about bytes.
func Assess(ops []Operation, faults []Fault) Report {
	out := Report{Status: "PASS", Unexcused: []Failure{}}
	fail := func(reason string) Report { out.Status = "FAIL"; out.Reason = reason; return out }
	if ops == nil || faults == nil {
		out.Status = "NOT_PROVEN"
		out.Reason = "receipt missing"
		return out
	}
	for _, f := range faults {
		if !validFault(f) {
			return fail("unknown role or malformed fault window")
		}
	}
	var order []int64
	by := map[int64]Operation{}
	for _, r := range ops {
		if r.ID <= 0 || r.Kind == "" || r.Start == nil || r.End == nil || !finite(*r.Start) || !finite(*r.End) || *r.End < *r.Start {
			return fail("malformed attempt")
		}
		if old, ok := by[r.ID]; ok {
			if !r.Late || !sameAttempt(old, r) {
				return fail("rewritten or duplicate attempt")
			}
		} else {
			order = append(order, r.ID)
		}
		by[r.ID] = r
	}
	for _, id := range order {
		r := by[id]
		if !r.OK {
			out.Failed++
		}
		if r.Refusal {
			out.Refusals++
			if !refusal(r) {
				return fail("false refusal classification")
			}
		}
		if r.OK || r.Refusal {
			continue
		}
		credited := false
		if m := r.Mark; m != nil && m.Fault != nil && *m.Fault >= 0 && *m.Fault < len(faults) && m.From != nil && m.To != nil {
			f := faults[*m.Fault]
			credited = *m.From == f.Window[0] && *m.To == f.Window[1] && Eligible(r, f)
		}
		if !credited && len(out.Unexcused) < 50 {
			out.Unexcused = append(out.Unexcused, Failure{ID: r.ID, Kind: r.Kind, Key: r.Key, Class: r.Class})
		}
		if !credited {
			out.Status = "FAIL"
		}
	}
	return out
}
