package seaweed

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

const cleanCloudInit = `{"status":"done","errors":[],"recoverable_errors":{}}`

// Reconstructed client2-shaped status, NOT a raw JSON capture from run 2.
const client2Warning = `Polling IMDS failed attempt 2 with exception: UrlError('410 Client Error: Gone for url: http://169.254.169.254/metadata/instance?api-version=2021-08-01&extended=true')`

func TestCloudInitJSONGate(t *testing.T) {
	warning := fmt.Sprintf(`{"status":"done","errors":[],"recoverable_errors":{"WARNING":[%q]},"init":{"errors":[],"recoverable_errors":{"WARNING":[%q]}}}`, client2Warning, client2Warning)
	for _, c := range []struct {
		name, raw string
		rc        int
		pass      bool
	}{
		{"clean", cleanCloudInit, 0, true}, {"client2", warning, 2, true},
		{"rc0 unknown warning", strings.ReplaceAll(warning, "Polling IMDS", "Unknown IMDS"), 0, false},
		{"wrong endpoint", strings.ReplaceAll(warning, "169.254.169.254", "10.0.0.1"), 2, false},
		{"mixed warning", strings.Replace(warning, `"WARNING":[`, `"WARNING":["unknown",`, 1), 2, false},
		{"fatal", strings.Replace(cleanCloudInit, `"errors":[]`, `"errors":["bad"]`, 1), 0, false},
		{"nested fatal", strings.TrimSuffix(cleanCloudInit, "}") + `,"init":{"errors":["bad"]}}`, 2, false},
		{"error level", strings.ReplaceAll(warning, "WARNING", "ERROR"), 2, false},
		{"running", strings.Replace(cleanCloudInit, "done", "running", 1), 0, false},
		{"crash", cleanCloudInit, 1, false}, {"timeout", cleanCloudInit, 124, false},
		{"missing fields", `{"status":"done"}`, 0, false}, {"null", `{"status":"done","errors":[],"recoverable_errors":null}`, 0, false},
		{"duplicate", `{"status":"running","status":"done","errors":[],"recoverable_errors":{}}`, 0, false},
		{"alias", strings.Replace(cleanCloudInit, `"errors"`, `"Errors"`, 1), 0, false},
		{"trailing", cleanCloudInit + ` {}`, 0, false}, {"malformed", `{`, 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := qualifyCloudInit(c.raw, c.rc)
			if (err == nil) != c.pass {
				t.Fatalf("pass=%v err=%v", c.pass, err)
			}
		})
	}
}

// Exact raw client2 status from Azure10 run 7d: attempts 1 and 2 of the same
// reprovisiondata 404 poll ("degraded done"). The pin refused attempt 2.
func TestCapturedClient2Reprovision404Attempt2(t *testing.T) {
	raw, err := os.ReadFile("testdata/azure-client2-imds404-attempt2.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := qualifyCloudInit(string(raw), 2); err != nil {
		t.Fatal(err)
	}
	if qualifyCloudInit(strings.ReplaceAll(string(raw), "404 Client Error: Not Found", "500 Server Error"), 2) == nil {
		t.Fatal("other status accepted")
	}
}

// Exact raw vol1 status from Azure10 run4, including all nested repeated warnings.
func TestCapturedVol1Reprovision404(t *testing.T) {
	raw, err := os.ReadFile("testdata/azure-vol1-imds404.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, raw string
		pass      bool
	}{
		{"captured", string(raw), true},
		{"foreign-host", strings.ReplaceAll(string(raw), "169.254.169.254", "10.0.0.1"), false},
		{"foreign-path", strings.ReplaceAll(string(raw), "metadata/reprovisiondata", "metadata/instance"), false},
		{"foreign-api", strings.ReplaceAll(string(raw), "2019-06-01", "2021-08-01"), false},
		{"other-status", strings.ReplaceAll(string(raw), "404 Client Error: Not Found", "410 Client Error: Gone"), false},
		{"other-attempt", strings.ReplaceAll(string(raw), "attempt 1", "attempt 12"), true},
		{"attempt-zero", strings.ReplaceAll(string(raw), "attempt 1", "attempt 0"), false},
		{"attempt-not-a-number", strings.ReplaceAll(string(raw), "attempt 1", "attempt 1x"), false},
		{"mixed-warning", strings.Replace(string(raw), "Polling IMDS", "Unknown warning; Polling IMDS", 1), false},
		{"fatal", strings.Replace(string(raw), `"errors": []`, `"errors": ["fatal"]`, 1), false},
		{"nested-fatal", strings.Replace(string(raw), `"init": {`, `"init": {"extra":{"errors":["fatal"]},`, 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := qualifyCloudInit(tc.raw, 2); (err == nil) != tc.pass {
				t.Fatalf("pass=%v error=%v", tc.pass, err)
			}
		})
	}
}
