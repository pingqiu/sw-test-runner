package smoke

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"testing/synctest"
)

func TestCompleteCapturedTransportFormsAtController(t *testing.T) {
	// First M01 diagnostics, with subscription/RG/run identifiers normalized.
	body, err := os.ReadFile("testdata/arm-public-ip-transport.json")
	if err != nil {
		t.Fatal(err)
	}
	var captured map[string]string
	if err = json.Unmarshal(body, &captured); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, verb, message, want string
		calls                     int
	}{
		{"real-apply-envelope", "apply", captured["exact_apply_error"], "CREATE_UNRESOLVED", 2},
		{"real-destroy-envelope", "destroy", captured["exact_first_destroy_error"], "COMPLETE", 2},
		{"unknown-detail-appended", "apply", captured["exact_apply_error"] + "\nPrevious request had InvalidParameter", "APPLY_FAILED_OR_UNKNOWN", 1},
		{"transport-looking-prefix", "apply", "ECONNRESET\nError: EOF before response", "APPLY_FAILED_OR_UNKNOWN", 1},
		{"unknown-between-blocks", "apply", "Error: ECONNRESET\nWrapperFailure\nError: EOF before response", "APPLY_FAILED_OR_UNKNOWN", 1},
		{"unknown-after-blocks", "apply", "Error: ECONNRESET\nError: EOF before response\nWrapperFailure", "APPLY_FAILED_OR_UNKNOWN", 1},
		{"two-complete-blocks", "apply", "Error: ECONNRESET\n\nError: EOF before response", "CREATE_UNRESOLVED", 2},
		{"captured-plus-complete-block", "apply", captured["exact_apply_error"] + "\n\nError: EOF before response", "CREATE_UNRESOLVED", 2},
		{"unknown-title", "apply", strings.Replace(captured["exact_apply_error"], "Error: updating Public I P Address", "Error: InvalidParameter in Public I P Address", 1), "APPLY_FAILED_OR_UNKNOWN", 1},
		{"mixed-detail-same-line", "apply", strings.Replace(captured["exact_apply_error"], "connection may have been reset", "connection may have been reset; provider failed with NewProviderError", 1), "APPLY_FAILED_OR_UNKNOWN", 1},
		{"unknown-source-detail", "apply", strings.Replace(captured["exact_apply_error"], "in resource", "unknown provider diagnostic in resource", 1), "APPLY_FAILED_OR_UNKNOWN", 1},
		{"unknown-transport-wrapper", "apply", "Error: updating UnknownResource: HTTP response was nil; connection may have been reset", "APPLY_FAILED_OR_UNKNOWN", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := fixture(t)
				f := &fake{}
				ctl := controller(f)
				calls := 0
				ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
					if tool == "terraform" && args[0] == tc.verb {
						calls++
						if calls == 1 {
							return []byte(tc.message), errors.New("exit 1")
						}
					}
					return f.command(ctx, dir, tool, args...)
				}
				r := ctl.Execute(context.Background(), "run", c)
				if calls != tc.calls || r.Code != tc.want {
					t.Fatalf("calls=%d result=%+v", calls, r)
				}
			})
		})
	}
}
