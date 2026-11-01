package smoke

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Post-show external retagging is outside the adopted exclusive-writer RG contract.
func TestReviewerOrphanOwnershipAtDelete(t *testing.T) {
	for _, kind := range []string{"owned", "untagged", "foreign-group", "foreign-subscription", "changed-before-show", "malformed-show", "array-show"} {
		t.Run(kind, func(t *testing.T) {
			c := clusterFixture(t)
			f := &fake{}
			ctl := controller(f)
			id := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/dedicated-test/providers/Microsoft.Network/networkInterfaces/orphan"
			if kind == "foreign-group" {
				id = strings.Replace(id, "/dedicated-test/", "/foreign-group/", 1)
			}
			if kind == "foreign-subscription" {
				id = strings.Replace(id, "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", 1)
			}
			tag := "output"
			if kind == "untagged" {
				tag = "other"
			}
			row := map[string]any{"id": id, "type": "Microsoft.Network/networkInterfaces", "tags": map[string]string{"seaweed_run": tag}, "etag": "original"}
			destroyed, gone := false, false
			deletes := 0
			tagAtDelete := ""
			ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
				if tool == "terraform" && args[0] == "destroy" {
					destroyed = true
				}
				if tool == "az" && args[0] == "resource" && destroyed {
					switch args[1] {
					case "list":
						if gone {
							return []byte("[]"), nil
						}
						return json.Marshal([]any{row})
					case "show":
						if kind == "changed-before-show" {
							row["tags"] = map[string]string{"seaweed_run": "adopted-by-other"}
						}
						snapshot, err := json.Marshal(row)
						if kind == "malformed-show" {
							extra, _ := json.Marshal(map[string]any{"id": id + "-other", "type": "Microsoft.Network/networkInterfaces", "tags": map[string]string{"seaweed_run": "other"}})
							return append(append(snapshot, ','), extra...), nil
						}
						if kind == "array-show" {
							return append(append([]byte("["), snapshot...), ']'), nil
						}
						return snapshot, err
					case "delete":
						deletes++
						tagAtDelete = row["tags"].(map[string]string)["seaweed_run"]
						gone = true
						return nil, nil
					case "wait":
						return nil, nil
					}
				}
				return f.command(ctx, dir, tool, args...)
			}
			result := ctl.Execute(context.Background(), "run", c)
			if kind == "owned" {
				if deletes != 1 || result.Code != "COMPLETE" {
					t.Fatalf("owned control: deletes=%d result=%+v", deletes, result)
				}
			} else if deletes != 0 || result.Code == "COMPLETE" || result.Code == "EMPTY" {
				t.Fatalf("foreign ownership must refuse: deletes=%d tag_at_delete=%q result=%+v", deletes, tagAtDelete, result)
			}
		})
	}
}
