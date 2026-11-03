package seaweed

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClockEvidenceRecordsThePairedCapture(t *testing.T) {
	row := `{"status":"SOAK_SETUP","clock":{"boot_id":"m02","entry_epoch":1793610000.5,"exit_epoch":1793610010.5,"entry_mono":7000.5,"exit_mono":7010.5}}`
	body, err := clockEvidence([]byte(row), "run-1", 100.25, 110.75)
	if err != nil {
		t.Fatal(err)
	}
	var got clockSample
	if err = json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	want := clockSample{BootID: "m02", RunID: "run-1", T1: 100.25, T4: 110.75, EntryEpoch: 1793610000.5, ExitEpoch: 1793610010.5, EntryMono: 7000.5, ExitMono: 7010.5}
	if got != want {
		t.Fatalf("got %+v", got)
	}
	// Field names are the assessor's contract (soak_assess.clock_bracket).
	for _, k := range []string{`"boot_id"`, `"run_id"`, `"t1"`, `"t4"`, `"entry_epoch"`, `"exit_epoch"`, `"entry_mono"`, `"exit_mono"`} {
		if !strings.Contains(string(body), k) {
			t.Fatalf("missing %s in %s", k, body)
		}
	}
}

func TestClockEvidenceRefusesAnIncompleteCapture(t *testing.T) {
	for name, row := range map[string]string{
		"no clock":    `{"status":"SOAK_SETUP"}`,
		"not json":    `partial`,
		"two rows":    `{"clock":{}} {"clock":{}}`,
		"no boot":     `{"clock":{"entry_epoch":1,"exit_epoch":1,"entry_mono":1,"exit_mono":1}}`,
		"zero fields": `{"clock":{"boot_id":"m02"}}`,
	} {
		if _, err := clockEvidence([]byte(row), "run-1", 1, 2); err == nil {
			t.Fatalf("%s: admitted", name)
		}
	}
	if _, err := clockEvidence([]byte(`{"clock":{"boot_id":"m02","entry_epoch":1,"exit_epoch":1,"entry_mono":1,"exit_mono":1}}`), "run-1", 0, 2); err == nil {
		t.Fatal("a zero T1 admitted")
	}
}

func TestClockAliasesAreRefused(t *testing.T) { // codex01 review b954b275
	for name, row := range map[string]string{
		"clock": `{"clock":{"boot_id":"bad"},"Clock":{"boot_id":"m02","entry_epoch":1793610000.5,"exit_epoch":1793610010.5,"entry_mono":7000.5,"exit_mono":7010.5}}`,
		"entry": `{"clock":{"boot_id":"m02","entry_epoch":1,"Entry_Epoch":1793610000.5,"exit_epoch":1793610010.5,"entry_mono":7000.5,"exit_mono":7010.5}}`,
		"exit":  `{"clock":{"boot_id":"m02","entry_epoch":1,"exit_epoch":2,"EXIT_EPOCH":3,"entry_mono":7000.5,"exit_mono":7010.5}}`,
		"mono":  `{"clock":{"boot_id":"m02","entry_epoch":1,"exit_epoch":2,"entry_mono":7000.5,"Entry_Mono":7,"exit_mono":7010.5}}`,
		"emono": `{"clock":{"boot_id":"m02","entry_epoch":1,"exit_epoch":2,"entry_mono":7000.5,"exit_mono":7010.5,"Exit_Mono":7}}`,
		"boot":  `{"clock":{"boot_id":"m02","Boot_ID":"x","entry_epoch":1,"exit_epoch":2,"entry_mono":7000.5,"exit_mono":7010.5}}`,
	} {
		if _, err := clockEvidence([]byte(row), "run-1", 100.25, 110.75); err == nil {
			t.Fatalf("%s: ambiguous clock fields admitted", name)
		}
	}
}

func TestPublishClockWritesTheRealEvidenceFile(t *testing.T) {
	defer func(f func(string, []byte) error) { clockPublish = f }(clockPublish)
	clockPublish = publishEvidence
	root := t.TempDir()
	row := []byte(`{"clock":{"boot_id":"m02","entry_epoch":1,"exit_epoch":2,"entry_mono":3,"exit_mono":4}}`)
	if err := publishClock(root, "soak-clock-s1.json", "run-1", row, 5, 6); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "soak-clock-s1.json"))
	if err != nil || !strings.Contains(string(raw), `"run_id":"run-1"`) {
		t.Fatalf("%s %v", raw, err)
	}
	if err = publishClock(root, "soak-clock-s1.json", "run-1", row, 5, 6); err == nil {
		t.Fatal("a second capture of one sample replaced the first")
	}
}

// Plan r2 step 2: the authorize stamp must lie inside the same job's envelope [envelope t1, t4); otherwise
// nothing is published, neither the sample nor its envelope.
func TestPublishSoakClockRefusesAStampOutsideTheEnvelope(t *testing.T) {
	defer func(f func(string, []byte) error) { clockPublish = f }(clockPublish)
	clockPublish = publishEvidence
	row := []byte(`{"status":"SOAK_SETUP","clock":{"boot_id":"m02","entry_epoch":1793610000.5,"exit_epoch":1793610010.5,"entry_mono":7000.5,"exit_mono":7010.5}}`)
	for _, c := range []struct {
		env, auth, t4 float64
		ok            bool
	}{{100, 100.5, 110, true}, {100, 100, 110, true}, {100, 99.9, 110, false}, {100, 110, 110, false}, {100, 111, 110, false}} {
		root := t.TempDir()
		err := publishSoakClock(root, "soak-clock-s1.json", "run-1", row, c.env, c.auth, c.t4)
		_, main := os.Stat(filepath.Join(root, "soak-clock-s1.json"))
		_, env := os.Stat(filepath.Join(root, "soak-clock-s1-envelope.json"))
		if c.ok != (err == nil) || c.ok != (main == nil) || c.ok != (env == nil) {
			t.Fatalf("%+v: err %v, sample %v, envelope %v", c, err, main, env)
		}
		if !c.ok {
			continue
		}
		var got, envelope clockSample
		a, _ := os.ReadFile(filepath.Join(root, "soak-clock-s1.json"))
		b, _ := os.ReadFile(filepath.Join(root, "soak-clock-s1-envelope.json"))
		if json.Unmarshal(a, &got) != nil || json.Unmarshal(b, &envelope) != nil || got.T1 != c.auth || envelope.T1 != c.env || got.T4 != c.t4 || envelope.T4 != c.t4 {
			t.Fatalf("%+v: published %+v and %+v", c, got, envelope)
		}
	}
}
