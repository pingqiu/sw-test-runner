package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testSub = "11111111-1111-1111-1111-111111111111"

// A recorded listing drives a dry run with no Azure call; the receipt and the one-call journal land in a fresh
// directory. --listing without --dry-run, bad inputs and a relative receipts path are refused before anything.
func TestSweepCommandDryRunFromARecordedListing(t *testing.T) {
	dir := t.TempDir()
	listing := filepath.Join(dir, "list.json")
	base := "/subscriptions/" + testSub + "/resourceGroups/dedicated-test/providers/"
	rows := []map[string]any{
		{"id": base + "Microsoft.Compute/virtualMachines/r-vm", "type": "Microsoft.Compute/virtualMachines", "tags": map[string]string{"seaweed_run": "r", "managed_by": "sw-test-runner", "expires_at": "2026-01-01T00:00:00Z"}},
		{"id": base + "Microsoft.Compute/disks/other", "type": "Microsoft.Compute/disks", "tags": map[string]string{}},
	}
	b, _ := json.Marshal(rows)
	os.WriteFile(listing, b, 0600)
	receipts := filepath.Join(dir, "receipts")
	orig := sweepLive
	sweepLive = func(string) bool { return false }
	defer func() { sweepLive = orig }()
	var out bytes.Buffer
	if code := runSweep([]string{"--subscription", testSub, "--resource-group", "dedicated-test", "--receipts", receipts, "--dry-run", "--listing", listing}, &out); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	var r struct {
		DryRun   bool `json:"dry_run"`
		Runs     []struct{ Run, Action string }
		Untagged []string `json:"untagged_left"`
	}
	json.Unmarshal(out.Bytes(), &r)
	if !r.DryRun || len(r.Runs) != 1 || r.Runs[0].Action != "would-delete" || len(r.Untagged) != 1 {
		t.Fatalf("%s", out.String())
	}
	sweeps, _ := os.ReadDir(receipts)
	if len(sweeps) != 1 {
		t.Fatalf("want one sweep directory, got %d", len(sweeps))
	}
	files, _ := os.ReadDir(filepath.Join(receipts, sweeps[0].Name()))
	names := []string{}
	for _, f := range files {
		names = append(names, f.Name())
	}
	if strings.Join(names, " ") != "001-az-intent.json 001-az-output 001-az-status receipt.json" {
		t.Fatalf("journal: %v", names)
	}
	for _, bad := range [][]string{
		{"--subscription", testSub, "--resource-group", "dedicated-test", "--receipts", receipts, "--listing", listing}, // not a dry run
		{"--subscription", "nope", "--resource-group", "dedicated-test", "--receipts", receipts, "--dry-run", "--listing", listing},
		{"--subscription", testSub, "--resource-group", "bad group", "--receipts", receipts, "--dry-run", "--listing", listing},
		{"--subscription", testSub, "--resource-group", "dedicated-test", "--receipts", "relative", "--dry-run", "--listing", listing},
	} {
		out.Reset()
		if code := runSweep(bad, &out); code != 1 || !strings.Contains(out.String(), "error") {
			t.Fatalf("%v accepted: %s", bad, out.String())
		}
	}
}
