package seaweed

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

// twoHourCollect is an M02 collect row of the 2 h shape (#1002): 180 rotations, 540 episode lines.
func twoHourCollect(t *testing.T) []byte {
	t.Helper()
	body := collectRow(t, 180)
	if len(body) <= tr.MaxCapturedOutputBytes {
		t.Fatalf("fixture is not 2 h shaped: %d bytes", len(body))
	}
	return body
}

// collectRow is an M02 collect row with the given number of rotations (three episode lines each).
func collectRow(t *testing.T, rotations int) []byte {
	t.Helper()
	type line struct {
		VID  int    `json:"vid"`
		Line string `json:"line"`
	}
	type event struct {
		Kind   string `json:"kind"`
		VID    int    `json:"vid"`
		Stamp  string `json:"stamp"`
		Gen    int    `json:"gen"`
		Line   string `json:"line"`
		SHA256 string `json:"sha256"`
	}
	row := struct {
		Status  string           `json:"status"`
		Coll    string           `json:"collection"`
		Rotated []line           `json:"rotated"`
		Events  []event          `json:"events"`
		DatVIDs []int            `json:"dat_vids"`
		Sources []map[string]any `json:"sources"`
		Clock   map[string]any   `json:"clock"`
	}{Status: "SOAK_COLLECTED", Coll: "lmcache", DatVIDs: []int{3}, Sources: []map[string]any{{"path": "/r/master.stderr", "bytes": 1}},
		Clock: map[string]any{"boot_id": "b", "entry_epoch": 1.0, "exit_epoch": 2.0, "entry_mono": 3.0, "exit_mono": 4.0}}
	for vid := 100; vid < 100+rotations; vid++ {
		text := fmt.Sprintf("I1105 16:%02d:%02d.%06d master_cache_volume_rotation.go:279 rotated non-replicated cache Volume %d collection=lmcache bytes=33554432", vid%60, vid%60, vid, vid)
		row.Rotated = append(row.Rotated, line{vid, text})
		for _, kind := range []string{"birth", "unwritable", "success"} {
			sum := sha256.Sum256([]byte(kind + text))
			row.Events = append(row.Events, event{kind, vid, "1105 16:00:00.000001", 0, text, hex.EncodeToString(sum[:])})
		}
	}
	body, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func refRow(body []byte, mutate func(map[string]any)) []byte {
	sum := sha256.Sum256(body)
	ref := map[string]any{"status": "SOAK_COLLECTED", "file": "m02-collect.json", "bytes": len(body), "sha256": hex.EncodeToString(sum[:]),
		"clock": map[string]any{"boot_id": "b", "entry_epoch": 1.0, "exit_epoch": 2.0, "entry_mono": 3.0, "exit_mono": 4.0}}
	if mutate != nil {
		mutate(ref)
	}
	raw, _ := json.Marshal(ref)
	return append(raw, '\n')
}

func stdoutCapture(stdout []byte) tr.CapturedOutput {
	sum := sha256.Sum256(stdout)
	empty := sha256.Sum256(nil)
	now := time.Now()
	return tr.CapturedOutput{Complete: true, ReadStart: now, ReadEnd: now, StdoutBase64: base64.StdEncoding.EncodeToString(stdout),
		StdoutSHA256: hex.EncodeToString(sum[:]), StderrSHA256: hex.EncodeToString(empty[:])}
}

// The 2 h run's failure and its fix: the whole row as a job's stdout exceeds the capture bound (e9d422a1's path);
// the reference row decodes, and the file fetched for it is published byte for byte.
func TestA2HourM02CollectTravelsByFileNotByStdout(t *testing.T) {
	body := twoHourCollect(t)
	if _, _, err := stdoutCapture(append(body, '\n')).Decode(); err == nil || !strings.Contains(err.Error(), "exceeds capture bound") {
		t.Fatalf("the 2 h row as stdout decoded: %v", err)
	}
	row, _, err := stdoutCapture(refRow(body, nil)).Decode()
	if err != nil {
		t.Fatalf("the reference row: %v", err)
	}
	var asked []string
	got, err := m02Evidence(row, "m02-collect.json", func(name string) (string, error) {
		asked = append(asked, name)
		return string(body), nil
	})
	if err != nil || string(got) != string(body) || strings.Join(asked, ",") != "m02-collect.json" {
		t.Fatalf("published %d bytes (want %d), fetched %v: %v", len(got), len(body), asked, err)
	}
	if _, err := clockEvidence(row, "run", 10, 11); err != nil { // the S2 sample still comes from the reference row
		t.Fatalf("clock from the reference row: %v", err)
	}
}

func TestM02EvidenceRefusesAnyMismatchOrMalformedReference(t *testing.T) {
	body := twoHourCollect(t)
	fetch := func(b []byte) func(string) (string, error) {
		return func(string) (string, error) { return string(b), nil }
	}
	flipped := append([]byte{}, body...)
	flipped[len(flipped)/2] ^= 1
	malformed := []byte(`{"status":"SOAK_COLLECTED"} {"x":1}`)
	wrongStatus := []byte(`{"status":"FAILED"}`)
	for name, c := range map[string]struct {
		row   []byte
		fetch func(string) (string, error)
	}{
		"a changed byte":         {refRow(body, nil), fetch(flipped)},
		"a shorter file":         {refRow(body, nil), fetch(body[:len(body)-1])},
		"another file":           {refRow(body, func(m map[string]any) { m["file"] = "../m01-collect.json" }), fetch(body)},
		"no status":              {refRow(body, func(m map[string]any) { m["status"] = "FAILED" }), fetch(body)},
		"over the bound":         {refRow(body, func(m map[string]any) { m["bytes"] = m02EvidenceBound + 1 }), fetch(body)},
		"a bad digest":           {refRow(body, func(m map[string]any) { m["sha256"] = "ABC" }), fetch(body)},
		"an unknown field":       {refRow(body, func(m map[string]any) { m["rotated"] = []int{1} }), fetch(body)},
		"no clock":               {refRow(body, func(m map[string]any) { delete(m, "clock") }), fetch(body)},
		"two values in the file": {refRow(malformed, nil), fetch(malformed)},
		"a failed collect file":  {refRow(wrongStatus, nil), fetch(wrongStatus)},
		"a failed fetch":         {refRow(body, nil), func(string) (string, error) { return "", errors.New("ssh: gone") }},
		// codex01 R3: a case-folded alias of a reference field is refused, not decoded into it.
		"a Bytes alias":  {aliased(refRow(body, nil), `"bytes"`, `"Bytes"`), fetch(body)},
		"a FILE alias":   {aliased(refRow(body, nil), `"file"`, `"FILE"`), fetch(body)},
		"a SHA256 alias": {aliased(refRow(body, nil), `"sha256"`, `"SHA256"`), fetch(body)},
		"a Clock alias":  {aliased(refRow(body, nil), `"clock"`, `"Clock"`), fetch(body)},
		// codex01 R1: both clocks valid, and the file's the same as the reference row's.
		"another boot in the file":       {refRow(reclocked(body, `"boot_id":"b"`, `"boot_id":"c"`), nil), fetch(reclocked(body, `"boot_id":"b"`, `"boot_id":"c"`))},
		"another exit stamp in the file": {refRow(reclocked(body, `"exit_mono":4`, `"exit_mono":5`), nil), fetch(reclocked(body, `"exit_mono":4`, `"exit_mono":5`))},
		"no clock in the file":           {refRow(reclocked(body, `"clock":{`, `"clock_x":{`), nil), fetch(reclocked(body, `"clock":{`, `"clock_x":{`))},
		"an invalid reference clock":     {refRow(body, func(m map[string]any) { m["clock"].(map[string]any)["entry_mono"] = 0.0 }), fetch(body)},
		"a reference clock with no boot": {refRow(body, func(m map[string]any) { m["clock"].(map[string]any)["boot_id"] = "" }), fetch(body)},
	} {
		if got, err := m02Evidence(c.row, "m02-collect.json", c.fetch); err == nil {
			t.Errorf("%s: published %d bytes", name, len(got))
		}
	}
}

// aliased replaces one exact key with its case variant.
func aliased(row []byte, key, alias string) []byte {
	return []byte(strings.Replace(string(row), key, alias, 1))
}

// reclocked changes the collect body's own clock; refRow still carries the default reference clock.
func reclocked(body []byte, from, to string) []byte {
	if !strings.Contains(string(body), from) {
		panic("fixture has no " + from)
	}
	return []byte(strings.Replace(string(body), from, to, 1))
}
