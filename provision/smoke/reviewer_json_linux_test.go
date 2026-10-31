//go:build linux

package smoke

import (
	"context"
	"strings"
	"testing"
)

func TestReviewerMalformedSubscriptionCannotCertifyEmpty(t *testing.T) {
	const id = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/leaked/providers/Microsoft.Compute/virtualMachines/leftover"
	for name, raw := range map[string]string{
		"duplicate-type":  `[{"id":"` + id + `","type":"Microsoft.Compute/virtualMachines","type":"Microsoft.Network/virtualNetworks"}]`,
		"type-case-alias": `[{"id":"` + id + `","type":"Microsoft.Compute/virtualMachines","Type":"Microsoft.Network/virtualNetworks"}]`,
		"null-tag-value":  `[{"id":"/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/leaked/providers/Microsoft.Network/networkInterfaces/leftover","type":"Microsoft.Network/networkInterfaces","tags":{"seaweed_run":null}}]`,
		"truncated":       `[{"id":"unfinished"`,
		"null-row":        `[null]`,
	} {
		t.Run(name, func(t *testing.T) {
			c := clusterFixture(t)
			f := &fake{}
			if r := controller(f).Execute(context.Background(), "run", c); r.Code != "COMPLETE" {
				t.Fatal(r)
			}
			ctl := Controller{Command: func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
				if tool != "az" || args[0] != "resource" {
					t.Fatal("unexpected", tool, args)
				}
				if strings.Contains(strings.Join(args, " "), "--resource-group") {
					return []byte(`[]`), nil
				}
				return []byte(raw), nil
			}}
			r := ctl.Execute(context.Background(), "verify", c)
			t.Logf("case=%s code=%s observed=%t remaining=%v summary=%s error=%s", name, r.Code, r.RemainingObserved, r.Remaining, r.RemainingSummary, r.Error)
			if r.Code == "EMPTY" || r.Code == "COMPLETE" || r.RemainingObserved {
				t.Fatalf("malformed subscription JSON certified clean: %+v", r)
			}
		})
	}
}
