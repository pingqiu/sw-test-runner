package smoke

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// #140: the cluster subscription scan is scoped to the run. An untagged VM,
// disk or public IP in ANOTHER resource group (another seat's run) neither
// blocks the pre-create check nor counts as this run's residue; it is
// reported in subscription_other_billable. A resource tagged for this run
// anywhere, or a billable resource in the run's own RG, still does.
func TestClusterSubscriptionScanScopedToRun(t *testing.T) {
	const sub = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/"
	for _, tc := range []struct {
		name, id, typ, tag string
		clean              bool
	}{
		{"untagged VM in another RG", sub + "someone-else/providers/Microsoft.Compute/virtualMachines/vm1", "Microsoft.Compute/virtualMachines", "", true},
		{"untagged disk in another RG", sub + "someone-else/providers/Microsoft.Compute/disks/d1", "Microsoft.Compute/disks", "", true},
		{"other run's tagged VM in another RG", sub + "someone-else/providers/Microsoft.Compute/virtualMachines/vm2", "Microsoft.Compute/virtualMachines", "another-run", true},
		{"untagged VM in the run's own RG", sub + "Dedicated-Test/providers/Microsoft.Compute/virtualMachines/vm3", "Microsoft.Compute/virtualMachines", "", false},
		{"this run's tagged NIC in another RG", sub + "someone-else/providers/Microsoft.Network/networkInterfaces/n1", "Microsoft.Network/networkInterfaces", "output", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := clusterFixture(t)
			f := &fake{}
			ctl := controller(f)
			ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
				if tool == "az" && args[0] == "resource" && !strings.Contains(strings.Join(args, " "), "--resource-group") {
					row := map[string]any{"id": tc.id, "type": tc.typ}
					if tc.tag != "" {
						row["tags"] = map[string]string{"seaweed_run": tc.tag}
					}
					return json.Marshal([]any{row})
				}
				return f.command(ctx, dir, tool, args...)
			}
			r := ctl.Execute(context.Background(), "run", c)
			if tc.clean {
				if r.Code != "COMPLETE" || len(r.Remaining) != 0 {
					t.Fatalf("another run's resource blocked or poisoned this run: %+v", r)
				}
			} else if r.Code == "COMPLETE" || r.Code == "EMPTY" {
				t.Fatalf("this run's residue certified clean: %+v", r)
			}
		})
	}
}

// The report-only list names the other RGs' billable resources.
func TestClusterSubscriptionOtherBillableReported(t *testing.T) {
	c := clusterFixture(t)
	f := &fake{}
	ctl := controller(f)
	id := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/someone-else/providers/Microsoft.Network/publicIPAddresses/ip1"
	ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
		if tool == "az" && args[0] == "resource" && !strings.Contains(strings.Join(args, " "), "--resource-group") {
			return json.Marshal([]any{map[string]any{"id": id, "type": "Microsoft.Network/publicIPAddresses"}})
		}
		return f.command(ctx, dir, tool, args...)
	}
	r := ctl.Execute(context.Background(), "run", c)
	if r.Code != "COMPLETE" || len(r.OtherBillable) != 1 || r.OtherBillable[0] != id {
		t.Fatalf("%+v", r)
	}
}
