package seaweed

import (
	"strings"
	"testing"
)

// The verdict row is decoded strictly: exactly one row, known fields only,
// resources NOT_OBSERVED, and a PASS that agrees with its criteria.
func TestDecodeVerdictFailsClosed(t *testing.T) {
	pass := `{"status":"PASS","criteria":{"wrong_reads":{"outcome":"PASS","reason":"0 wrong"}},"k7":[],"resources":"NOT_OBSERVED"}`
	if v, err := decodeVerdict(captured([]byte(pass + "\n"))); err != nil || v.Status != "PASS" {
		t.Fatalf("valid PASS refused: %v", err)
	}
	fail := strings.Replace(pass, `"status":"PASS","criteria":{"wrong_reads":{"outcome":"PASS"`, `"status":"FAIL","criteria":{"wrong_reads":{"outcome":"FAIL"`, 1)
	if v, err := decodeVerdict(captured([]byte(fail))); err != nil || v.Status != "FAIL" {
		t.Fatalf("valid FAIL not decoded: %v", err)
	}
	for name, body := range map[string]string{
		"two rows":                pass + "\n" + pass,
		"unknown field":           strings.Replace(pass, `"k7":[]`, `"k7":[],"extra":1`, 1),
		"resources observed":      strings.Replace(pass, `NOT_OBSERVED`, `PASS`, 1),
		"PASS with a FAIL inside": strings.Replace(pass, `"outcome":"PASS"`, `"outcome":"FAIL"`, 1),
		"PASS without criteria":   `{"status":"PASS","criteria":{},"k7":[],"resources":"NOT_OBSERVED"}`,
		"unknown status":          strings.Replace(pass, `"status":"PASS"`, `"status":"MAYBE"`, 1),
		"not JSON":                "garbage",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeVerdict(captured([]byte(body))); err == nil {
				t.Fatal("admitted")
			}
		})
	}
	e := captured([]byte(pass))
	e.Code = 1
	if _, err := decodeVerdict(e); err == nil {
		t.Fatal("non-zero exit admitted")
	}
}
