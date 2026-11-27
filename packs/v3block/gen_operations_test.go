package v3block

import (
	"reflect"
	"strings"
	"testing"
)

// product/operations.json carries the values the generator had in code
// before they became data: a change to any of them changes every recorded
// run of the same seed, so it must come with a new generator version.
func TestOperationsTableKeepsTheGeneratorsValues(t *testing.T) {
	if genTable.Product != "block" || !strings.HasPrefix(genTable.ReadAt, "seaweed-block ") {
		t.Fatalf("product %q read at %q", genTable.Product, genTable.ReadAt)
	}
	weights := map[string]int{
		"v3_op_create": 6, "v3_op_attach": 10, "v3_op_write": 25, "v3_op_read": 20,
		"v3_op_snapshot": 8, "v3_op_snapshot_delete": 5, "v3_op_restore": 6,
		"v3_op_stop": 6, "v3_op_move": 6, "v3_op_delete": 8,
	}
	costs := map[string]float64{
		"v3_op_create": 2, "v3_op_attach": 8, "v3_op_write": 1, "v3_op_read": 1,
		"v3_op_snapshot": 2, "v3_op_snapshot_delete": 2, "v3_op_restore": 3,
		"v3_op_stop": 3, "v3_op_move": 20, "v3_op_delete": 65, "v3_op_observe": 1,
		"v3_op_release": 2,
	}
	if !reflect.DeepEqual(DefaultWeights, weights) {
		t.Errorf("weights %v", DefaultWeights)
	}
	if !reflect.DeepEqual(OpCost, costs) {
		t.Errorf("costs %v", OpCost)
	}
	if MaxSnapshots != 10 {
		t.Errorf("MaxSnapshots %d", MaxSnapshots)
	}
	// Each limit: its name as gen_step records it, and the action that probes it.
	limits := map[string][2]string{
		"create_deleted":            {"create a deleted name", "v3_op_create"},
		"delete_attached":           {"delete while attached", "v3_op_delete"},
		"delete_source":             {"delete a source whose snapshot has a restore target", "v3_op_delete"},
		"delete_snapshot_dependent": {"delete a snapshot that has a restore target", "v3_op_snapshot_delete"},
		"eleventh_snapshot":         {"an eleventh snapshot", "v3_op_snapshot"},
		"restore_existing":          {"restore into an existing name", "v3_op_restore"},
		"restore_from_target":       {"restore from a snapshot of a restore target", "v3_op_restore"},
		"release_live":              {"release a live restore target", "v3_op_release"},
	}
	if len(genTable.Limits) != len(limits) {
		t.Errorf("%d limits, want %d", len(genTable.Limits), len(limits))
	}
	for _, l := range genTable.Limits {
		if w, ok := limits[l.ID]; !ok || w != [2]string{l.Name, l.Action} {
			t.Errorf("limit %+v", l)
		}
		for _, c := range strings.Split(l.Cite, ", ") {
			if !strings.HasPrefix(c, "docs/") || !strings.Contains(c, ".md:") {
				t.Errorf("limit %s cites %q, not a page line", l.ID, c)
			}
		}
	}
}

// One vocabulary: each operation's model op id is the one its record carries.
func TestOperationsTableOpIDsAreTheRecordedOnes(t *testing.T) {
	for _, o := range genTable.Operations {
		verb := strings.TrimPrefix(o.Action, "v3_op_")
		if o.Op != opIDs[verb] {
			t.Errorf("%s: op %q, its record carries %q", o.Action, o.Op, opIDs[verb])
		}
		if genHandlers[o.Action] == nil {
			t.Errorf("%s has no handler", o.Action)
		}
	}
}
