package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDirectRefusesIncompleteInputBeforeAnyRun(t *testing.T) {
	dir := t.TempDir()
	inv := filepath.Join(dir, "inv.json")
	cfg := filepath.Join(dir, "cfg.json")
	os.WriteFile(inv, []byte(`{}`), 0600)
	os.WriteFile(cfg, []byte(`{"ssh_key":"k","host_key":"h","files":{},"extra":1}`), 0600)
	existing := filepath.Join(dir, "exists")
	os.Mkdir(existing, 0700)
	for name, args := range map[string][]string{
		"no flags":             {},
		"no output dir":        {"--inventory", inv, "--config", cfg},
		"unknown config field": {"--inventory", inv, "--config", cfg, "--out", filepath.Join(dir, "o1")},
		"existing output dir":  {"--inventory", inv, "--config", filepath.Join(dir, "absent.json"), "--out", existing},
		"extra argument":       {"--inventory", inv, "--config", cfg, "--out", filepath.Join(dir, "o2"), "extra"},
	} {
		t.Run(name, func(t *testing.T) {
			var w bytes.Buffer
			if code := run(append([]string{"direct"}, args...), &w); code != 1 {
				t.Fatalf("exit %d", code)
			}
			var out directResult
			if json.Unmarshal(w.Bytes(), &out) != nil || out.Code != "INPUT_REFUSED" || out.Schema != "seaweed.direct-run/v1" {
				t.Fatalf("output %q", w.String())
			}
		})
	}
}

func TestDirectFailsAnInventoryThePackRefuses(t *testing.T) {
	dir := t.TempDir()
	inv := filepath.Join(dir, "inv.json")
	cfg := filepath.Join(dir, "cfg.json")
	os.WriteFile(inv, []byte(`{}`), 0600)
	os.WriteFile(cfg, []byte(`{"ssh_key":"k","host_key":"h","files":{}}`), 0600)
	var w bytes.Buffer
	if code := run([]string{"direct", "--inventory", inv, "--config", cfg, "--out", filepath.Join(dir, "out")}, &w); code != 1 {
		t.Fatalf("exit %d", code)
	}
	var out directResult
	if json.Unmarshal(w.Bytes(), &out) != nil || out.Code != "FAIL" || out.Error == "" {
		t.Fatalf("output %q", w.String())
	}
}
