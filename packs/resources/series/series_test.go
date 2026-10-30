package series

import (
	"compress/gzip"
	"encoding/json"
	"math"
	"os"
	"reflect"
	"testing"
)

func fixture(t *testing.T, run string) (Input, map[string]Check) {
	t.Helper()
	f, e := os.Open("testdata/" + run + ".json.gz")
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	z, e := gzip.NewReader(f)
	if e != nil {
		t.Fatal(e)
	}
	defer z.Close()
	var packet struct {
		Input    Input
		Expected map[string]Check
	}
	if e = json.NewDecoder(z).Decode(&packet); e != nil {
		t.Fatal(e)
	}
	return packet.Input, packet.Expected
}
func TestFullFrozenResourceParity(t *testing.T) {
	for _, run := range []string{"132324", "095251"} {
		t.Run(run, func(t *testing.T) {
			in, want := fixture(t, run)
			got := Assess(in)
			status := "PASS"
			if run == "095251" {
				status = "FAIL"
			}
			if got.Status != status {
				t.Fatalf("resource aggregate %s want %s", got.Status, status)
			}
			if len(got.Checks) != 5 {
				t.Fatal("missing check", got)
			}
			for name, w := range want {
				g := got.Checks[name]
				if g.Status != w.Status {
					t.Fatalf("%s %s want %s", name, g.Status, w.Status)
				}
				if !reflect.DeepEqual(g.MaxData,w.MaxData)||!reflect.DeepEqual(g.MaxAux,w.MaxAux){t.Fatalf("%s disk diagnostic differs",name)}
				if len(g.Details) != len(w.Details) {
					t.Fatalf("%s lost process generations %d want %d", name, len(g.Details), len(w.Details))
				}
				for i, d := range w.Details {
					a := g.Details[i]
					if a.Identity != d.Identity || a.Status != d.Status || a.Samples != d.Samples {
						t.Fatalf("%s row %d %+v want %+v", name, i, a, d)
					}
					for key, pair := range map[string][2]float64{"rss baseline": {a.RSSBaseline, d.RSSBaseline}, "rss tail": {a.RSSTail, d.RSSTail}, "growth": {a.RSSGrowth, d.RSSGrowth}, "slope": {a.Slope, d.Slope}, "fd baseline": {a.FDBaseline, d.FDBaseline}, "fd max": {a.FDMax, d.FDMax}} {
						if math.Abs(pair[0]-pair[1]) > 1e-9 {
							t.Fatalf("%s/%+v/%s: %v", name, d.Identity, key, pair)
						}
					}
					if !reflect.DeepEqual(a.Baseline, d.Baseline) || !reflect.DeepEqual(a.Final, d.Final) {
						t.Fatalf("%s terminal pin/lock values differ", name)
					}
					if d.FDWindow != nil && (!reflect.DeepEqual(a.FDWindow, d.FDWindow) || a.FDFrom != d.FDFrom) {
						t.Fatal("warm FD sample selection changed", a.FDWindow, d.FDWindow)
					}
					if !reflect.DeepEqual(a.PinWindow, d.PinWindow) {
						t.Fatal("terminal pin baseline sample selection changed")
					}
					if !reflect.DeepEqual(a.GoMemory,d.GoMemory){t.Fatalf("%s Go-memory join differs",name)}
				}
			}
		})
	}
}
