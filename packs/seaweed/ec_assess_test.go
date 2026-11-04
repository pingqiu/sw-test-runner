package seaweed

import (
	"encoding/json"
	"os"
	"testing"
)

func TestECPlantEvidenceJoin(t *testing.T) {
	m := EC1aManifest{RunID: "ec-join", Generation: "1"}
	key := "ec-0"
	payload := ecPayload(m.RunID, key)
	m.Objects = []EC1aObject{{Key: key, FID: "1,abc", Length: len(payload), SHA256: hash(payload), ACK: true}}
	original := ecOutcome{chaosOp: chaosOp{Op: 0, Kind: "s3_get", Key: key, Started: 1, Ended: 2, Exact: true}, Route: "s3", Status: 200, Length: len(payload), SHA256: hash(payload)}
	payload[0] ^= 1
	planted := original
	planted.Exact, planted.SHA256, planted.Original = false, hash(payload), &original
	good := ecReadBody{RunID: m.RunID, Wrong: 1, Control: "WRONG_BYTE_REJECTED", Outcomes: []ecOutcome{planted}}
	raw, _ := json.Marshal(good)
	for _, mode := range []string{"positive", "run", "route", "key", "original-digest", "mutated-digest", "time", "not-original-exact", "nested"} {
		t.Run(mode, func(t *testing.T) {
			var body ecReadBody
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			r := &body.Outcomes[0]
			o := r.Original
			switch mode {
			case "run":
				body.RunID = "foreign"
			case "route":
				r.Route, o.Route = "volume", "volume"
			case "key":
				r.Key, o.Key = "ec-foreign", "ec-foreign"
			case "original-digest":
				o.SHA256 = "foreign"
			case "mutated-digest":
				r.SHA256 = "foreign"
			case "time":
				o.Ended = 3
			case "not-original-exact":
				o.Exact = false
			case "nested":
				o.Original = &ecOutcome{}
			}
			if err := ecAssessRead(body, "s3", m, true); (err == nil) != (mode == "positive") {
				t.Fatalf("mode %s: %v", mode, err)
			}
		})
	}
}

func TestECRealCorruptResponseCannotEarnControlPass(t *testing.T) {
	raw, err := os.ReadFile("testdata/ec-preplant-controls.json")
	if err != nil {
		t.Fatal(err)
	}
	var receipts struct {
		Cases []struct {
			Mode   string
			Plant  bool
			Exit   int
			Output json.RawMessage
		}
	}
	if err = json.Unmarshal(raw, &receipts); err != nil {
		t.Fatal(err)
	}
	bad := receipts.Cases[1]
	if bad.Mode != "corrupt" || !bad.Plant || bad.Exit != 0 {
		t.Fatal("counterexample not reproduced")
	}
	var body ecReadBody
	if err = json.Unmarshal(bad.Output, &body); err != nil {
		t.Fatal(err)
	}
	manifest := EC1aManifest{RunID: body.RunID, Generation: "1"}
	for _, row := range body.Outcomes {
		payload := ecPayload(body.RunID, row.Key)
		manifest.Objects = append(manifest.Objects, EC1aObject{Key: row.Key, FID: "1,abc", Length: len(payload), SHA256: hash(payload), ACK: true})
	}
	if err = ecAssessRead(body, "s3", manifest, true); err == nil {
		t.Fatal("real corrupt pre-plant response accepted")
	}
}
