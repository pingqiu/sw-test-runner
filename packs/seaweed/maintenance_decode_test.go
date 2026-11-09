package seaweed

import "testing"

func TestMaintenanceReplyCannotAliasOwnershipOrAppendVerdict(t *testing.T) {
	for _, raw := range []string{
		`{"before":1,"Before":2}`, `{"before":1,"before":2}`,
		`{"before":NaN}`, `{"before":1} {"before":2}`,
		`{"identity":{"PID":1}}`, `{"before":null}`,
		`null`, `{"before":1,"alive":null}`,
	} {
		var s maintenanceSample
		if maintenanceDecode([]byte(raw), &s) == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	var s maintenanceSample
	if err := maintenanceDecode([]byte(`{"before":1,"after":2,"identity":null,"alive":false,"boot":"b","result":null,"progress":null}`), &s); err != nil {
		t.Fatal(err)
	}
	// Product metadata is not rewritten/case-normalized by the evidence reader.
	var scope maintenanceScope
	if err := maintenanceDecode([]byte(`{"collection":"c","vid":"7","generation":0,"encode_ns":"1","shards":[],"files":[],"metadata":{"c_7.vif":{"ecShardConfig":{"encodeTsNs":"1"}}},"pending":0,"ordinary":false}`), &scope); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceLateCleanRefusalIsNeverRecovered(t *testing.T) {
	e := maintenanceExample()
	e.Terminal = "refused"
	e.Refusal = "RPC_UNAVAILABLE"
	v := assessMaintenance(e)
	if v.Task != "CLEAN_REFUSAL" || v.Coverage != "COVERED" || v.Refusal != "RPC_UNAVAILABLE" {
		t.Fatalf("%+v", v)
	}
	e.EffectsKnown = false
	v = assessMaintenance(e)
	if v.Task != "UNKNOWN" || v.Coverage != "NOT_COVERED" {
		t.Fatalf("partial refusal laundered: %+v", v)
	}
}
