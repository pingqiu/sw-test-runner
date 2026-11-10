package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func directCall(t *testing.T, args ...string) (int, directResult) {
	t.Helper()
	var w bytes.Buffer
	code := run(append([]string{"direct"}, args...), &w)
	var out directResult
	if json.Unmarshal(w.Bytes(), &out) != nil || out.Schema != "seaweed.direct-run/v1" {
		t.Fatalf("output %q", w.String())
	}
	return code, out
}

// Each refusal is for its own reason: the other inputs are valid.
func TestDirectRefusesIncompleteInputBeforeAnyRun(t *testing.T) {
	dir := t.TempDir()
	inv := filepath.Join(dir, "inv.json")
	cfg := filepath.Join(dir, "cfg.json")
	bad := filepath.Join(dir, "bad.json")
	azure := filepath.Join(dir, "azure.json")
	os.WriteFile(inv, []byte(`{"target":"onprem"}`), 0600)
	os.WriteFile(azure, []byte(`{"target":"azure"}`), 0600)
	os.WriteFile(cfg, []byte(`{"ssh_key":"k","host_key":"h","files":{}}`), 0600)
	os.WriteFile(bad, []byte(`{"ssh_key":"k","host_key":"h","files":{},"extra":1}`), 0600)
	existing := filepath.Join(dir, "exists")
	os.Mkdir(existing, 0700)
	for _, c := range []struct {
		name, want string
		args       []string
	}{
		{"no flags", "use: direct", nil},
		{"no output dir", "use: direct", []string{"--inventory", inv, "--config", cfg}},
		{"extra argument", "use: direct", []string{"--inventory", inv, "--config", cfg, "--out", filepath.Join(dir, "o1"), "extra"}},
		{"azure inventory", "only an onprem or docker inventory", []string{"--inventory", azure, "--config", cfg, "--out", filepath.Join(dir, "o2")}},
		{"unknown config field", "extra", []string{"--inventory", inv, "--config", bad, "--out", filepath.Join(dir, "o3")}},
		{"existing output dir", "exist", []string{"--inventory", inv, "--config", cfg, "--out", existing}},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, out := directCall(t, c.args...)
			if code != 1 || out.Code != "INPUT_REFUSED" || !strings.Contains(out.Error, c.want) {
				t.Fatalf("want INPUT_REFUSED containing %q, got exit %d %+v", c.want, code, out)
			}
		})
	}
}

func TestDirectFailsAnInventoryThePackRefuses(t *testing.T) {
	dir := t.TempDir()
	inv := filepath.Join(dir, "inv.json")
	cfg := filepath.Join(dir, "cfg.json")
	os.WriteFile(inv, []byte(`{"target":"onprem"}`), 0600)
	os.WriteFile(cfg, []byte(`{"ssh_key":"k","host_key":"h","files":{}}`), 0600)
	code, out := directCall(t, "--inventory", inv, "--config", cfg, "--out", filepath.Join(dir, "out"))
	if code != 1 || out.Code != "FAIL" || out.Error == "" {
		t.Fatalf("exit %d %+v", code, out)
	}
}

// The ten-node mount soak rehearsal reaches the pack only for a docker inventory, with host_keys and mount_soak
// together; any other combination is refused before an output directory exists.
func TestDirectMountSoakOnlyForADockerRehearsal(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		name, target, cfg string
		refused           bool
	}{
		{"onprem", "onprem", `{"ssh_key":"k","host_key":"","files":{},"mount_soak":true,"host_keys":{"meta":"p"}}`, true},
		{"docker without pins", "docker", `{"ssh_key":"k","host_key":"","files":{},"mount_soak":true}`, true},
		{"pins without mount soak", "docker", `{"ssh_key":"k","host_key":"","files":{},"host_keys":{"meta":"p"}}`, true},
		{"docker rehearsal", "docker", `{"ssh_key":"k","host_key":"","files":{},"mount_soak":true,"host_keys":{"meta":"p"}}`, false},
	} {
		inv, cfg, out := filepath.Join(dir, c.name+"-inv.json"), filepath.Join(dir, c.name+"-cfg.json"), filepath.Join(dir, c.name+"-out")
		os.WriteFile(inv, []byte(`{"target":"`+c.target+`"}`), 0600)
		os.WriteFile(cfg, []byte(c.cfg), 0600)
		_, got := directCall(t, "--inventory", inv, "--config", cfg, "--out", out)
		_, statErr := os.Stat(out)
		if c.refused && (got.Code != "INPUT_REFUSED" || !strings.Contains(got.Error, "docker inventory") || statErr == nil) {
			t.Fatalf("%s: not refused before any run: %+v", c.name, got)
		}
		if !c.refused && (got.Code != "FAIL" || strings.Contains(got.Error, "docker inventory") || strings.Contains(got.Error, "unknown field")) {
			t.Fatalf("%s: did not reach the pack: %+v", c.name, got) // the pack then refuses the one-field inventory
		}
	}
}

func TestDirectECSelectorReachesPack(t *testing.T) {
	dir := t.TempDir()
	inv, cfg := filepath.Join(dir, "inv.json"), filepath.Join(dir, "cfg.json")
	os.WriteFile(inv, []byte(`{"target":"onprem"}`), 0600)
	os.WriteFile(cfg, []byte(`{"ssh_key":"k","host_key":"h","files":{},"ec_stage1":true}`), 0600)
	_, out := directCall(t, "--inventory", inv, "--config", cfg, "--out", filepath.Join(dir, "out"))
	if out.Code != "FAIL" || strings.Contains(out.Error, "unknown field") {
		t.Fatalf("EC selector did not reach shared pack: %+v", out)
	}
}

func TestDirectMasterWritesSelectorReachesPack(t *testing.T) {
	dir := t.TempDir()
	inv, cfg := filepath.Join(dir, "inv.json"), filepath.Join(dir, "cfg.json")
	os.WriteFile(inv, []byte(`{"target":"onprem"}`), 0600)
	os.WriteFile(cfg, []byte(`{"ssh_key":"k","host_key":"h","files":{},"master_writes":true}`), 0600)
	_, out := directCall(t, "--inventory", inv, "--config", cfg, "--out", filepath.Join(dir, "out"))
	if out.Code != "FAIL" || strings.Contains(out.Error, "unknown field") {
		t.Fatalf("master writes selector did not reach shared pack: %+v", out)
	}
}

func TestDirectReadCaseSelectorReachesPack(t *testing.T) {
	dir := t.TempDir()
	inv, cfg := filepath.Join(dir, "inv.json"), filepath.Join(dir, "cfg.json")
	raw, err := os.ReadFile("../../inventory/testdata/http.requested.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.Unmarshal(raw, &doc)
	doc["target"] = "onprem"
	raw, _ = json.Marshal(doc)
	os.WriteFile(inv, raw, 0600)
	os.WriteFile(cfg, []byte(`{"read_case":"invalid-case"}`), 0600)
	_, out := directCall(t, "--inventory", inv, "--config", cfg, "--out", filepath.Join(dir, "out"))
	if out.Code != "FAIL" || !strings.Contains(out.Error, "read_case requires") {
		t.Fatalf("selector did not reach pack: %+v", out)
	}
}

func TestDirectECUnderQuorumSelectorReachesPack(t *testing.T) {
	dir := t.TempDir()
	inv, cfg := filepath.Join(dir, "inv.json"), filepath.Join(dir, "cfg.json")
	os.WriteFile(inv, []byte(`{"target":"onprem"}`), 0600)
	os.WriteFile(cfg, []byte(`{"ssh_key":"k","host_key":"h","files":{},"ec_stage1":true,"ec_under_quorum":true}`), 0600)
	_, out := directCall(t, "--inventory", inv, "--config", cfg, "--out", filepath.Join(dir, "out"))
	if out.Code != "FAIL" || strings.Contains(out.Error, "unknown field") {
		t.Fatalf("EC selector did not reach shared pack: %+v", out)
	}
}

func TestDirectRustVolumeSelectorReachesPack(t *testing.T) {
	dir := t.TempDir()
	inv, cfg := filepath.Join(dir, "inv.json"), filepath.Join(dir, "cfg.json")
	os.WriteFile(inv, []byte(`{"target":"onprem"}`), 0600)
	os.WriteFile(cfg, []byte(`{"ssh_key":"k","host_key":"h","files":{},"ec_stage1":true,"ec_volume_impl":"rust"}`), 0600)
	_, out := directCall(t, "--inventory", inv, "--config", cfg, "--out", filepath.Join(dir, "out"))
	if out.Code != "FAIL" || strings.Contains(out.Error, "unknown field") {
		t.Fatalf("EC selector did not reach shared pack: %+v", out)
	}
}
