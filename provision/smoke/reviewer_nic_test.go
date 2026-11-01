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

func TestReviewerNICEnvelopeBoundary(t *testing.T) {
	bytes, err := os.ReadFile("testdata/arm-public-ip-transport.json")
	if err != nil {
		t.Fatal(err)
	}
	var captured map[string]string
	if err = json.Unmarshal(bytes, &captured); err != nil {
		t.Fatal(err)
	}
	message := captured["exact_nic_apply_error"]
	contextAt := strings.Index(message, "\n\n  with ")
	if contextAt < 0 {
		t.Fatal("missing captured source context")
	}
	cases := []struct {
		name, diagnostic string
		wantCalls        int
		wantCode         string
	}{
		{"captured", message, 2, "CREATE_UNRESOLVED"},
		{"missing-source-context", message[:contextAt], 1, "APPLY_FAILED_OR_UNKNOWN"},
		{"foreign-arm-resource-path", strings.Replace(message, "/Microsoft.Network/networkInterfaces/", "/Microsoft.Network/publicIPAddresses/", 1), 1, "APPLY_FAILED_OR_UNKNOWN"},
		{"unknown-arm-operation-path", strings.Replace(message, "/Microsoft.Network/networkInterfaces/", "/Microsoft.Compute/virtualMachines/", 1), 1, "APPLY_FAILED_OR_UNKNOWN"},
		{"same-block-provider-error", strings.Replace(message, "HTTP response was nil; connection may have been reset", "HTTP response was nil; connection may have been reset; BadRequest", 1), 1, "APPLY_FAILED_OR_UNKNOWN"},
		{"context-resource-mismatch", strings.ReplaceAll(message, "azurerm_network_interface", "azurerm_public_ip"), 1, "APPLY_FAILED_OR_UNKNOWN"},
		{"unknown-prefix", "WrapperFailure\n" + message, 1, "APPLY_FAILED_OR_UNKNOWN"},
		{"unknown-suffix", message + "\nWrapperFailure", 1, "APPLY_FAILED_OR_UNKNOWN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := fixture(t)
				f := &fake{}
				ctl := controller(f)
				calls := 0
				ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
					if tool == "terraform" && args[0] == "apply" {
						calls++
						if calls == 1 {
							return []byte(tc.diagnostic), errors.New("exit 1")
						}
					}
					return f.command(ctx, dir, tool, args...)
				}
				result := ctl.Execute(context.Background(), "run", c)
				if calls != tc.wantCalls || result.Code != tc.wantCode {
					t.Fatalf("applies=%d want=%d code=%s want=%s unresolved=%v", calls, tc.wantCalls, result.Code, tc.wantCode, result.CreateUnresolved)
				}
				if calls > 1 && !result.CreateUnresolved {
					t.Fatal("retry cleared UNKNOWN")
				}
			})
		})
	}
}
