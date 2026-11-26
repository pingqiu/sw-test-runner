package smoke

import (
	"context"
	"encoding/json"
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
		// The reviewer #143 review: only canonical unique keys and identities admission accepts
		{"duplicate RG key (same, then another)", `{"subscription_id":"11111111-1111-1111-1111-111111111111","resource_group_name":"dedicated-test","resource_group_name":"another-rg"}`, true},
		{"case-aliased RG key", `{"subscription_id":"11111111-1111-1111-1111-111111111111","resource_group_name":"dedicated-test","RESOURCE_GROUP_NAME":"another-rg"}`, true},
		{"garbage subscription", `{"subscription_id":"not-a-uuid","resource_group_name":"another-rg"}`, true},
		{"garbage RG", `{"subscription_id":"11111111-1111-1111-1111-111111111111","resource_group_name":"../another"}`, true},
		{"full tfvars, another RG", "FULL:another-rg", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fixture(t)
			d := filepath.Join(filepath.Dir(c.Output), "earlier-run")
			tv := filepath.Join(d, "provision", "terraform", "azure-validation")
			body := []byte(tc.tfvars)
			if rg, full := strings.CutPrefix(tc.tfvars, "FULL:"); full { // the fixture's real tfvars, extra fields kept
				var v map[string]any
				b, err := os.ReadFile(c.Variables)
				if err == nil {
					err = json.Unmarshal(b, &v)
				}
				if err != nil {
					t.Fatal(err)
				}
				v["resource_group_name"] = rg
				if body, err = json.Marshal(v); err != nil {
					t.Fatal(err)
				}
			}
			for _, e := range []error{
				os.MkdirAll(tv, 0700),
				os.WriteFile(filepath.Join(d, "launch.json"), []byte(`{}`), 0600),
				os.WriteFile(filepath.Join(d, "apply-started"), []byte("x"), 0600),
				os.WriteFile(filepath.Join(tv, "run.auto.tfvars.json"), body, 0600),
			} {
				if e != nil {
					t.Fatal(e)
				}
			}
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
