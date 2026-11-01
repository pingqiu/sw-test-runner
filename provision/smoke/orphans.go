package smoke

import (
	"context"
	"encoding/json"
	"errors"
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
	ranks := map[string]int{}
	prefix := "/subscriptions/" + subscription + "/resourceGroups/" + group + "/providers/"
	for _, r := range rows {
		if r.Tags["seaweed_run"] != run {
			continue
		}
		parts := strings.Split(r.ID, "/")
		rank, ok := sweepRank(r.Type) // moduleTypes: a VM first, before its NIC (run 7e)
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

// Contract: the dedicated test RG belongs exclusively to the runner Azure lane;
// the manager serializes run tokens, and no other writer retags its resources.
// The fresh tag check below is a sanity check, not atomic ownership protection.
// It returns the IDs it confirmed gone (absent on re-read, or deleted and
// waited), which the final list may still show for a moment.
func sweepOrphans(ctx context.Context, subscription, group, run string, call func(context.Context, string, ...string) ([]byte, error)) ([]string, error) {
	b, err := call(ctx, "az", "resource", "list", "--subscription", subscription, "--resource-group", group, "--output", "json", "--only-show-errors")
	if err != nil {
		return nil, err
	}
	ids, err := orphanIDs(b, subscription, group, run)
	if err != nil {
		return nil, err
	}
	gone := []string{}
	for _, id := range ids {
		// An orphan already gone (exact ResourceNotFound) is deleted, not a failure.
		present, b, err := reread(ctx, call, id)
		if err != nil {
			return gone, err
		}
		if !present {
			gone = append(gone, id)
			continue
		}
		current, e := orphanIDs(append(append([]byte("["), b...), ']'), subscription, group, run)
		if e != nil || len(current) != 1 || current[0] != id {
			return gone, fmt.Errorf("orphan tag/scope changed: %s: %v", id, e)
		}
		if present, err = deleteOrphan(ctx, call, id); err != nil {
			return gone, err
		}
		if present {
			if _, err = call(ctx, "az", "resource", "wait", "--deleted", "--ids", id, "--interval", "2", "--timeout", "90", "--only-show-errors"); err != nil {
				return gone, err
			}
		}
		gone = append(gone, id)
	}
	return gone, nil
}

// deleteOrphan deletes one ID. A lost response or reset re-reads it: absent is
// deleted, present is deleted again, at most two more times.
func deleteOrphan(ctx context.Context, call func(context.Context, string, ...string) ([]byte, error), id string) (bool, error) {
	for attempt := 0; ; attempt++ {
		b, err := call(ctx, "az", "resource", "delete", "--ids", id, "--only-show-errors")
		if err == nil {
			return true, nil
		}
		diagnostics, failed := b, (*toolError)(nil)
		if errors.As(err, &failed) {
			diagnostics = failed.stderr
		}
		if attempt == 2 || !transportOnly(diagnostics) {
			return true, err
		}
		present, _, e := reread(ctx, call, id)
		if e != nil || !present {
			return present, errors.Join(e)
		}
	}
}
