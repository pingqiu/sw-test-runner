package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirectSwctlSelectorReachesSharedPack(t *testing.T) {
	dir := t.TempDir()
	inv, cfg := filepath.Join(dir, "inv.json"), filepath.Join(dir, "cfg.json")
	os.WriteFile(inv, []byte(`{"target":"onprem"}`), 0600)
	os.WriteFile(cfg, []byte(`{"swctl":{"binary":{"path":"not-opened","sha256":"unused"},"cluster":"not-opened","artifacts_dir":"not-opened"},"files":{}}`), 0600)
	_, out := directCall(t, "--inventory", inv, "--config", cfg, "--out", filepath.Join(dir, "out"))
	if out.Code != "FAIL" || strings.Contains(out.Error, "unknown field") {
		t.Fatalf("swctl selector never reached shared pack: %+v", out)
	}
}
