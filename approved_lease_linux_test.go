//go:build linux

package testrunner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestApprovedLeaseReadsExistingRecords(t *testing.T) {
	dir := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	write := func(suffix string, v any) {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, id+suffix+".json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	now := float64(time.Now().UnixNano()) / 1e9
	write("-acquire", map[string]any{"lease_id": id, "agent": "codex03", "started": now - 1200, "host": "broker-host-not-a-host-set", "purpose": "existing lease"})
	write("-renew", map[string]any{"lease_id": id, "agent": "codex03", "renewed": now - 10})
	r, err := ReadLiveLease(dir, id, "codex03")
	if err != nil || r.LeaseID != id {
		t.Fatalf("existing renewal: %#v %v", r, err)
	}
	for _, v := range []map[string]any{
		{"lease_id": id, "agent": "other", "renewed": now - 10},
		{"lease_id": id, "agent": "codex03", "renewed": now + 60},
		{"lease_id": id, "agent": "codex03", "renewed": now - 601},
		{"lease_id": id, "agent": "codex03", "renewed": "fresh"},
	} {
		write("-renew", v)
		if _, err := ReadLiveLease(dir, id, "codex03"); err == nil {
			t.Fatalf("accepted invalid renewal %#v", v)
		}
	}
	write("-renew", map[string]any{"lease_id": id, "agent": "codex03", "renewed": now - 10})
	write("-release", map[string]any{"lease_id": id})
	if _, err := ReadLiveLease(dir, id, "codex03"); err == nil {
		t.Fatal("release must refuse work even with a fresh renewal")
	}
}
