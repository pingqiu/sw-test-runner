//go:build linux

package testrunner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClaimedRequestIsStrictData(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "r1.env")
	// The queue suffix stays .env, but S0b's versioned body is JSON data.
	body := `{"version":"s0b-request-v1","binding":{"request_id":"r1","run_id":"run1","workload":"kv","transport":"rdma"}}`
	if err := os.WriteFile(name, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := ReadClaimedRequest(name, dir, uint32(os.Getuid()))
	if err != nil || r.Binding.RequestID != "r1" || string(r.Bytes) != body {
		t.Fatalf("snapshot: %#v %v", r, err)
	}
	for _, bad := range []string{
		`{"version":"s0b-request-v1","version":"s0b-request-v1","binding":{"request_id":"r1"}}`,
		`{"version":"s0b-request-v1","binding":{"request_id":"r1","transport":"http","transport":"rdma"}}`,
		`{"version":"s0b-request-v1","Version":"s0b-request-v1","binding":{"request_id":"r1"}}`,
		`{"version":"s0b-request-v1","binding":{"request_id":"r1"},"RUN_SCRIPT":"$(touch unwanted)"}`,
		`{"version":"s0b-request-v1","binding":{"request_id":"other"}}`,
		`TESTOPS_MONO_REF=$(touch unwanted)`,
		body + ` {}`,
	} {
		if err := os.WriteFile(name, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadClaimedRequest(name, dir, uint32(os.Getuid())); err == nil {
			t.Fatalf("accepted submitted data %q", bad)
		}
	}
}
