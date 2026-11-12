package gomemory

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

type replayGroup struct {
	ID       Identity `json:"identity"`
	Baseline *float64 `json:"baseline"`
	FD0      *float64 `json:"fd0"`
	FDMax    *float64 `json:"fdmax"`
	Live     bool     `json:"live"`
	Evidence Evidence `json:"evidence"`
	Expected Result   `json:"expected"`
}

func fixture(t *testing.T, run string) []replayGroup {
	t.Helper()
	b, e := os.ReadFile("testdata/" + run + ".json")
	if e != nil {
		t.Fatal(e)
	}
	var r struct{ Groups []replayGroup }
	if e = json.Unmarshal(b, &r); e != nil {
		t.Fatal(e)
	}
	return r.Groups
}
func replay(g replayGroup) Result { return Group(g.ID, g.Baseline, g.FD0, g.FDMax, g.Evidence, g.Live) }
func TestFrozenGoMemoryRows(t *testing.T) {
	for _, run := range []string{"132324", "095251"} {
		t.Run(run, func(t *testing.T) {
			gs := fixture(t, run)
			if len(gs) != 5 {
				t.Fatal("lost generation", len(gs))
			}
			for _, g := range gs {
				got := replay(g)
				want := g.Expected
				if got.Status != want.Status {
					t.Fatalf("%+v: %+v want %+v", g.ID, got, want)
				}
				if want.Status == "PASS" && (got.RSSOK != want.RSSOK || got.HeapOK != want.HeapOK || got.FDsOK != want.FDsOK || got.RSSBaseline != want.RSSBaseline || got.RSSFinal != want.RSSFinal || got.InuseStart != want.InuseStart || got.InuseEnd != want.InuseEnd || got.InuseGrowth != want.InuseGrowth) {
					t.Fatalf("%+v: metrics %+v want %+v", g.ID, got, want)
				}
			}
		})
	}
}
func liveFiler(t *testing.T) replayGroup {
	t.Helper()
	for _, g := range fixture(t, "132324") {
		if g.Live && g.ID.Role == "filer" {
			return g
		}
	}
	t.Fatal("missing live filer")
	return replayGroup{}
}
func f(v float64) *float64 { return &v }
func TestEvidenceCannotBeSubstituted(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*replayGroup)
	}{
		{"foreign pid", func(g *replayGroup) { g.Evidence.End.PID++ }},
		{"foreign start", func(g *replayGroup) { g.Evidence.Warm.Start++ }},
		{"foreign file", func(g *replayGroup) { g.Evidence.Warm.File = "other.heap" }},
		{"hash mismatch", func(g *replayGroup) { g.Evidence.End.Raw = heap(1) }},
		{"unowned listener", func(g *replayGroup) { g.Evidence.End.ListenerOwned = false }},
		{"missing baseline", func(g *replayGroup) { g.Evidence.Warm.RSS = nil }},
		{"missing time", func(g *replayGroup) { g.Evidence.Warm.ProfileEnd = nil }},
		{"profile after warm RSS", func(g *replayGroup) { g.Evidence.Warm.RSSStart = f(*g.Evidence.Warm.ProfileEnd - 1) }},
		{"warm RSS late end", func(g *replayGroup) { g.Evidence.Warm.RSSEnd = f(*g.Evidence.Warm.Anchor + 635.001) }},
		{"reversed wall clock", func(g *replayGroup) { g.Evidence.End.ProfileEpoch = f(*g.Evidence.End.RequestEpoch - 1) }},
		{"changed anchor", func(g *replayGroup) { g.Evidence.End.Anchor = f(*g.Evidence.End.Anchor + 1) }},
		{"missing GC receipt", func(g *replayGroup) { g.Evidence.End.GCRequests = nil }},
		{"second GC", func(g *replayGroup) { v := 2; g.Evidence.End.GCRequests = &v }},
		{"short quiescence", func(g *replayGroup) { g.Evidence.End.Quiesce = f(59.999) }},
		{"gone without closure", func(g *replayGroup) { g.Live = false }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := liveFiler(t)
			tc.mutate(&g)
			if got := replay(g); got.Status != "FAIL" {
				t.Fatalf("accepted %+v", got)
			}
		})
	}
}
func u(v uint64) []byte {
	var b []byte
	for v >= 128 {
		b = append(b, byte(v)|128)
		v >>= 7
	}
	return append(b, byte(v))
}
func field(n byte, b []byte) []byte {
	return append(append([]byte{n*8 + 2}, u(uint64(len(b)))...), b...)
}
func heap(v uint64) []byte {
	data := field(1, []byte{8, 1, 16, 2})
	data = append(data, field(2, field(2, u(v)))...)
	for _, s := range []string{"", "inuse_space", "bytes"} {
		data = append(data, field(6, []byte(s))...)
	}
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	_, _ = w.Write(data)
	_ = w.Close()
	return out.Bytes()
}
func setHeap(c *Capture, v uint64) {
	c.Raw = heap(v)
	h := sha256.Sum256(c.Raw)
	c.SHA256 = hex.EncodeToString(h[:])
}
func TestExactThresholds(t *testing.T) {
	g := liveFiler(t)
	setHeap(g.Evidence.Warm, 100)
	setHeap(g.Evidence.End, 100+32*MiB-1)
	g.Evidence.End.RSS = f(*g.Evidence.Warm.RSS + 64*MiB)
	g.FDMax = f(*g.FD0 + 16)
	if got := replay(g); got.Status != "PASS" {
		t.Fatal(got)
	}
	setHeap(g.Evidence.End, 100+32*MiB)
	if got := replay(g); got.Status != "FAIL" || got.HeapOK {
		t.Fatal(got)
	}
	setHeap(g.Evidence.End, 100)
	g.Evidence.End.RSS = f(*g.Evidence.Warm.RSS + 64*MiB + 1)
	if got := replay(g); got.RSSOK || got.Status != "FAIL" {
		t.Fatal(got)
	}
	g.Evidence.End.RSS = g.Evidence.Warm.RSS
	g.FDMax = f(*g.FD0 + 17)
	if got := replay(g); got.FDsOK || got.Status != "FAIL" {
		t.Fatal(got)
	}
}
func TestClosedGenerationIsNotLiveMemoryPass(t *testing.T) {
	g := fixture(t, "132324")[0]
	if g.Evidence.Closed == nil {
		t.Fatal("fixture")
	}
	if got := replay(g); got.Status != "CLOSED" {
		t.Fatal(got)
	}
	g.Live = true
	if got := replay(g); got.Status != "FAIL" {
		t.Fatal(got)
	}
	g.Live = false
	g.Evidence.Closed.Start++
	if got := replay(g); got.Status != "FAIL" {
		t.Fatal(got)
	}
}

func TestClosedProfileStillNeedsArchiveBinding(t *testing.T) {
	g := fixture(t, "132324")[0]
	g.Evidence.Warm.Raw[0] ^= 1
	if got := replay(g); got.Status != "FAIL" {
		t.Fatal(got)
	}
}

func TestFilerUsesWarmRSSAndMasterKeepsInitialRSS(t *testing.T) {
	g := liveFiler(t)
	g.Baseline = nil // r6r3 does not reuse the startup RSS baseline.
	if got := replay(g); got.Status != "PASS" {
		t.Fatal(got)
	}
	for _, m := range fixture(t, "132324") {
		if m.Live && m.ID.Role == "master" {
			m.Baseline = nil
			if got := replay(m); got.Status != "FAIL" {
				t.Fatal(got)
			}
		}
	}
}

func TestFrozenFixtureHashes(t *testing.T) {
	for name, want := range map[string]string{
		"132324.json": "72996b6485b49af3b12388e34aa105dea07950661105949afe3b4a3b0f22656b",
		"095251.json": "0820d4e691558d94c159fc1c75e20100b2297eb470fcadb49f7d22224465b7b0",
	} {
		b, e := os.ReadFile("testdata/" + name)
		if e != nil {
			t.Fatal(e)
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != want {
			t.Fatalf("frozen fixture changed: %s", name)
		}
	}
}
func TestMalformedOrEmptyProfilesRefuse(t *testing.T) {
	for _, raw := range [][]byte{nil, {}, []byte("not gzip"), heap(1 << 63)} {
		if _, err := InuseSpace(raw); err == nil {
			t.Fatal("accepted malformed/negative profile")
		}
	}
	for _, g := range fixture(t, "132324") {
		if g.Live {
			for _, c := range []*Capture{g.Evidence.Warm, g.Evidence.End} {
				if _, e := InuseSpace(c.Raw); e != nil {
					t.Fatal(e)
				}
			}
		}
	}
}
