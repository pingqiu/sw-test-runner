package smoke

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestCleanupSweepsOnlyTaggedScopedOrphans(t *testing.T) {
	for _, kind := range []string{"clean", "destroy-failed", "delete-failed", "changed-tag", "foreign-id", "malformed", "unknown-create"} {
		t.Run(kind, func(t *testing.T) {
			c := clusterFixture(t)
			f := &fake{}
			ctl := controller(f)
			base := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/dedicated-test/providers/"
			types := []string{"Microsoft.Network/networkInterfaces", "Microsoft.Network/publicIPAddresses", "Microsoft.Compute/disks", "Microsoft.Network/virtualNetworks", "Microsoft.Network/networkSecurityGroups"}
			rows := []map[string]any{}
			byID := map[string]map[string]any{}
			want := []string{}
			for _, typ := range types {
				id := base + typ + "/orphan"
				r := map[string]any{"id": id, "type": typ, "tags": map[string]string{"seaweed_run": "output"}}
				rows = append([]map[string]any{r}, rows...)
				byID[id] = r
				want = append(want, id)
			}
			rows = append(rows, map[string]any{"id": base + "Microsoft.Compute/disks/foreign", "type": "Microsoft.Compute/disks", "tags": map[string]string{"seaweed_run": "other"}})
			if kind == "foreign-id" {
				rows[0]["id"] = "/subscriptions/else/resourceGroups/else/providers/Microsoft.Network/networkSecurityGroups/foreign"
			}
			deleted := []string{}
			waits := 0
			destroyed := false
			swept := false
			ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
				if tool == "terraform" && args[0] == "apply" && kind == "unknown-create" {
					return nil, errors.New("lost response")
				}
				if tool == "terraform" && args[0] == "destroy" {
					destroyed = true
					if kind == "destroy-failed" {
						return nil, errors.New("subnet in use")
					}
				}
				if tool == "az" && args[0] == "resource" {
					switch args[1] {
					case "list":
						if destroyed && strings.Contains(strings.Join(args, " "), "--resource-group") && !swept {
							swept = true
							if kind == "malformed" {
								return []byte(`[{"id":"x","type":"y","tags":{"seaweed_run":null}}]`), nil
							}
							return json.Marshal(rows)
						}
					case "show":
						r := byID[args[3]]
						if kind == "changed-tag" {
							r["tags"] = map[string]string{"seaweed_run": "other"}
						}
						return json.Marshal(r)
					case "delete":
						if kind == "delete-failed" {
							return nil, errors.New("delete refused")
						}
						deleted = append(deleted, args[3])
						return nil, nil
					case "wait":
						waits++
						return nil, nil
					}
				}
				return f.command(ctx, dir, tool, args...)
			}
			r := ctl.Execute(context.Background(), "run", c)
			if kind == "clean" || kind == "destroy-failed" || kind == "unknown-create" {
				if strings.Join(deleted, ",") != strings.Join(want, ",") || waits != 5 {
					t.Fatalf("delete order/waits: %v %d", deleted, waits)
				}
				if kind == "clean" && r.Code != "COMPLETE" {
					t.Fatal(r)
				}
				if kind == "unknown-create" && (!r.CreateUnresolved || r.Code == "COMPLETE") {
					t.Fatal(r)
				}
			} else if len(deleted) != 0 {
				t.Fatalf("unsafe deletion: %v", deleted)
			}
			if kind != "clean" && (r.Code == "COMPLETE" || r.Code == "EMPTY") {
				t.Fatal(r)
			}
		})
	}
}
