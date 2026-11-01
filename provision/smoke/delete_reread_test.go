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

const rereadBase = "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/dedicated-test/providers/"

func rereadForms(t *testing.T) map[string]string {
	body, err := os.ReadFile("testdata/arm-public-ip-transport.json")
	if err != nil {
		t.Fatal(err)
	}
	var forms map[string]string
	if err = json.Unmarshal(body, &forms); err != nil {
		t.Fatal(err)
	}
	for k, v := range forms {
		forms[k] = strings.ReplaceAll(v, "fixture-rg", "dedicated-test") // the controller fixture's RG
	}
	return forms
}

// notFound is the run6 az ResourceNotFound text for another disk name.
func notFound(forms map[string]string, name string) string {
	return strings.ReplaceAll(forms["exact_disk_not_found"], "fixture-run-vol6-os", name)
}

// Run5 lost a NIC Delete response, run6 a Managed Disk one. Destroy re-reads
// each exact ID: absent is deleted, present is re-deleted by the same-state
// retry; an unknown re-read or any other text refuses.
func TestDestroyDeleteResetRereadsExactIDs(t *testing.T) {
	forms := rereadForms(t)
	disk, nic := forms["exact_disk_destroy_error"], forms["exact_nic_destroy_error"]
	diskID := rereadBase + "Microsoft.Compute/disks/fixture-run-vol5-os"
	nicID := rereadBase + "Microsoft.Network/networkInterfaces/fixture-run-vol4-nic"
	raw := map[string]string{"fixture-rg": strings.ReplaceAll(disk, "dedicated-test", "fixture-rg")}
	for _, tc := range []struct {
		name, message, want string
		show                func(id string) ([]byte, error)
		destroys            int
		shows               []string
	}{
		{"run6-disk-absent", disk, "COMPLETE", func(string) ([]byte, error) {
			return []byte(notFound(forms, "fixture-run-vol5-os")), errors.New("az: exit status 3")
		}, 2, []string{diskID}},
		{"run6-disk-present", disk, "COMPLETE", func(id string) ([]byte, error) { return []byte(`{"id":"` + id + `"}`), nil }, 2, []string{diskID}},
		{"run5-nic-and-run6-disk", strings.TrimSpace(nic) + "\n\n" + disk, "COMPLETE", func(id string) ([]byte, error) {
			return []byte(`{"id":"` + id + `"}`), nil
		}, 2, []string{nicID, diskID}},
		{"reread-unknown", disk, "CLEANUP_INCOMPLETE", func(string) ([]byte, error) {
			return []byte("ERROR: (AuthorizationFailed) denied"), errors.New("az: exit status 1")
		}, 1, []string{diskID}},
		{"not-found-for-another-id", disk, "CLEANUP_INCOMPLETE", func(string) ([]byte, error) {
			return []byte(notFound(forms, "fixture-run-vol6-os")), errors.New("az: exit status 3")
		}, 1, []string{diskID}},
		{"out-of-scope-id", raw["fixture-rg"], "CLEANUP_INCOMPLETE", nil, 1, nil},
		{"unknown-suffix", disk + "\nWrapperFailure", "CLEANUP_INCOMPLETE", nil, 1, nil},
		{"put-is-not-delete", strings.Replace(disk, "performing Delete: Delete", "performing CreateOrUpdate: Put", 1), "CLEANUP_INCOMPLETE", nil, 1, nil},
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

// Nothing changes for create/PUT: apply never takes the generic delete rule.
func TestApplyDoesNotTakeTheGenericDeleteRule(t *testing.T) {
	forms := rereadForms(t)
	synctest.Test(t, func(t *testing.T) {
		c := fixture(t)
		f := &fake{}
		ctl := controller(f)
		applies, shows := 0, 0
		ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
			if tool == "terraform" && args[0] == "apply" {
				applies++
				if applies == 1 {
					return []byte(forms["exact_disk_destroy_error"]), errors.New("terraform: exit status 1")
				}
			}
			if tool == "az" && args[0] == "resource" && args[1] == "show" {
				shows++
			}
			return f.command(ctx, dir, tool, args...)
		}
		r := ctl.Execute(context.Background(), "run", c)
		if applies != 1 || shows != 0 || r.Code != "APPLY_FAILED_OR_UNKNOWN" {
			t.Fatalf("applies=%d shows=%d result=%+v", applies, shows, r)
		}
	})
}

// Sweep: ResourceNotFound on the fresh show is already deleted (run6 step
// 083); a reset delete re-reads, absent is done, present is deleted again.
func TestSweepRereadsInsteadOfStopping(t *testing.T) {
	forms := rereadForms(t)
	disk, vnet := rereadBase+"Microsoft.Compute/disks/fixture-run-vol6-os", rereadBase+"Microsoft.Network/virtualNetworks/fixture-run-vnet"
	reset := "('Connection aborted.', ConnectionResetError(104, 'Connection reset by peer'))"
	for _, tc := range []struct {
		name    string
		deletes []string
		ok      bool
	}{
		{"run6-not-found", []string{vnet}, true},
		{"delete-reset-absent", []string{vnet}, true},
		{"delete-reset-present", []string{vnet, vnet}, true},
		{"show-unknown", nil, false},
		{"delete-other-error", []string{vnet}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deletes, vnetShows := []string{}, 0
			call := func(_ context.Context, tool string, args ...string) ([]byte, error) {
				switch args[1] {
				case "list":
					rows := []map[string]any{}
					for _, r := range [][2]string{{disk, "Microsoft.Compute/disks"}, {vnet, "Microsoft.Network/virtualNetworks"}} {
						rows = append(rows, map[string]any{"id": r[0], "type": r[1], "tags": map[string]string{"seaweed_run": "fixture-run"}})
					}
					return json.Marshal(rows)
				case "show":
					if args[3] == disk {
						if tc.name == "show-unknown" {
							return []byte("ERROR: (AuthorizationFailed) denied"), errors.New("az: exit status 1")
						}
						return []byte(forms["exact_disk_not_found"]), errors.New("az: exit status 3")
					}
					vnetShows++
					if vnetShows == 2 && tc.name == "delete-reset-absent" {
						return []byte(strings.ReplaceAll(forms["exact_disk_not_found"], "Microsoft.Compute/disks/fixture-run-vol6-os", "Microsoft.Network/virtualNetworks/fixture-run-vnet")), errors.New("az: exit status 3")
					}
					return json.Marshal(map[string]any{"id": vnet, "type": "Microsoft.Network/virtualNetworks", "tags": map[string]string{"seaweed_run": "fixture-run"}})
				case "delete":
					deletes = append(deletes, args[3])
					if len(deletes) == 1 && strings.HasPrefix(tc.name, "delete-reset") {
						return []byte(reset), errors.New("az: exit status 1")
					}
					if tc.name == "delete-other-error" {
						return []byte("ERROR: (InUseSubnetCannotBeDeleted) in use"), errors.New("az: exit status 1")
					}
				}
				return []byte("{}"), nil
			}
			e := sweepOrphans(context.Background(), "11111111-1111-1111-1111-111111111111", "dedicated-test", "fixture-run", call)
			if (e == nil) != tc.ok || strings.Join(deletes, " ") != strings.Join(tc.deletes, " ") {
				t.Fatalf("err=%v deletes=%v", e, deletes)
			}
		})
	}
}
