package seaweed

import (
	"encoding/json"
	"strings"
	"testing"
)

// #999: the soak's read_transport decodes only "dc" or "rc"; absent is DC.
// null, "" and any other value refuse, on the struct and through the plan.
func TestSoakReadTransportDecodesStrictly(t *testing.T) {
	for raw, want := range map[string]SoakTransport{`{}`: "", `{"read_transport":"dc"}`: "dc", `{"read_transport":"rc"}`: "rc"} {
		var p SoakPlan
		if err := json.Unmarshal([]byte(raw), &p); err != nil || p.ReadTransport != want {
			t.Errorf("%s decoded as %q: %v", raw, p.ReadTransport, err)
		}
	}
	for _, bad := range []string{`null`, `""`, `"RC"`, `"http"`, `"dc "`, `1`, `["rc"]`} {
		var p SoakPlan
		if err := json.Unmarshal([]byte(`{"read_transport":`+bad+`}`), &p); err == nil {
			t.Errorf("read_transport %s decoded as %q", bad, p.ReadTransport)
		}
		if _, _, err := decodePlan(`{"soak":{"read_transport":` + bad + `}}`); err == nil {
			t.Errorf("a plan with read_transport %s decoded", bad)
		}
	}
}

// checkSchedule bounds a read_transport set past the decoder, and keeps "rc"
// to the soak: kv_load carries its own pattern transport.
func TestCheckScheduleBoundsTheReadTransport(t *testing.T) {
	p, _ := soakTestPlan()
	for _, rt := range []SoakTransport{"", "dc", "rc"} {
		if p.Soak.ReadTransport = rt; p.Soak.checkSchedule() != nil {
			t.Errorf("read_transport %q refused: %v", rt, p.Soak.checkSchedule())
		}
	}
	for _, rt := range []SoakTransport{"RC", "http", "dc "} {
		if p.Soak.ReadTransport = rt; p.Soak.checkSchedule() == nil {
			t.Errorf("read_transport %q passed", rt)
		}
	}
	p.Soak.Pattern = lmcacheLike()
	if p.Soak.ReadTransport = "dc"; p.Soak.checkSchedule() != nil {
		t.Fatalf("kv_load with read_transport dc refused: %v", p.Soak.checkSchedule())
	}
	if p.Soak.ReadTransport = "rc"; p.Soak.checkSchedule() == nil || !strings.Contains(p.Soak.checkSchedule().Error(), "read_transport") {
		t.Fatalf("kv_load with read_transport rc passed: %v", p.Soak.checkSchedule())
	}
}

// The load and the assessor are told "rc" only for an RC soak; a DC soak's
// argv (absent or "dc") is unchanged.
func TestSoakArgsCarryTheRCRouteOnlyForAnRCSoak(t *testing.T) {
	d := profileFixture(t)
	s := SmokeSettings{HelperDir: "/opt/profile/worker", NativeDir: "/opt/profile/native", Keys: 1, Length: 64}
	base := map[string]string{}
	for _, rt := range []SoakTransport{"", "dc", "rc"} {
		x := &SoakPlan{DurationS: 1800, ReadTransport: rt}
		for name, args := range map[string]func() ([]string, error){
			"load":   func() ([]string, error) { return SoakLoadArgs(d, s, x) },
			"assess": func() ([]string, error) { return SoakAssessArgs(d, s, x) },
		} {
			argv, err := args()
			var c map[string]json.RawMessage
			if err != nil || json.Unmarshal([]byte(argv[len(argv)-1]), &c) != nil {
				t.Fatalf("%s %q argv %q: %v", name, rt, argv, err)
			}
			got, present := c["read_transport"]
			if want := rt == "rc"; present != want || present && string(got) != `"rc"` {
				t.Errorf("%s %q: read_transport %s (present %v)", name, rt, got, present)
			}
			if rt == "" {
				base[name] = argv[len(argv)-1]
			} else if rt == "dc" && argv[len(argv)-1] != base[name] {
				t.Errorf("%s: an explicit dc changed the argv", name)
			}
		}
	}
}
