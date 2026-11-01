package smoke

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
)

// From claude03's #46 review: a destroy that loses ONLY a NIC or Public IP
// Delete response must still go through the exact-ID re-read; with an unknown
// re-read (AuthorizationFailed) it refuses. No per-type Delete form bypasses it.
func TestNoDeleteFormBypassesReread(t *testing.T) {
	forms := rereadForms(t)
	for _, tc := range []struct{ name, message string }{
		{"run5-nic-only", forms["exact_nic_destroy_error"]},
		{"public-ip-delete-only", forms["exact_first_destroy_error"]},
		{"run6-disk-only (generic path, for contrast)", forms["exact_disk_destroy_error"]},
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
						return []byte("ERROR: (AuthorizationFailed) denied"), errors.New("az: exit status 1")
					}
					return f.command(ctx, dir, tool, args...)
				}
				r := ctl.Execute(context.Background(), "run", c)
				t.Logf("destroys=%d rereads=%d code=%s", destroys, len(shows), r.Code)
				if len(shows) == 0 || r.Code == "COMPLETE" {
					t.Fatalf("delete reset retried without the exact-ID re-read: destroys=%d shows=%v code=%s", destroys, strings.Join(shows, " "), r.Code)
				}
			})
		})
	}
}
