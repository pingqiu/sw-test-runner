package smoke

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
)

// detachReset is S2-az r12's destroy failure (2026-11-20) in the controller fixture's
// subscription/RG: the data disk detach is a PUT on the VM whose response was lost.
func detachReset(rg, vm string) string {
	return `Error: removing Data Disk: (Name "` + vm + `-cache" / Virtual Machine Name "` + vm + `" / Resource Group "` + rg + `") from Virtual Machine "` + vm + `" : performing CreateOrUpdate: Put "https://management.azure.com/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/` + rg + `/providers/Microsoft.Compute/virtualMachines/` + vm + `?api-version=2024-03-01": HTTP response was nil; connection may have been reset`
}

// A lost detach response re-reads the exact VM; a known answer (present or absent)
// lets the same-state destroy retry; an unknown re-read, another scope, another
// resource type or any other text keeps CLEANUP_INCOMPLETE.
func TestDestroyDetachResetRereadsTheVM(t *testing.T) {
	vmID := rereadBase + "Microsoft.Compute/virtualMachines/fixture-run-node1"
	r12 := detachReset("dedicated-test", "fixture-run-node1")
	for _, tc := range []struct {
		name, message, want string
		show                func(id string) ([]byte, error)
		destroys            int
		shows               []string
	}{
		{"r12-detach-vm-present", r12, "COMPLETE", func(id string) ([]byte, error) { return []byte(`{"id":"` + id + `"}`), nil }, 2, []string{vmID}},
		{"detach-reread-unknown", r12, "CLEANUP_INCOMPLETE", func(string) ([]byte, error) {
			return []byte("ERROR: (AuthorizationFailed) denied"), errors.New("az: exit status 1")
		}, 1, []string{vmID}},
		{"detach-out-of-scope", detachReset("other-rg", "fixture-run-node1"), "CLEANUP_INCOMPLETE", nil, 1, nil},
		{"detach-unknown-suffix", r12 + "\nWrapperFailure", "CLEANUP_INCOMPLETE", nil, 1, nil},
		{"detach-put-not-a-vm", strings.Replace(r12, "Microsoft.Compute/virtualMachines/", "Microsoft.Compute/disks/", 1), "CLEANUP_INCOMPLETE", nil, 1, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := fixture(t)
				f := &fake{}
				ctl := controller(f)
				destroys, shows := 0, []string{}
				ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
					if tool == "terraform" && args[0] == "destroy" {
						destroys++
						if destroys == 1 {
							return []byte(tc.message), errors.New("terraform: exit status 1")
						}
					}
					if tool == "az" && args[0] == "resource" && args[1] == "show" {
						shows = append(shows, args[3])
						if tc.show == nil {
							return []byte("unexpected re-read"), errors.New("az: exit status 1")
						}
						return tc.show(args[3])
					}
					return f.command(ctx, dir, tool, args...)
				}
				r := ctl.Execute(context.Background(), "run", c)
				if destroys != tc.destroys || r.Code != tc.want || strings.Join(shows, " ") != strings.Join(tc.shows, " ") {
					t.Fatalf("destroys=%d shows=%v result=%+v", destroys, shows, r)
				}
			})
		})
	}
}
