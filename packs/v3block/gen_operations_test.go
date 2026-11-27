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
	// The documented refusals the generator does not propose, listed.
	notProposed := map[string][2]string{
		"attach_moved_old_host": {"attach on the old host after a move", "v3_op_attach"},
		"attach_deleted":        {"attach a deleted volume", "v3_op_attach"},
		"attach_stale_root":     {"attach on a stale local root", "v3_op_attach"},
		"attach_geometry":       {"attach with other geometry", "v3_op_attach"},
		"attach_after_stop":     {"attach after a stop", "v3_op_attach"},
		"restore_deleted_name":  {"restore into a deleted name", "v3_op_restore"},
	}
	if len(genTable.Limits) != len(limits)+len(notProposed) {
		t.Errorf("%d limits, want %d", len(genTable.Limits), len(limits)+len(notProposed))
	}
	for _, l := range genTable.Limits {
		want, proposed := limits[l.ID]
		if !proposed {
			want = notProposed[l.ID]
		}
		if want != [2]string{l.Name, l.Action} || l.IsProposed() != proposed || (!proposed && l.Why == "") {
			t.Errorf("limit %+v", l)
		}
		// Re-read at the pin: verified_at is read_at's commit, abbreviated.
		if pin := strings.TrimPrefix(genTable.ReadAt, "seaweed-block "); len(pin) < 7 || l.VerifiedAt != pin[:7] {
			t.Errorf("limit %s: cite verified at %q, not at the pin %s", l.ID, l.VerifiedAt, genTable.ReadAt)
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

// A limit the table lists as not proposed is never probed: genLimit refuses
// it, and no run proposes a negative under its name.
func TestNotProposedLimitsAreNeverProbed(t *testing.T) {
	func() {
		defer func() {
			if recover() == nil {
				t.Error("genLimit accepted a limit listed as not proposed")
			}
		}()
		genLimit("attach_after_stop")
	}()
	names := map[string]bool{}
	for _, l := range genTable.Limits {
		if !l.IsProposed() {
			names[l.Name] = true
		}
	}
	for seed := uint64(1); seed <= 4; seed++ {
		ops, _ := run(t, GenConfig{Seed: seed, Ops: 2000, Pool: pool(60), Blocks: 8, Negative: 0.3})
		for _, o := range ops {
			if names[o.Negative] || (o.Negative != "" && o.Action == "v3_op_attach") {
				t.Fatalf("seed %d: %s proposed as a negative (%q)", seed, o.Action, o.Negative)
			}
		}
	}
}
