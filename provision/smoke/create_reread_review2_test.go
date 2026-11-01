package smoke

import (
	"context"
	"testing"
)

func TestCreateCorrectiveStandaloneIDNeedsAddress(t *testing.T) {
	raw := []byte(`Put "https://management.azure.com/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/dedicated-test/providers/Microsoft.Network/publicIPAddresses/output-vol2-ip?api-version=2025-07-01": EOF`)
	if !transportOnly(raw) {
		t.Fatal("not an accepted control envelope")
	}
	calls := 0
	_, err := settleCreates(context.Background(), func(context.Context, string, ...string) ([]byte, error) { calls++; return nil, nil }, raw, "11111111-1111-1111-1111-111111111111", "dedicated-test", "output")
	if err == nil || calls != 0 {
		t.Fatalf("unbound named ID must refuse before contact: calls=%d err=%v", calls, err)
	}
}

func TestCreateCorrectivePlanUnknownsRefuse(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		ok        bool
	}{
		{"null", `null`, false},
		{"empty", `{}`, false},
		{"missing-address", `{"resource_changes":[]}`, false},
		{"missing-actions", `{"resource_changes":[{"address":"owned","change":{}}]}`, false},
		{"unknown-action", `{"resource_changes":[{"address":"owned","change":{"actions":["future-action"]}}]}`, false},
		{"ambiguous-replacement", `{"resource_changes":[{"address":"owned","change":{"actions":["delete","create"],"actions":["no-op"]}}]}`, false},
		{"replace-create-first", `{"resource_changes":[{"address":"owned","change":{"actions":["create","delete"]}}]}`, false},
		{"replace-delete-first", `{"resource_changes":[{"address":"owned","change":{"actions":["delete","create"]}}]}`, false},
		{"no-op", `{"resource_changes":[{"address":"owned","change":{"actions":["no-op"]}}]}`, true},
		{"update", `{"resource_changes":[{"address":"owned","change":{"actions":["update"]}}]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := keepsImported(context.Background(), func(ctx context.Context, tool string, args ...string) ([]byte, error) {
				if tool != "terraform" || len(args) != 4 || args[0] != "show" || args[1] != "-json" || args[3] != "smoke.tfplan" {
					t.Fatalf("wrong plan read: %s %v", tool, args)
				}
				return []byte(tc.raw), nil
			}, []string{"owned"})
			if (err == nil) != tc.ok {
				t.Fatalf("ok=%v err=%v", tc.ok, err)
			}
		})
	}
}
