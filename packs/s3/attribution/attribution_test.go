package attribution

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

func TestRetainedA1Rows(t *testing.T) {
	for _, run := range []string{"194018"} {
		t.Run(run, func(t *testing.T) {
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
			var p struct {
				Rows     []Operation
				Faults   []Fault
				Expected Report
			}
			if e = json.NewDecoder(z).Decode(&p); e != nil {
				t.Fatal(e)
			}
			got := Assess(p.Rows, p.Faults)
			if got.Status != p.Expected.Status || got.Failed != p.Expected.Failed || got.Refusals != p.Expected.Refusals || len(got.Unexcused) != len(p.Expected.Unexcused) {
				t.Fatalf("%+v want %+v", got, p.Expected)
			}
		})
	}
}

func TestRetainedFixturesArePinned(t *testing.T) {
	for name, want := range map[string]string{
		"194018.json.gz": "0edbe599909a02ae7f0b863bba74c26f1a3ffe5e53139f7fbffc8cb26912b493",
	} {
		b, e := os.ReadFile("testdata/" + name)
		if e != nil {
			t.Fatal(e)
		}
		h := sha256.Sum256(b)
		if hex.EncodeToString(h[:]) != want {
			t.Fatal("fixture changed", name)
		}
	}
}
