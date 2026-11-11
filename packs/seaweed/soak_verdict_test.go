package seaweed

import (
	"encoding/json"
	"strings"
	"testing"
)

var testBinding = SoakBinding{Schema: SoakVerdictSchema, RunID: "run-1", BootID: "boot-1", Root: "/run/run-1"}

// passRow is a complete PASS: every mandatory criterion, bound to testBinding.
func passRow(t *testing.T, edit func(map[string]any)) string {
	t.Helper()
	crit := map[string]any{}
	for _, c := range soakCriteria {
		crit[c] = map[string]any{"outcome": "PASS", "reason": "ok"}
	}
	row := map[string]any{"status": "PASS", "criteria": crit, "k7": []any{}, "resources": "NOT_OBSERVED", "binding": testBinding,
		"misses_qualification": map[string]any{"method": "qualified observed ordering under the stated limit; no parity claim", "clock": "conditional", "limit": "latent loss"}}
	if edit != nil {
		edit(row)
	}
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The verdict row is decoded strictly: one JSON value, no duplicate keys,
// known fields only, this run's binding, resources NOT_OBSERVED, and a PASS
// that carries exactly the mandatory criteria, all PASS.
func TestDecodeVerdictFailsClosed(t *testing.T) {
	pass := passRow(t, nil)
	if v, err := decodeVerdict(captured([]byte(pass+"\n")), testBinding, soakCriteria); err != nil || v.Status != "PASS" {
		t.Fatalf("valid PASS refused: %v", err)
	}
	fail := passRow(t, func(r map[string]any) {
		r["status"] = "FAIL"
		r["criteria"].(map[string]any)["routes"] = map[string]any{"outcome": "FAIL", "reason": "bad"}
	})
	if v, err := decodeVerdict(captured([]byte(fail)), testBinding, soakCriteria); err != nil || v.Status != "FAIL" {
		t.Fatalf("valid FAIL not decoded: %v", err)
	}
	// Rule A: the assessor reports undecidable misses alongside a PASS row; the strict decoder accepts the field.
	undecidable := passRow(t, func(r map[string]any) {
		r["misses_undecidable"] = []any{undecidableRow(nil)}
	})
	if v, err := decodeVerdict(captured([]byte(undecidable)), testBinding, soakCriteria); err != nil || v.Status != "PASS" || len(v.MissesUndecidable) != 1 {
		t.Fatalf("PASS with an undecidable miss: %+v %v", v.MissesUndecidable, err)
	}
	// codex01 on 66da7699: the offsets are rounded diagnostics, not a second classifier. A submit 0.1 us after
	// early(B) rounds to an early offset of 0.0, and a multi-VID op reports a VID already after B with a positive
	// late offset; both are valid producer rows and must decode.
	for name, edit := range map[string]func(map[string]any){
		"early offset rounded to 0": func(u map[string]any) { u["submit_minus_early_b_s"] = 0.0 },
		"multi-VID, one VID after B": func(u map[string]any) {
			u["vid"], u["submit_minus_early_b_s"], u["submit_minus_late_b_s"] = 27, 9.93, 9.88
		},
	} {
		row := passRow(t, func(r map[string]any) { r["misses_undecidable"] = []any{undecidableRow(nil), undecidableRow(edit)} })
		if v, err := decodeVerdict(captured([]byte(row)), testBinding, soakCriteria); err != nil || len(v.MissesUndecidable) != 2 {
			t.Fatalf("%s refused: %v", name, err)
		}
	}
	errRow := passRow(t, func(r map[string]any) {
		r["status"], r["criteria"], r["error"] = "FAIL", map[string]any{}, "ValueError('x')"
	})
	if !strings.Contains(passRow(t, nil), "misses_qualification") {
		t.Fatal("fixture lost the qualification")
	}
	if v, err := decodeVerdict(captured([]byte(errRow)), testBinding, soakCriteria); err != nil || v.Status != "FAIL" {
		t.Fatalf("assessor error row not decoded as FAIL: %v", err)
	}
	for name, body := range map[string]string{
		// codex01's controls: an incomplete PASS, two values on one line, a duplicate status.
		"PASS with only wrong_reads":  `{"status":"PASS","criteria":{"wrong_reads":{"outcome":"PASS","reason":"0 wrong"}},"k7":[],"resources":"NOT_OBSERVED","binding":` + mustJSON(testBinding) + `}`,
		"PASS then FAIL on one line":  pass + " " + fail,
		"duplicate status, last PASS": strings.Replace(fail, `"status":"FAIL"`, `"status":"FAIL","status":"PASS"`, 1),
		"duplicate nested outcome":    strings.Replace(pass, `"routes":{"outcome":"PASS"`, `"routes":{"outcome":"FAIL","outcome":"PASS"`, 1),
		"two rows":                    pass + "\n" + pass,
		// codex01 round 2: case-folded aliases the struct decoder merges, last value winning.
		"outcome FAIL then Outcome PASS": strings.Replace(pass, `"routes":{"outcome":"PASS"`, `"routes":{"outcome":"FAIL","Outcome":"PASS"`, 1),
		"run_id foreign then RUN_ID":     strings.Replace(pass, `"run_id":"run-1"`, `"run_id":"run-2","RUN_ID":"run-1"`, 1),
		"status FAIL then long-s alias":  strings.Replace(fail, `"status":"FAIL"`, `"status":"FAIL","ſtatus":"PASS"`, 1),
		"another run":                    passRow(t, func(r map[string]any) { r["binding"] = SoakBinding{SoakVerdictSchema, "run-2", "boot-1", "/run/run-1"} }),
		"another boot":                   passRow(t, func(r map[string]any) { r["binding"] = SoakBinding{SoakVerdictSchema, "run-1", "boot-2", "/run/run-1"} }),
		"another schema":                 passRow(t, func(r map[string]any) { r["binding"] = SoakBinding{"v0", "run-1", "boot-1", "/run/run-1"} }),
		"no binding":                     passRow(t, func(r map[string]any) { delete(r, "binding") }),
		"unknown criterion":              passRow(t, func(r map[string]any) { r["criteria"].(map[string]any)["vibes"] = map[string]any{"outcome": "PASS"} }),
		"unknown outcome": passRow(t, func(r map[string]any) {
			r["status"] = "FAIL"
			r["criteria"].(map[string]any)["routes"] = map[string]any{"outcome": "MAYBE"}
		}),
		"PASS with an error":      passRow(t, func(r map[string]any) { r["error"] = "x" }),
		"unknown field":           passRow(t, func(r map[string]any) { r["extra"] = 1 }),
		"F3 late count on a soak":  passRow(t, func(r map[string]any) { r["late_completions"] = 0 }), // r12: stage 3 F3 only
		"resources observed":      passRow(t, func(r map[string]any) { r["resources"] = "PASS" }),
		"PASS with a FAIL inside": passRow(t, func(r map[string]any) { r["criteria"].(map[string]any)["routes"] = map[string]any{"outcome": "FAIL"} }),
		"unknown status":          passRow(t, func(r map[string]any) { r["status"] = "MAYBE" }),
		// decision 3 revision: every verdict states what an excused miss means.
		"no qualification":    passRow(t, func(r map[string]any) { delete(r, "misses_qualification") }),
		"empty limit":         passRow(t, func(r map[string]any) { r["misses_qualification"].(map[string]any)["limit"] = "" }),
		"qualification extra": passRow(t, func(r map[string]any) { r["misses_qualification"].(map[string]any)["vibes"] = "x" }),
		// rule A rows (codex01 R1 on #110): the field and each member are typed, required and alias-guarded.
		"undecidable alias": passRow(t, func(r map[string]any) { r["MISSES_UNDECIDABLE"] = []any{} }),
		"undecidable op bool": passRow(t, func(r map[string]any) {
			r["misses_undecidable"] = []any{undecidableRow(func(u map[string]any) { u["op"] = true })}
		}),
		"undecidable vid str": passRow(t, func(r map[string]any) {
			r["misses_undecidable"] = []any{undecidableRow(func(u map[string]any) { u["vid"] = "26" })}
		}),
		"undecidable no vid": passRow(t, func(r map[string]any) {
			r["misses_undecidable"] = []any{undecidableRow(func(u map[string]any) { delete(u, "vid") })}
		}),
		"undecidable no early": passRow(t, func(r map[string]any) {
			r["misses_undecidable"] = []any{undecidableRow(func(u map[string]any) { delete(u, "submit_minus_early_b_s") })}
		}),
		"undecidable no late": passRow(t, func(r map[string]any) {
			r["misses_undecidable"] = []any{undecidableRow(func(u map[string]any) { delete(u, "submit_minus_late_b_s") })}
		}),
		"undecidable no op": passRow(t, func(r map[string]any) {
			r["misses_undecidable"] = []any{undecidableRow(func(u map[string]any) { delete(u, "op") })}
		}),
		"undecidable extra": passRow(t, func(r map[string]any) {
			r["misses_undecidable"] = []any{undecidableRow(func(u map[string]any) { u["vibes"] = 1 })}
		}),
		"undecidable OP alias": passRow(t, func(r map[string]any) {
			r["misses_undecidable"] = []any{undecidableRow(func(u map[string]any) { u["OP"] = "x" })}
		}),
		"undecidable not array": passRow(t, func(r map[string]any) { r["misses_undecidable"] = undecidableRow(nil) }),
		"not JSON":              "garbage",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeVerdict(captured([]byte(body)), testBinding, soakCriteria); err == nil {
				t.Fatal("admitted")
			}
		})
	}
	e := captured([]byte(pass))
	e.Code = 1
	if _, err := decodeVerdict(e, testBinding, soakCriteria); err == nil {
		t.Fatal("non-zero exit admitted")
	}
}

// undecidableRow is one valid rule-A row (soak #4 op 36640), edited by the caller.
func undecidableRow(edit func(map[string]any)) map[string]any {
	u := map[string]any{"op": "36640", "vid": 26, "submit_minus_early_b_s": 0.047, "submit_minus_late_b_s": -0.008}
	if edit != nil {
		edit(u)
	}
	return u
}

func mustJSON(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}
