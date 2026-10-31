package seaweed

import (
	"fmt"
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
