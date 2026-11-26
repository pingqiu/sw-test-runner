package v3block

import (
	"strings"
	"testing"
)

// Every function that decides an outcome, one row per cell.
func TestParseV3ctl(t *testing.T) {
	id := strings.Repeat("ab", 16)
	for _, c := range []struct {
		name, out string
		code      int
		want      string
	}{
		{"ok", `{"outcome":"ok","volume":"v001","layout_id":"` + id + `","call_ns":5}`, 0, "ok"},
		{"refused", `{"outcome":"refused","volume":"v001","code":"AlreadyExists"}`, 1, "refused"},
		{"unknown", `{"outcome":"unknown","volume":"v001"}`, 3, "unknown"},
		{"ok answer, nonzero exit", `{"outcome":"ok","volume":"v001","layout_id":"` + id + `"}`, 1, "unknown"},
		{"refused answer, exit 0", `{"outcome":"refused","volume":"v001"}`, 0, "unknown"},
		{"ok without a layout id", `{"outcome":"ok","volume":"v001"}`, 0, "unknown"},
		{"another volume", `{"outcome":"ok","volume":"v002","layout_id":"` + id + `"}`, 0, "unknown"},
		{"no answer", ``, 0, "unknown"},
		{"no outcome", `{"volume":"v001"}`, 0, "unknown"},
		{"an outcome outside the three, exit 0", `{"outcome":"maybe","volume":"v001","layout_id":"` + id + `"}`, 0, "unknown"},
	} {
		if _, got := parseV3ctl(c.out, c.code, "v001"); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

func TestChunkOutcome(t *testing.T) {
	for _, c := range []struct {
		name, out string
		code      int
		want      string
	}{
		{"ok", `{"name":"s1"}`, 0, "ok"},
		// Refused only where the product states nothing was applied.
		{"detach aborted, retryable, applied nothing", `{"code":"detach_aborted","message":"x","retryable":true}`, 1, "refused"},
		{"usage, before the request", `{"code":"bad_request","message":"x","retryable":false}`, 2, "refused"},
		// The product's own unknown stays unknown whatever its flag says.
		{"outcome unknown, retryable", `{"code":"outcome_unknown","message":"x","retryable":true}`, 1, "unknown"},
		{"outcome unknown, not retryable", `{"code":"outcome_unknown","message":"x","retryable":false}`, 1, "unknown"},
		// No stated "not applied": unknown, a read of the state decides.
		{"non-retryable without a guarantee", `{"code":"layout_refused","message":"x","retryable":false}`, 1, "unknown"},
		{"retryable", `{"code":"remote_unavailable","message":"x","retryable":true}`, 1, "unknown"},
		{"no body", ``, 1, "unknown"},
		{"body without a code", `{"message":"x"}`, 1, "unknown"},
	} {
		if got, _ := chunkOutcome(c.out, c.code); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

func TestWsOutcome(t *testing.T) {
	want := map[int]string{0: "ok", 1: "refused", 2: "refused", 3: "unknown", 4: "unknown", 5: "refused", 9: "unknown", -1: "unknown"}
	for code, w := range want {
		if got := wsOutcome(code); got != w {
			t.Errorf("exit %d: %s, want %s", code, got, w)
		}
	}
}

func TestParseDetach(t *testing.T) {
	good := `{"volumeID":"v001","headGen":7,"headEpoch":2,"cutLSN":3,"lastWrittenLSN":3}`
	if g, err := parseDetach(good, "sealed HEAD generation: 7\n", "v001", 2); err != nil || g != 7 {
		t.Fatalf("good detach: %d %v", g, err)
	}
	for name, c := range map[string][2]string{
		"stderr names another generation": {good, "sealed HEAD generation: 8"},
		"stderr has more":                 {good, "sealed HEAD generation: 7\nwarning"},
		"another volume":                  {strings.Replace(good, "v001", "v002", 1), "sealed HEAD generation: 7"},
		"another epoch":                   {strings.Replace(good, `"headEpoch":2`, `"headEpoch":3`, 1), "sealed HEAD generation: 7"},
		"no generation":                   {strings.Replace(good, `"headGen":7`, `"headGen":0`, 1), "sealed HEAD generation: 0"},
		"not JSON":                        {"sealed", "sealed HEAD generation: 7"},
	} {
		if _, err := parseDetach(c[0], c[1], "v001", 2); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Every shell-special character is quoted; plain words stay bare.
func TestShellQuote(t *testing.T) {
	for _, s := range []string{"v001|r1|0|", "a;b", "a&b", "a>b", "a<b", "(a)", "a*", "a?", "[a]", "~a", "#a", "a!b", "a b", "it's", "$x", "`x`", "a\nb"} {
		q := shellQuote(s)
		if !strings.HasPrefix(q, "'") {
			t.Errorf("%q left bare as %q", s, q)
		}
	}
	for _, s := range []string{"v001", "127.0.0.1:20013", "/root/x-y_z", "nqn.2026-11.io.seaweedfs:vs-v001", "a=b,c@d%e+f"} {
		if q := shellQuote(s); q != s {
			t.Errorf("%q quoted as %q", s, q)
		}
	}
	if q := shellQuote("it's"); q != `'it'\''s'` {
		t.Errorf("embedded quote: %q", q)
	}
}

func TestServing(t *testing.T) {
	ok := vsStatus{VolumeID: "v001", Epoch: 2, Healthy: true, AuthorityRole: "primary", FrontendPrimaryReady: true}
	if !ok.serving("v001") {
		t.Fatal("a serving primary was not serving")
	}
	for name, st := range map[string]vsStatus{
		"other volume": {VolumeID: "v002", Epoch: 2, Healthy: true, AuthorityRole: "primary", FrontendPrimaryReady: true},
		"unhealthy":    {VolumeID: "v001", Epoch: 2, AuthorityRole: "primary", FrontendPrimaryReady: true},
		"not primary":  {VolumeID: "v001", Epoch: 2, Healthy: true, AuthorityRole: "replica", FrontendPrimaryReady: true},
		"no frontend":  {VolumeID: "v001", Epoch: 2, Healthy: true, AuthorityRole: "primary"},
		"epoch zero":   {VolumeID: "v001", Healthy: true, AuthorityRole: "primary", FrontendPrimaryReady: true},
	} {
		if st.serving("v001") {
			t.Errorf("%s: serving", name)
		}
	}
}

