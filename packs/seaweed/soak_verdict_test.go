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
	row := map[string]any{"status": "PASS", "criteria": crit, "k7": []any{}, "resources": "NOT_OBSERVED", "binding": testBinding}
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
	if v, err := decodeVerdict(captured([]byte(pass+"\n")), testBinding); err != nil || v.Status != "PASS" {
		t.Fatalf("valid PASS refused: %v", err)
	}
	fail := passRow(t, func(r map[string]any) {
		r["status"] = "FAIL"
		r["criteria"].(map[string]any)["routes"] = map[string]any{"outcome": "FAIL", "reason": "bad"}
	})
	if v, err := decodeVerdict(captured([]byte(fail)), testBinding); err != nil || v.Status != "FAIL" {
		t.Fatalf("valid FAIL not decoded: %v", err)
	}
	errRow := passRow(t, func(r map[string]any) {
		r["status"], r["criteria"], r["error"] = "FAIL", map[string]any{}, "ValueError('x')"
	})
	if v, err := decodeVerdict(captured([]byte(errRow)), testBinding); err != nil || v.Status != "FAIL" {
		t.Fatalf("assessor error row not decoded as FAIL: %v", err)
	}
	for name, body := range map[string]string{
		// codex01's controls: an incomplete PASS, two values on one line, a duplicate status.
		"PASS with only wrong_reads":  `{"status":"PASS","criteria":{"wrong_reads":{"outcome":"PASS","reason":"0 wrong"}},"k7":[],"resources":"NOT_OBSERVED","binding":` + mustJSON(testBinding) + `}`,
		"PASS then FAIL on one line":  pass + " " + fail,
		"duplicate status, last PASS": strings.Replace(fail, `"status":"FAIL"`, `"status":"FAIL","status":"PASS"`, 1),
		"duplicate nested outcome":    strings.Replace(pass, `"routes":{"outcome":"PASS"`, `"routes":{"outcome":"FAIL","outcome":"PASS"`, 1),
		"two rows":                    pass + "\n" + pass,
		"another run":                 passRow(t, func(r map[string]any) { r["binding"] = SoakBinding{SoakVerdictSchema, "run-2", "boot-1", "/run/run-1"} }),
		"another boot":                passRow(t, func(r map[string]any) { r["binding"] = SoakBinding{SoakVerdictSchema, "run-1", "boot-2", "/run/run-1"} }),
		"another schema":              passRow(t, func(r map[string]any) { r["binding"] = SoakBinding{"v0", "run-1", "boot-1", "/run/run-1"} }),
		"no binding":                  passRow(t, func(r map[string]any) { delete(r, "binding") }),
		"unknown criterion":           passRow(t, func(r map[string]any) { r["criteria"].(map[string]any)["vibes"] = map[string]any{"outcome": "PASS"} }),
		"unknown outcome": passRow(t, func(r map[string]any) {
			r["status"] = "FAIL"
			r["criteria"].(map[string]any)["routes"] = map[string]any{"outcome": "MAYBE"}
		}),
		"PASS with an error":      passRow(t, func(r map[string]any) { r["error"] = "x" }),
		"unknown field":           passRow(t, func(r map[string]any) { r["extra"] = 1 }),
		"resources observed":      passRow(t, func(r map[string]any) { r["resources"] = "PASS" }),
		"PASS with a FAIL inside": passRow(t, func(r map[string]any) { r["criteria"].(map[string]any)["routes"] = map[string]any{"outcome": "FAIL"} }),
		"unknown status":          passRow(t, func(r map[string]any) { r["status"] = "MAYBE" }),
		"not JSON":                "garbage",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeVerdict(captured([]byte(body)), testBinding); err == nil {
				t.Fatal("admitted")
			}
		})
	}
	e := captured([]byte(pass))
	e.Code = 1
	if _, err := decodeVerdict(e, testBinding); err == nil {
		t.Fatal("non-zero exit admitted")
	}
}

func mustJSON(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}
