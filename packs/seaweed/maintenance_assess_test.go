package seaweed

import (
	"math"
	"strings"
	"testing"
)

func maintenanceExample() maintenanceEvidence {
	s := maintenanceSubject{Collection: "run-owned", VID: "7", EncodeNS: "123", Generation: 2, Shards: []int{0, 1, 2}}
	d := maintenanceData{Key: "key", ExpectedSHA256: strings.Repeat("a", 64), ActualSHA256: strings.Repeat("a", 64), ExpectedLength: 3, ActualLength: 3, Observed: true, Status: 200}
	return maintenanceEvidence{RequiredFaults: 1, Task: "ec_repair", Expected: s, Job: "job", Attempt: "attempt1", Owner: "meta/pid/start", Boot: "boot", Terminal: "succeeded", EffectsKnown: true, PostOwners: true, GenerationConverged: true, Data: []maintenanceData{d}, ExpectedData: []maintenanceData{d}, Flights: []maintenanceFlight{{Subject: s, Job: "job", Attempt: "attempt1", Owner: "meta/pid/start", Boot: "boot", Operation: "copy/source-shard-0", State: "in_flight", Bytes: 1024, Begin: maintenanceBound{10, 11}, End: maintenanceBound{40, 41}}}, Faults: []maintenanceFault{{Subject: s, Owner: "meta/pid/start", Boot: "boot", Before: maintenanceBound{20, 21}, Gone: maintenanceBound{30, 31}, Missing: []int{0, 1}, Confirmed: true}}}
}

func TestMaintenanceEveryScheduledFaultMustBeCovered(t *testing.T) {
	for _, covered := range []int{0, 1, 2} {
		e := maintenanceExample()
		e.RequiredFaults = 2
		e.Faults = append(e.Faults, e.Faults[0])
		for i := covered; i < len(e.Faults); i++ {
			e.Faults[i].Confirmed = false
		}
		v := assessMaintenance(e)
		if v.CoveredFaults != covered || v.TotalFaults != 2 || (v.Coverage == "COVERED") != (covered == 2) || (v.Task == "RECOVERED") != (covered == 2) {
			t.Fatalf("%d of 2 required faults: %+v", covered, v)
		}
	}
	for _, observed := range []int{0, 1} {
		e := maintenanceExample()
		e.RequiredFaults = 2
		e.Faults = e.Faults[:observed]
		if v := assessMaintenance(e); v.Coverage == "COVERED" || v.Task == "RECOVERED" {
			t.Fatalf("missing required fault receipt: %+v", v)
		}
	}
}

func TestMaintenanceCoverageRequiresOwnActualWorkAndConservativeFaultBounds(t *testing.T) {
	for _, x := range []struct {
		name    string
		change  func(*maintenanceEvidence)
		covered bool
	}{
		{"real work spans fault", func(*maintenanceEvidence) {}, true},
		{"queued", func(e *maintenanceEvidence) { e.Flights[0].State = "queued" }, false},
		{"no byte work", func(e *maintenanceEvidence) { e.Flights[0].Bytes = 0 }, false},
		{"no operation", func(e *maintenanceEvidence) { e.Flights[0].Operation = "" }, false},
		{"foreign VID", func(e *maintenanceEvidence) { e.Flights[0].Subject.VID = "8" }, false},
		{"foreign collection", func(e *maintenanceEvidence) { e.Flights[0].Subject.Collection = "foreign" }, false},
		{"old encode", func(e *maintenanceEvidence) { e.Flights[0].Subject.EncodeNS = "122" }, false},
		{"old generation", func(e *maintenanceEvidence) { e.Flights[0].Subject.Generation = 1 }, false},
		{"foreign shard", func(e *maintenanceEvidence) { e.Faults[0].Subject.Shards = []int{9} }, false},
		{"foreign job", func(e *maintenanceEvidence) { e.Flights[0].Job = "foreign" }, false},
		{"retried attempt", func(e *maintenanceEvidence) { e.Flights[0].Attempt = "attempt2" }, false},
		{"raw victim monotonic", func(e *maintenanceEvidence) { e.Faults[0].Owner = "victim/pid/start" }, false},
		{"different boot", func(e *maintenanceEvidence) { e.Faults[0].Boot = "new-boot" }, false},
		{"unconfirmed GONE", func(e *maintenanceEvidence) { e.Faults[0].Confirmed = false }, false},
		{"begins inside uncertain fault bracket", func(e *maintenanceEvidence) { e.Flights[0].Begin = maintenanceBound{19, 20.1} }, false},
		{"ends inside uncertain GONE bracket", func(e *maintenanceEvidence) { e.Flights[0].End = maintenanceBound{30.9, 32} }, false},
		{"NaN", func(e *maintenanceEvidence) { e.Faults[0].Before.Lower = math.NaN() }, false},
		{"five distinct shards", func(e *maintenanceEvidence) { e.Faults[0].Missing = []int{0, 1, 2, 3, 4} }, false},
		{"duplicate missing shard not two losses", func(e *maintenanceEvidence) { e.Faults[0].Missing = []int{0, 0, 0, 0, 0} }, true},
	} {
		t.Run(x.name, func(t *testing.T) {
			e := maintenanceExample()
			x.change(&e)
			v := assessMaintenance(e)
			if (v.Coverage == "COVERED") != x.covered || (v.Task == "RECOVERED") != x.covered {
				t.Fatalf("%+v", v)
			}
		})
	}
}

func TestMaintenanceRefusalCannotExcuseDataOrPartialEffects(t *testing.T) {
	for _, x := range []struct {
		name       string
		change     func(*maintenanceEvidence)
		data, task string
	}{
		{"early refusal separate", func(e *maintenanceEvidence) { e.Terminal = "refused"; e.Refusal = "RPC_UNAVAILABLE"; e.Flights = nil }, "PASS", "CLEAN_REFUSAL"},
		{"loss on refusal", func(e *maintenanceEvidence) {
			e.Terminal = "refused"
			e.Refusal = "RPC_UNAVAILABLE"
			e.Data[0].Status = 404
		}, "FAIL", "FAIL"},
		{"wrong bytes", func(e *maintenanceEvidence) { e.Data[0].ActualSHA256 = strings.Repeat("b", 64) }, "FAIL", "FAIL"},
		{"cannot relabel expected hash", func(e *maintenanceEvidence) {
			e.Data[0].ExpectedSHA256 = strings.Repeat("b", 64)
			e.Data[0].ActualSHA256 = strings.Repeat("b", 64)
		}, "FAIL", "FAIL"},
		{"deleted resurrected", func(e *maintenanceEvidence) { e.ExpectedData[0].Deleted = true }, "FAIL", "FAIL"},
		{"deleted absent", func(e *maintenanceEvidence) { e.ExpectedData[0].Deleted = true; e.Data[0].Status = 404 }, "PASS", "RECOVERED"},
		{"omitted ACK", func(e *maintenanceEvidence) { e.Data = nil }, "UNKNOWN", "UNKNOWN"},
		{"duplicate read", func(e *maintenanceEvidence) { e.Data = append(e.Data, e.Data[0]) }, "UNKNOWN", "UNKNOWN"},
		{"unknown read", func(e *maintenanceEvidence) { e.Data[0].Observed = false }, "UNKNOWN", "UNKNOWN"},
		{"loss outranks earlier unknown", func(e *maintenanceEvidence) {
			e.ExpectedData = append(e.ExpectedData, e.ExpectedData[0])
			e.ExpectedData[1].Key = "second"
			e.Data = append(e.Data, e.Data[0])
			e.Data[0].Observed = false
			e.Data[1].Key = "second"
			e.Data[1].Status = 404
		}, "FAIL", "FAIL"},
		{"pending switch", func(e *maintenanceEvidence) { e.Pending = 1 }, "PASS", "UNKNOWN"},
		{"unknown partial effect", func(e *maintenanceEvidence) { e.EffectsKnown = false }, "PASS", "UNKNOWN"},
		{"owners unreadable", func(e *maintenanceEvidence) { e.PostOwners = false }, "PASS", "UNKNOWN"},
		{"mixed generation", func(e *maintenanceEvidence) { e.GenerationConverged = false }, "PASS", "UNKNOWN"},
	} {
		t.Run(x.name, func(t *testing.T) {
			e := maintenanceExample()
			x.change(&e)
			v := assessMaintenance(e)
			if v.Data != x.data || v.Task != x.task {
				t.Fatalf("want %s/%s got %+v", x.data, x.task, v)
			}
			if v.Task == "CLEAN_REFUSAL" && v.Coverage != "NOT_COVERED" {
				t.Fatal("refusal counted as work")
			}
		})
	}
}
