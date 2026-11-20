package smoke

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Since RG claims (#141) a run owns its subscription+RG, so the sibling-dir
// gate blocks a new run only on an unfinished sibling that targeted the SAME
// subscription and RG (its saved run.auto.tfvars.json). A sibling whose target
// cannot be read still blocks (fail closed; pre-claims dirs included).
func TestUnfinishedSiblingBlocksOnlyItsOwnRG(t *testing.T) {
	for _, tc := range []struct {
		name, tfvars string
		block        bool
	}{
		{"another RG", `{"subscription_id":"11111111-1111-1111-1111-111111111111","resource_group_name":"another-rg"}`, false},
		{"same RG, other case", `{"subscription_id":"11111111-1111-1111-1111-111111111111","resource_group_name":"DEDICATED-TEST"}`, true},
		{"another subscription, same RG name", `{"subscription_id":"22222222-2222-2222-2222-222222222222","resource_group_name":"dedicated-test"}`, false},
		{"malformed target", `{"subscription_id":`, true},
		{"target without an RG", `{"subscription_id":"11111111-1111-1111-1111-111111111111"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fixture(t)
			d := filepath.Join(filepath.Dir(c.Output), "earlier-run")
			tv := filepath.Join(d, "provision", "terraform", "azure-validation")
			os.MkdirAll(tv, 0700)
			os.WriteFile(filepath.Join(d, "launch.json"), []byte(`{}`), 0600)
			os.WriteFile(filepath.Join(d, "apply-started"), []byte("x"), 0600)
			os.WriteFile(filepath.Join(tv, "run.auto.tfvars.json"), []byte(tc.tfvars), 0600)
			calls := 0
			ctl := controller(&fake{})
			inner := ctl.Command
			ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
				calls++
				return inner(ctx, dir, tool, args...)
			}
			r := ctl.Execute(context.Background(), "run", c)
			if tc.block {
				if r.Code != "RECOVERY_REQUIRED" || calls != 0 || !strings.Contains(r.Error, "earlier-run") {
					t.Fatalf("code %s calls %d error %q, want RECOVERY_REQUIRED naming earlier-run", r.Code, calls, r.Error)
				}
			} else if r.Code == "RECOVERY_REQUIRED" || calls == 0 {
				t.Fatalf("code %s calls %d: an unfinished run in %s must not block", r.Code, calls, tc.name)
			}
		})
	}
}
