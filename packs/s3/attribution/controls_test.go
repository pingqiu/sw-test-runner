package attribution

import (
	"encoding/json"
	"testing"
)

func f(v float64) *float64 { return &v }
func i(v int) *int         { return &v }
func s(v string) *string   { return &v }
func basic() (Operation, []Fault) {
	return Operation{ID: 1, Kind: "put", Key: s("fresh"), Start: f(100), End: f(140), Class: "http500", Status: i(500), Designated: true, Mark: &Mark{From: f(101), To: f(120), Fault: i(0)}}, []Fault{{Role: "master", Window: []float64{101, 120}}}
}
func TestA1CanExcuseCompletionPastReadyWindow(t *testing.T) {
	r, fs := basic()
	if !Eligible(r, fs[0]) {
		t.Fatal("A1 internal retry tail rejected")
	}
	got := Assess([]Operation{r}, fs)
	if got.Status != "PASS" || got.Failed != 1 || got.Refusals != 0 {
		t.Fatal(got)
	}
}
func TestEachA1RequirementRefusesIndependently(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Operation, []Fault)
	}{
		{"sent after close", func(r *Operation, fs []Fault) { r.Start = f(120.001) }},
		{"failed before fault", func(r *Operation, fs []Fault) { r.Start = f(90); r.End = f(100.999) }},
		{"past designated timeout", func(r *Operation, fs []Fault) { r.End = f(285.001) }},
		{"ordinary mutation", func(r *Operation, fs []Fault) { r.Designated = false; r.End = f(130) }},
		{"semantic never excused", func(r *Operation, fs []Fault) { r.Class = "semantic" }},
		{"wrong role table", func(r *Operation, fs []Fault) { fs[0].Role = "objsvc" }},
		{"wrong error class", func(r *Operation, fs []Fault) { r.Class = "http503"; fs[0].Role = "s3" }},
		{"runtime mark missing", func(r *Operation, fs []Fault) { r.Mark = nil }},
		{"wrong mark bound", func(r *Operation, fs []Fault) { r.Mark.To = f(120.001) }},
		{"mark index missing", func(r *Operation, fs []Fault) { r.Mark.Fault = nil }},
		{"reversed attempt", func(r *Operation, fs []Fault) { r.Start = f(141) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, fs := basic()
			tc.mutate(&r, fs)
			if got := Assess([]Operation{r}, fs); got.Status != "FAIL" {
				t.Fatal(got)
			}
		})
	}
}
func TestExactOwnTimeouts(t *testing.T) {
	for _, designated := range []bool{false, true} {
		r, fs := basic()
		r.Designated = designated
		if !designated {
			r.Kind = "http_get"
		}
		limit := 35.
		if designated {
			limit = 185
		}
		r.End = f(*r.Start + limit)
		if got := Assess([]Operation{r}, fs); got.Status != "PASS" {
			t.Fatal(got)
		}
		r.End = f(*r.End + .001)
		if got := Assess([]Operation{r}, fs); got.Status != "FAIL" {
			t.Fatal(got)
		}
	}
}
func TestRuntimeMarkMustJoinTheEligibleFault(t *testing.T) {
	r, fs := basic()
	fs[0].Role = "objsvc"
	fs = append(fs, Fault{Role: "master", Window: []float64{101, 120}})
	if got := Assess([]Operation{r}, fs); got.Status != "FAIL" {
		t.Fatal(got)
	}
	r.Mark.Fault = i(1)
	if got := Assess([]Operation{r}, fs); got.Status != "PASS" {
		t.Fatal(got)
	}
}
func TestClientKillCreditIsVictimOnly(t *testing.T) {
	r := Operation{ID: 1, Kind: "dc_get", Key: s("victim"), Start: f(100), End: f(110), Class: "client_kill", Mark: &Mark{From: f(110), To: f(110), Fault: i(0)}, Witness: &Witness{PID: 123}}
	fs := []Fault{{Role: "client", Window: []float64{110, 110}, Victim: &Victim{PID: 123, Key: "victim"}}}
	if got := Assess([]Operation{r}, fs); got.Status != "PASS" {
		t.Fatal(got)
	}
	r.Key = s("other")
	if got := Assess([]Operation{r}, fs); got.Status != "FAIL" {
		t.Fatal(got)
	}
	r.Key = s("victim")
	r.Witness.PID++
	if got := Assess([]Operation{r}, fs); got.Status != "FAIL" {
		t.Fatal(got)
	}
}
func TestOnlyExplicitReadRefusalsAreSkipped(t *testing.T) {
	for _, k := range []string{"http_get", "range_get", "head", "http_416"} {
		r, _ := basic()
		r.Kind = k
		r.Designated = false
		r.Mark = nil
		r.Refusal = true
		r.Status = i(404)
		if k == "http_416" {
			r.Status = i(416)
		}
		if got := Assess([]Operation{r}, []Fault{}); got.Status != "PASS" || got.Refusals != 1 {
			t.Fatal(got)
		}
	}
	for _, k := range []string{"put", "complete", "delete", "dc_get", "range_dc"} {
		r, _ := basic()
		r.Kind = k
		r.Refusal = true
		r.Status = i(404)
		if got := Assess([]Operation{r}, []Fault{}); got.Status != "FAIL" {
			t.Fatal(got)
		}
	}
}
func TestLateClassificationCannotRewriteAttempt(t *testing.T) {
	r, fs := basic()
	initial := r
	initial.Mark = nil
	late := r
	late.Late = true
	if got := Assess([]Operation{initial, late}, fs); got.Status != "PASS" || got.Failed != 1 {
		t.Fatal(got)
	}
	late.Key = s("foreign")
	if got := Assess([]Operation{initial, late}, fs); got.Status != "FAIL" {
		t.Fatal(got)
	}
	late = r
	if got := Assess([]Operation{initial, late}, fs); got.Status != "FAIL" {
		t.Fatal(got)
	}
}
func TestMissingOrUnknownEvidenceRefuses(t *testing.T) {
	r, fs := basic()
	if got := Assess(nil, fs); got.Status != "NOT_PROVEN" {
		t.Fatal(got)
	}
	if got := Assess([]Operation{r}, nil); got.Status != "NOT_PROVEN" {
		t.Fatal(got)
	}
	fs[0].Role = "gateway"
	if got := Assess([]Operation{r}, fs); got.Status != "FAIL" {
		t.Fatal("legacy role silently renamed", got)
	}
}
func TestNoMutationOfInput(t *testing.T) {
	r, fs := basic()
	before, _ := json.Marshal([]any{r, fs})
	_ = Assess([]Operation{r}, fs)
	after, _ := json.Marshal([]any{r, fs})
	if string(before) != string(after) {
		t.Fatal("input mutated")
	}
}
