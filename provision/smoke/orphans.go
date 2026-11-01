package smoke

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Only top-level resources in this subscription/RG with this exact run tag
// are candidates. Validate the whole list before issuing any deletion.
func orphanIDs(raw []byte, subscription, group, run string) ([]string, error) {
	if _, err := resourceIDs(raw, run, true); err != nil {
		return nil, err
	}
	var rows []struct {
		ID, Type string
		Tags     map[string]string
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	order := map[string]int{"microsoft.network/networkinterfaces": 0, "microsoft.network/publicipaddresses": 1, "microsoft.compute/disks": 2, "microsoft.network/virtualnetworks": 3, "microsoft.network/networksecuritygroups": 4}
	ranks := map[string]int{}
	prefix := "/subscriptions/" + subscription + "/resourceGroups/" + group + "/providers/"
	for _, r := range rows {
		if r.Tags["seaweed_run"] != run {
			continue
		}
		parts := strings.Split(r.ID, "/")
		rank, ok := order[strings.ToLower(r.Type)]
		if !ok || len(parts) != 9 || !strings.EqualFold(strings.Join(parts[:6], "/")+"/", prefix) || !strings.EqualFold(parts[6]+"/"+parts[7], r.Type) || parts[8] == "" {
			return nil, fmt.Errorf("unsupported or out-of-scope tagged orphan: %s", r.ID)
		}
		ranks[r.ID] = rank
	}
	ids := []string{}
	for id := range ranks {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if ranks[ids[i]] != ranks[ids[j]] {
			return ranks[ids[i]] < ranks[ids[j]]
		}
		return ids[i] < ids[j]
	})
	return ids, nil
}

func sweepOrphans(ctx context.Context, subscription, group, run string, call func(context.Context, string, ...string) ([]byte, error)) error {
	b, err := call(ctx, "az", "resource", "list", "--subscription", subscription, "--resource-group", group, "--output", "json", "--only-show-errors")
	if err != nil {
		return err
	}
	ids, err := orphanIDs(b, subscription, group, run)
	if err != nil {
		return err
	}
	for _, id := range ids {
		b, err = call(ctx, "az", "resource", "show", "--ids", id, "--output", "json", "--only-show-errors")
		if err != nil {
			return err
		}
		current, e := orphanIDs(append(append([]byte("["), b...), ']'), subscription, group, run)
		if e != nil || len(current) != 1 || current[0] != id {
			return fmt.Errorf("orphan tag/scope changed: %s: %v", id, e)
		}
		if _, err = call(ctx, "az", "resource", "delete", "--ids", id, "--only-show-errors"); err != nil {
			return err
		}
		if _, err = call(ctx, "az", "resource", "wait", "--deleted", "--ids", id, "--interval", "2", "--timeout", "90", "--only-show-errors"); err != nil {
			return err
		}
	}
	return nil
}
