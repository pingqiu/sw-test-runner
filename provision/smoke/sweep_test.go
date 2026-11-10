package smoke

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

const sweepSub, sweepRG = "11111111-1111-1111-1111-111111111111", "dedicated-test"

// fakeAzure is the resource group: list returns what is there, show one row, delete removes it.
type fakeAzure struct {
	rows      map[string]map[string]any
	calls     []string
	listErr   error
	keepAfter bool // delete answers success but the resource stays (the verify list must catch it)
	listRaw   []byte
}

func (f *fakeAzure) call(ctx context.Context, tool string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, tool+" "+strings.Join(args, " "))
	if p := os.Getenv("AZURE_ARGV_CONTROL_OUTPUT"); p != "" && tool == "az" { // the sweep's argv for the wrapper control
		b, _ := json.Marshal(args)
		if out, e := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); e == nil {
			out.Write(append(b, '\n'))
			out.Close()
		}
	}
	switch args[1] {
	case "list":
		if f.listErr != nil {
			return nil, f.listErr
		}
		if f.listRaw != nil {
			return f.listRaw, nil
		}
		rows := []map[string]any{}
		for _, r := range f.rows {
			rows = append(rows, r)
		}
		return json.Marshal(rows)
	case "show":
		return json.Marshal(f.rows[args[3]])
	case "delete":
		if !f.keepAfter {
			delete(f.rows, args[3])
		}
	}
	return []byte("{}"), nil
}

func (f *fakeAzure) deletes() []string {
	out := []string{}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "az resource delete") {
			out = append(out, strings.TrimSuffix(strings.TrimPrefix(c, "az resource delete --ids "), " --only-show-errors"))
		}
	}
	return out
}

// runRows adds a run's VM, NIC and NSG with the given tags (nil tag value = absent).
func runRows(f *fakeAzure, run, managedBy, expires string) []string {
	base := "/subscriptions/" + sweepSub + "/resourceGroups/" + sweepRG + "/providers/"
	ids := []string{}
	for _, typ := range []string{"Microsoft.Compute/virtualMachines", "Microsoft.Network/networkInterfaces", "Microsoft.Network/networkSecurityGroups"} {
		id := base + typ + "/" + run + "-x"
		tags := map[string]string{"seaweed_run": run}
		if managedBy != "" {
			tags["managed_by"] = managedBy
		}
		if expires != "" {
			tags["expires_at"] = expires
		}
		f.rows[id] = map[string]any{"id": id, "type": typ, "tags": tags}
		ids = append(ids, id)
	}
	return ids
}

var sweepNow = time.Date(2026, 11, 10, 12, 30, 0, 0, time.UTC)

func noneLive(string) bool { return false }

func byRunOf(r SweepReceipt) map[string]SweptRun {
	m := map[string]SweptRun{}
	for _, s := range r.Runs {
		m[s.Run] = s
	}
	return m
}

func TestSweepDeletesOnlyExpiredRunnerRunsWithoutAController(t *testing.T) {
	f := &fakeAzure{rows: map[string]map[string]any{}}
	expired := runRows(f, "expired", "sw-test-runner", "2026-11-10T10:25:00Z")
	runRows(f, "future", "sw-test-runner", "2026-11-10T13:00:00Z")
	runRows(f, "alive", "sw-test-runner", "2026-11-10T10:25:00Z")
	runRows(f, "foreign", "someone-else", "2026-11-10T10:25:00Z")
	runRows(f, "no-expiry", "sw-test-runner", "")
	runRows(f, "bad-expiry", "sw-test-runner", "tomorrow")
	other := "/subscriptions/" + sweepSub + "/resourceGroups/" + sweepRG + "/providers/Microsoft.Compute/disks/not-ours"
	f.rows[other] = map[string]any{"id": other, "type": "Microsoft.Compute/disks", "tags": map[string]string{}}
	r := Sweep(context.Background(), sweepSub, sweepRG, sweepNow, false, func(run string) bool { return run == "alive" }, f.call)
	if r.Error != "" {
		t.Fatal(r.Error)
	}
	got := byRunOf(r)
	if s := got["expired"]; s.Action != "deleted" || len(s.Gone) != 3 || len(s.Left) != 0 {
		t.Fatalf("expired: %+v", s)
	}
	for run, reason := range map[string]string{"future": "not expired", "alive": "controller live", "foreign": "not managed_by", "no-expiry": "expires_at missing", "bad-expiry": "expires_at missing"} {
		if s := got[run]; s.Action != "left" || !strings.Contains(s.Reason, reason) || len(s.Left) != 3 {
			t.Fatalf("%s: %+v", run, s)
		}
	}
	if len(r.Untagged) != 1 || r.Untagged[0] != other {
		t.Fatalf("untagged: %v", r.Untagged)
	}
	d := f.deletes()
	if len(d) != 3 || d[0] != expired[0] { // the VM first, before the NIC it holds
		t.Fatalf("deletes: %v", d)
	}
	for _, id := range d {
		if !strings.Contains(id, "/expired-x") {
			t.Fatalf("deleted something not expired: %s", id)
		}
	}
	if !r.Acted() {
		t.Fatal("a sweep that found the runner's resources must be reported")
	}
}

func TestSweepDryRunDeletesNothing(t *testing.T) {
	f := &fakeAzure{rows: map[string]map[string]any{}}
	runRows(f, "expired", "sw-test-runner", "2026-11-10T10:25:00Z")
	r := Sweep(context.Background(), sweepSub, sweepRG, sweepNow, true, noneLive, f.call)
	if s := byRunOf(r)["expired"]; s.Action != "would-delete" || len(s.Left) != 3 || !r.DryRun {
		t.Fatalf("%+v", r)
	}
	if len(f.deletes()) != 0 || len(f.calls) != 1 {
		t.Fatalf("a dry run made calls beyond one list: %v", f.calls)
	}
}

func TestSweepVerifiesWithAFreshList(t *testing.T) {
	f := &fakeAzure{rows: map[string]map[string]any{}, keepAfter: true}
	runRows(f, "expired", "sw-test-runner", "2026-11-10T10:25:00Z")
	r := Sweep(context.Background(), sweepSub, sweepRG, sweepNow, false, noneLive, f.call)
	if s := byRunOf(r)["expired"]; s.Action != "left" || !strings.Contains(s.Reason, "still listed") || len(s.Left) != 3 {
		t.Fatalf("a delete the list does not confirm was reported gone: %+v", s)
	}
}

func TestSweepMixedExpiryIsLeft(t *testing.T) {
	f := &fakeAzure{rows: map[string]map[string]any{}}
	ids := runRows(f, "mixed", "sw-test-runner", "2026-11-10T10:25:00Z")
	f.rows[ids[1]]["tags"].(map[string]string)["expires_at"] = "2026-11-11T00:00:00Z"
	r := Sweep(context.Background(), sweepSub, sweepRG, sweepNow, false, noneLive, f.call)
	if s := byRunOf(r)["mixed"]; s.Action != "left" || !strings.Contains(s.Reason, "disagree") || len(f.deletes()) != 0 {
		t.Fatalf("%+v %v", s, f.deletes())
	}
}

func TestSweepAListThatCannotBeTrustedDeletesNothing(t *testing.T) {
	for name, f := range map[string]*fakeAzure{
		"list error":     {listErr: errors.New("az: login required")},
		"duplicate keys": {listRaw: []byte(`[{"id":"/subscriptions/x","id":"/y","type":"t","tags":{"seaweed_run":"r"}}]`)},
		"not an array":   {listRaw: []byte(`{"value":[]}`)},
	} {
		t.Run(name, func(t *testing.T) {
			r := Sweep(context.Background(), sweepSub, sweepRG, sweepNow, false, noneLive, f.call)
			if r.Error == "" || len(f.deletes()) != 0 || !r.Acted() {
				t.Fatalf("%+v %v", r, f.calls)
			}
		})
	}
}

func TestSweepOfAnEmptyGroupIsQuiet(t *testing.T) {
	f := &fakeAzure{rows: map[string]map[string]any{}}
	r := Sweep(context.Background(), sweepSub, sweepRG, sweepNow, false, noneLive, f.call)
	if r.Error != "" || len(r.Runs) != 0 || r.Acted() || r.Schema != sweepSchema {
		t.Fatalf("%+v", r)
	}
}
