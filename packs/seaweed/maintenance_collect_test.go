package seaweed

import (
	"encoding/json"
	"testing"
)

func maintenanceSampleFixture(t *testing.T, at float64) maintenanceSample {
	t.Helper()
	var s maintenanceSample
	raw := `{"before":20,"after":21,"identity":{"pid":12,"start":"100","boot":"boot","task":"volume.balance","run_id":"run","config_sha256":"hash"},"alive":true,"boot":"boot","result":null,"progress":{"state":"in_flight","vid":"7","operation":"1","bytes":1024,"begin":10,"source":"source","target":"target"}}`
	if err := maintenanceDecode([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	s.Before = at
	s.After = at + 1
	s.Progress.Bytes = int64(at * 100)
	return s
}
func TestMaintenanceSamplesJoinOnlyOneOwnedCopyAcrossGone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*maintenanceFaultSample)
		valid  bool
	}{
		{"same active copy", func(*maintenanceFaultSample) {}, true},
		{"not alive", func(f *maintenanceFaultSample) { f.After.Alive = false }, false},
		{"queued", func(f *maintenanceFaultSample) { f.After.Progress.State = "queued" }, false},
		{"another copy of same VID", func(f *maintenanceFaultSample) { f.After.Progress.Operation = "2" }, false},
		{"foreign VID", func(f *maintenanceFaultSample) { f.After.Progress.VID = "8" }, false},
		{"different start", func(f *maintenanceFaultSample) { f.After.Progress.Begin = 31 }, false},
		{"foreign boot", func(f *maintenanceFaultSample) { f.After.Boot = "victim-boot" }, false},
		{"PID reused", func(f *maintenanceFaultSample) { f.After.Identity.Start = "101" }, false},
		{"no work", func(f *maintenanceFaultSample) { f.Before.Progress.Bytes = 0 }, false},
		{"counter reset", func(f *maintenanceFaultSample) { f.After.Progress.Bytes = 1 }, false},
		{"no further byte progress", func(f *maintenanceFaultSample) { f.After.Progress.Bytes = f.Before.Progress.Bytes }, false},
		{"uncertain order", func(f *maintenanceFaultSample) { f.Before.After = 32 }, false},
		{"foreign process endpoint", func(f *maintenanceFaultSample) { f.TargetEndpoint = "unrelated" }, false},
		{"foreign unit generation", func(f *maintenanceFaultSample) { f.TargetUnit = "sw-cloud-other-vol1-volume" }, false},
		{"sample preceded GONE", func(f *maintenanceFaultSample) { f.AfterFollowsGone = false }, false},
		{"no GONE receipt", func(f *maintenanceFaultSample) { f.GoneWitness = "OLD 11 10" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := maintenanceSampleFixture(t, 20), maintenanceSampleFixture(t, 30)
			f := maintenanceFaultSample{Before: a, After: b, Subject: maintenanceSubject{VID: "7", Collection: "own"}, Confirmed: true, Role: "volume", Victim: "vol1", TargetEndpoint: "source", TargetUnit: "sw-cloud-run-vol1-volume", GoneWitness: "OLD 11 10 GONE", BeforePrecedesDispatch: true, AfterFollowsGone: true}
			result := maintenanceTaskResult{Job: "job", Attempt: "1", Identity: *a.Identity, Started: 5, Ended: 50}
			tc.change(&f)
			w, fault, valid := maintenanceReplicaFlight("volume.balance", f.Subject, result, &f)
			if valid != tc.valid {
				raw, _ := json.Marshal(f)
				t.Fatalf("valid=%t expected=%t %s", valid, tc.valid, raw)
			}
			if valid && (w.Begin.Upper > fault.Before.Lower || w.End.Lower < fault.Gone.Upper || fault.Before.Upper > fault.Gone.Lower) {
				t.Fatalf("non-conservative join %+v %+v", w, fault)
			}
		})
	}
}

func TestMaintenanceReplicaReadbackCannotHidePartialCopy(t *testing.T) {
	makeScopes := func() map[string]maintenanceScope {
		m := map[string]maintenanceScope{}
		for _, id := range []string{"vol1", "vol2", "vol3", "vol4", "vol5", "vol6"} {
			m[id] = maintenanceScope{}
		}
		m["vol1"] = maintenanceScope{Ordinary: true, Mounted: true}
		m["vol2"] = maintenanceScope{Ordinary: true, Mounted: true}
		return m
	}
	for _, tc := range []struct {
		name                 string
		change               func(map[string]maintenanceScope)
		known, owners, moved bool
	}{
		{"no-op", func(map[string]maintenanceScope) {}, true, true, false},
		{"moved", func(s map[string]maintenanceScope) {
			s["vol2"] = maintenanceScope{}
			s["vol3"] = maintenanceScope{Ordinary: true, Mounted: true}
		}, true, true, true},
		{"unmounted partial", func(s map[string]maintenanceScope) { s["vol3"] = maintenanceScope{Ordinary: true} }, false, false, true},
		{"missing host", func(s map[string]maintenanceScope) { delete(s, "vol6") }, false, true, false},
		{"readonly partial", func(s map[string]maintenanceScope) { x := s["vol2"]; x.ReadOnly = true; s["vol2"] = x }, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := makeScopes(), makeScopes()
			tc.change(b)
			known, owners, moved, _ := maintenanceReplicaPost(a, b, "volume.balance")
			if known != tc.known || owners != tc.owners || moved != tc.moved {
				t.Fatalf("%t/%t/%t", known, owners, moved)
			}
		})
	}
}
