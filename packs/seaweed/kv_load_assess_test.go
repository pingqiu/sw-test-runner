package seaweed

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pingqiu/sw-test-runner/inventory"
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
		_, pattern := c["pattern"]
		if _, faults := c["faults"]; pattern != (x.Pattern != nil) || faults != pattern {
			t.Errorf("pattern %v: the assessor config has pattern %v, faults %v", x.Pattern != nil, pattern, faults)
		}
	}
}

// M01 may run a volume server beside the client (kv_load's replication 001),
// and nothing else.
func TestM01RunsTheClientAndOptionallyAVolume(t *testing.T) {
	smoke := SmokeSettings{HelperDir: "/opt/profile/worker", NativeDir: "/opt/profile/native", Keys: 1, Length: 64}
	kv := Plan{Smoke: smoke, Soak: &SoakPlan{Pattern: lmcacheLike()}}.profileSettings(false)
	pins := map[string]string{"/opt/profile/bin/weed": strings.Repeat("a", 64), "/opt/profile/bin/weed-volume": strings.Repeat("b", 64)}
	for name, c := range map[string]struct {
		roles string
		s     SmokeSettings
		ok    bool
	}{"client": {"client", smoke, true}, "client and volume, kv_load": {"client,volume", kv, true}, "client and volume, soak": {"client,volume", smoke, false},
		"client and master, kv_load": {"client,master", kv, false}, "volume alone": {"volume", kv, false}} {
		d := profileFixture(t)
		d.Hosts[0].Roles = strings.Split(c.roles, ",")
		_, err := ProfileArgs(d, "m01", "preflight", "", c.s, pins)
		if (err == nil) != c.ok || !c.ok && !strings.Contains(err.Error(), "M01 client only, or client and volume for kv_load") {
			t.Errorf("%s: %v", name, err)
		}
	}
	d := profileFixture(t)
	d.Hosts[0].Roles = []string{"client", "volume"}
	if _, err := ProfileArgs(d, "m01", "deploy", "volume", kv, map[string]string{}); err == nil || !strings.Contains(err.Error(), "product pin") {
		t.Fatalf("M01's volume deployed without its product pins: %v", err)
	}
	args, err := ProfileArgs(d, "m01", "deploy", "volume", kv, pins)
	var c ProfileConfig
	if err != nil || json.Unmarshal([]byte(args[3]), &c) != nil || c.Products["weed-volume"] != pins["/opt/profile/bin/weed-volume"] {
		t.Fatalf("M01's volume deploy: products %v, %v", c.Products, err)
	}
}

// kv_load's M01 volume beside the client doesn't break the load's smoke
// config: its endpoints stay M02's, whichever host comes first; two volumes
// that aren't the client's are still refused.
func TestSmokeConfigTakesTheServerVolumeBesideTheClients(t *testing.T) {
	s := SmokeSettings{HelperDir: "/opt/profile/worker", NativeDir: "/opt/profile/native", Keys: 1, Length: 64}
	for _, reverse := range []bool{false, true} {
		d := profileFixture(t)
		d.Hosts[0].Roles = []string{"client", "volume"}
		if reverse {
			d.Hosts[0], d.Hosts[1] = d.Hosts[1], d.Hosts[0]
		}
		c, err := smokeConfig(d, "m01", s)
		var m02 inventory.Host
		for _, h := range d.Hosts {
			if h.ID == "m02" {
				m02 = h
			}
		}
		if err != nil || c.RDMAAddress != endpoint(m02, "rdma") {
			t.Fatalf("reverse=%v: %v, rdma %q, want M02's %q", reverse, err, c.RDMAAddress, endpoint(m02, "rdma"))
		}
	}
	d := profileFixture(t)
	third := d.Hosts[1]
	third.ID, third.Roles = "m03", []string{"volume"}
	d.Hosts = append(d.Hosts, third)
	if _, err := smokeConfig(d, "m01", s); err == nil || !strings.Contains(err.Error(), "single-volume") {
		t.Fatalf("two non-client volumes: %v", err)
	}
}
