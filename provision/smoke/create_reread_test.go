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

// Run 7b: vol2's Public IP PUT lost its response (005) but Azure created it
// outside state, so the plain retry failed "already exists" (007). The fake
// apply 2 behaves like Azure did: it fails 007 unless the IP was imported.
func TestCreateResetRereadsAndImports(t *testing.T) {
	body, err := os.ReadFile("testdata/arm-public-ip-transport.json")
	if err != nil {
		t.Fatal(err)
	}
	var forms map[string]string
	if err = json.Unmarshal(body, &forms); err != nil {
		t.Fatal(err)
	}
	lost, exists := forms["run7b_ip_create_lost_put"], forms["run7b_ip_already_exists"]
	id := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/dedicated-test/providers/Microsoft.Network/publicIPAddresses/output-vol2-ip"
	address := `module.run.azurerm_public_ip.node["vol2"]`
	object := func(tag string) func() ([]byte, error) {
		return func() ([]byte, error) {
			return json.Marshal(map[string]any{"id": id, "type": "Microsoft.Network/publicIPAddresses", "tags": map[string]string{"seaweed_run": tag}})
		}
	}
	plan := func(actions ...string) []byte {
		b, _ := json.Marshal(map[string]any{"resource_changes": []any{map[string]any{"address": address, "change": map[string]any{"actions": actions}}}})
		return b
	}
	for _, tc := range []struct {
		name, message string
		show          func() ([]byte, error)
		planned       []byte
		importFails   bool
		want          string
		applies       int
		imports       int
	}{
		{"run7b-present-is-imported", lost, object("output"), plan("no-op"), false, "CREATE_UNRESOLVED", 2, 1},
		{"absent-retries-as-before", lost, func() ([]byte, error) { return absentShow(id), errors.New("az: exit status 3") }, nil, false, "CREATE_UNRESOLVED", 2, 0},
		{"foreign-tag-refuses", lost, object("someone-else"), nil, false, "APPLY_FAILED_OR_UNKNOWN", 1, 0},
		{"unknown-read-refuses", lost, func() ([]byte, error) {
			return []byte("ERROR: (AuthorizationFailed) denied"), errors.New("az: exit status 1")
		}, nil, false, "APPLY_FAILED_OR_UNKNOWN", 1, 0},
		{"no-address-refuses", lost[:strings.Index(lost, "\n\n  with ")], object("output"), nil, false, "APPLY_FAILED_OR_UNKNOWN", 1, 0},
		{"out-of-scope-refuses", strings.ReplaceAll(lost, "dedicated-test", "other-rg"), object("output"), nil, false, "APPLY_FAILED_OR_UNKNOWN", 1, 0},
		{"import-failure-refuses", lost, object("output"), nil, true, "APPLY_FAILED_OR_UNKNOWN", 1, 1},
		{"replan-replacing-import-refuses", lost, object("output"), plan("delete", "create"), false, "APPLY_FAILED_OR_UNKNOWN", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := fixture(t)
				f := &fake{}
				ctl := controller(f)
				applies, imports, shows := 0, []string{}, 0
				ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
					switch {
					case tool == "az" && args[0] == "resource" && args[1] == "show":
						shows++
						if args[3] != id {
							t.Fatal("re-read a different ID", args)
						}
						return tc.show()
					case tool == "terraform" && args[0] == "import":
						imports = append(imports, strings.Join(args[len(args)-2:], " "))
						if tc.importFails {
							return []byte("Error: Cannot import"), errors.New("terraform: exit status 1")
						}
						return nil, nil
					case tool == "terraform" && args[0] == "show":
						return tc.planned, nil
					case tool == "terraform" && args[0] == "apply":
						applies++
						if applies == 1 {
							return []byte(tc.message), errors.New("terraform: exit status 1")
						}
						if len(imports) == 0 && tc.name == "run7b-present-is-imported" {
							return []byte(exists), errors.New("terraform: exit status 1") // what run 7b's 007 saw
						}
					}
					return f.command(ctx, dir, tool, args...)
				}
				r := ctl.Execute(context.Background(), "run", c)
				if r.Code != tc.want || applies != tc.applies || len(imports) != tc.imports {
					t.Fatalf("applies=%d imports=%v shows=%d result=%+v", applies, imports, shows, r)
				}
				if tc.imports == 1 && imports[0] != address+" "+id {
					t.Fatalf("imported %q", imports[0])
				}
				if tc.name == "no-address-refuses" && shows != 0 {
					t.Fatal("re-read without an address")
				}
			})
		})
	}
}
