package seaweed

import (
	"encoding/json"
	"strings"
	"testing"
)

// A kv_load plan's verdict must carry kv_load's set and a soak's the soak's;
// neither PASSes with the other's.
func TestKVLoadVerdictCarriesItsOwnSet(t *testing.T) {
	kv := &SoakPlan{Pattern: &LoadPattern{}}
	if (&SoakPlan{}).criteria()[6] != "coverage" || kv.criteria()[6] != "write_once" {
		t.Fatal("criteria() does not follow the pattern")
	}
	kvRow := passRow(t, func(row map[string]any) {
		crit := map[string]any{}
		for _, c := range kvLoadCriteria {
			crit[c] = map[string]any{"outcome": "PASS", "reason": "ok"}
		}
		row["criteria"] = crit
	})
	if v, err := decodeVerdict(captured([]byte(kvRow)), testBinding, kv.criteria()); err != nil || v.Status != "PASS" {
		t.Fatalf("kv_load PASS row refused: %v", err)
	}
	if _, err := decodeVerdict(captured([]byte(kvRow)), testBinding, soakCriteria); err == nil {
		t.Fatal("a kv_load row passed as a soak verdict")
	}
	if _, err := decodeVerdict(captured([]byte(passRow(t, nil))), testBinding, kv.criteria()); err == nil {
		t.Fatal("a soak row passed as a kv_load verdict")
	}
}

// The assessor is told the pattern only for kv_load.
func TestSoakAssessArgsCarryThePatternOnlyForKVLoad(t *testing.T) {
	d := profileFixture(t)
	s := SmokeSettings{HelperDir: "/opt/profile/worker"}
	for _, x := range []*SoakPlan{{DurationS: 1800}, {DurationS: 1800, Pattern: lmcacheLike()}} {
		argv, err := SoakAssessArgs(d, s, x)
		var c map[string]json.RawMessage
		if err != nil || json.Unmarshal([]byte(argv[len(argv)-1]), &c) != nil {
			t.Fatalf("assess argv %q: %v", argv, err)
		}
		if _, has := c["pattern"]; has != (x.Pattern != nil) {
			t.Errorf("pattern %v in the assessor config: %v", x.Pattern != nil, has)
		}
	}
}

// M01 may run a volume server beside the client (kv_load's replication 001),
// and nothing else.
func TestM01RunsTheClientAndOptionallyAVolume(t *testing.T) {
	s := SmokeSettings{HelperDir: "/opt/profile/worker", NativeDir: "/opt/profile/native", Keys: 1, Length: 64}
	files := map[string]string{"/opt/profile/bin/weed": "a", "/opt/profile/bin/weed-volume": "b"}
	for roles, ok := range map[string]bool{"client": true, "client,volume": true, "client,master": false, "volume": false, "client,volume,filer": false} {
		d := profileFixture(t)
		d.Hosts[0].Roles = strings.Split(roles, ",")
		_, err := ProfileArgs(d, "m01", "preflight", "", s, files)
		if (err == nil) != ok || !ok && !strings.Contains(err.Error(), "M01 client only, or client and volume") {
			t.Errorf("M01 roles %s: %v", roles, err)
		}
	}
	d := profileFixture(t)
	d.Hosts[0].Roles = []string{"client", "volume"}
	if _, err := ProfileArgs(d, "m01", "deploy", "volume", s, files); err != nil {
		t.Fatalf("M01's volume deploy refused: %v", err)
	}
}
