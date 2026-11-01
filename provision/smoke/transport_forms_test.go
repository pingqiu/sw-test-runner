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
		{"real-nic-apply-envelope", "apply", captured["exact_nic_apply_error"], "CREATE_UNRESOLVED", 2},
		{"nic-unknown-prefix", "apply", "FATAL InvalidParameter\n" + captured["exact_nic_apply_error"], "APPLY_FAILED_OR_UNKNOWN", 1},
		{"nic-provider-suffix", "apply", captured["exact_nic_apply_error"] + "\nBadRequest", "APPLY_FAILED_OR_UNKNOWN", 1},
		{"nic-wrong-operation", "apply", strings.Replace(captured["exact_nic_apply_error"], "creating Network Interface", "updating Network Interface", 1), "APPLY_FAILED_OR_UNKNOWN", 1},
		{"nic-foreign-source", "apply", strings.ReplaceAll(captured["exact_nic_apply_error"], "azurerm_network_interface", "azurerm_public_ip"), "APPLY_FAILED_OR_UNKNOWN", 1},
		{"real-apply-envelope", "apply", captured["exact_apply_error"], "CREATE_UNRESOLVED", 2},
		// Delete resets no longer use a form allowlist: they go through the
		// re-read rule (delete_reread_test.go). These fixture IDs are outside
		// the fixture's RG, so the rule refuses them.
		{"real-destroy-envelope", "destroy", captured["exact_first_destroy_error"], "CLEANUP_INCOMPLETE", 1},
		{"real-nic-destroy-envelope", "destroy", captured["exact_nic_destroy_error"], "CLEANUP_INCOMPLETE", 1},
		{"nic-destroy-source-context", "destroy", strings.TrimSpace(captured["exact_nic_destroy_error"]) + "\n\n  with module.run.azurerm_network_interface.node[\"vol4\"],", "CLEANUP_INCOMPLETE", 1},
		{"nic-destroy-foreign-path", "destroy", strings.Replace(captured["exact_nic_destroy_error"], "/networkInterfaces/", "/networkSecurityGroups/", 1), "CLEANUP_INCOMPLETE", 1},
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
