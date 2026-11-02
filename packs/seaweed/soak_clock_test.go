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
