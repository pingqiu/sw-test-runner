package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/packs/seaweed"
	"github.com/pingqiu/sw-test-runner/provision/smoke"
)

func dryRunFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if e := os.Mkdir(bin, 0700); e != nil {
		t.Fatal(e)
	}
	write := func(p, s string, mode os.FileMode) {
		t.Helper()
		if e := os.WriteFile(p, []byte(s), mode); e != nil {
			t.Fatal(e)
		}
	}
	log := filepath.Join(root, "commands")
	t.Setenv("DRY_CALL_LOG", log)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	for _, tool := range []string{"az", "terraform"} {
		write(filepath.Join(bin, tool), "#!/bin/sh\nprintf '%s\\n' \"$0 $*\" >> \"$DRY_CALL_LOG\"\ncase \"$1\" in resource) printf '[]';; output) printf '{}';; esac\n", 0700)
	}
	source, e := filepath.Abs("../..")
	if e != nil {
		t.Fatal(e)
	}
	_, pin, e := smoke.Sources(source)
	if e != nil {
		t.Fatal(e)
	}
	hard := time.Now().UTC().Add(50 * time.Minute).Truncate(time.Minute)
	vars, _ := json.Marshal(map[string]any{"subscription_id": "11111111-1111-1111-1111-111111111111", "resource_group_name": "test", "location": "westus2", "vm_size": "Standard_D2s_v5", "run_declaration": map[string]any{"work_deadline": hard.Add(-20 * time.Minute), "hard_deadline": hard, "cloud_cleanup_reserve_seconds": 1200, "budget_usd": 3, "abort_estimate_usd": 2.5}})
	vp := filepath.Join(root, "vars.json")
	write(vp, string(vars), 0600)
	c := smoke.Config{Source: source, ModuleSHA256: pin, Variables: vp, VariablesSHA256: fmt.Sprintf("%x", sha256.Sum256(vars)), Output: filepath.Join(root, "output"), EstimatedUSD: .12, Cloud: seaweed.CloudConfig{SSHKey: filepath.Join(root, "key"), Files: map[string]seaweed.CloudFile{}}}
	for _, name := range []string{"weed", "weed-volume", "native.tar"} {
		p := filepath.Join(root, name)
		write(p, name, 0600)
		c.Cloud.Files[name] = seaweed.CloudFile{Path: p, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(name)))}
	}
	b, _ := json.Marshal(c)
	cp := filepath.Join(root, "config.json")
	write(cp, string(b), 0600)
	return cp, log
}

func TestRunDryRunHasNoNativeEffects(t *testing.T) {
	cp, log := dryRunFixture(t)
	var output bytes.Buffer
	rc := run([]string{"run", "--config", cp, "--dry-run", "--json"}, &output)
	b, _ := os.ReadFile(log)
	if strings.Contains(string(b), "apply") || strings.Contains(string(b), "destroy") || strings.Contains(string(b), "ssh") {
		t.Fatalf("dry-run issued paid/remote effects: %s", b)
	}
	var result smoke.Result
	if rc != 0 || json.Unmarshal(output.Bytes(), &result) != nil || result.Code != "PLAN_ONLY" || len(result.Effects) != 0 || len(b) != 0 {
		t.Fatalf("not a local plan: rc=%d %s commands=%s", rc, output.String(), b)
	}
}

func TestVerifyAndClusterDryRunHaveNoNativeEffects(t *testing.T) {
	for _, mode := range []string{"verify", "cluster-run"} {
		t.Run(mode, func(t *testing.T) {
			cp, log := dryRunFixture(t)
			verb := mode
			if mode == "cluster-run" {
				verb = "run"
				b, _ := os.ReadFile(cp)
				var c smoke.Config
				if e := json.Unmarshal(b, &c); e != nil {
					t.Fatal(e)
				}
				b, _ = os.ReadFile(c.Variables)
				var vars map[string]any
				if e := json.Unmarshal(b, &vars); e != nil {
					t.Fatal(e)
				}
				vars["cluster"] = true
				b, _ = json.Marshal(vars)
				if e := os.WriteFile(c.Variables, b, 0600); e != nil {
					t.Fatal(e)
				}
				c.VariablesSHA256 = fmt.Sprintf("%x", sha256.Sum256(b))
				b, _ = json.Marshal(c)
				if e := os.WriteFile(cp, b, 0600); e != nil {
					t.Fatal(e)
				}
			}
			var output bytes.Buffer
			if run([]string{verb, "--config", cp, "--dry-run"}, &output) != 0 {
				t.Fatal(output.String())
			}
			b, _ := os.ReadFile(log)
			var result smoke.Result
			if len(b) != 0 || json.Unmarshal(output.Bytes(), &result) != nil || result.Code != "PLAN_ONLY" || len(result.Effects) != 0 {
				t.Fatalf("%s commands=%s", output.String(), b)
			}
			if mode == "cluster-run" && result.Plan["vm_count"] != float64(10) {
				t.Fatal(result)
			}
		})
	}
}

func TestOrdinaryRunRetainsIntentionalApplyPath(t *testing.T) {
	cp, log := dryRunFixture(t)
	var output bytes.Buffer
	if run([]string{"run", "--config", cp}, &output) == 0 {
		t.Fatal("fake invalid inventory must fail")
	}
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "terraform apply") || !strings.Contains(string(b), "terraform destroy") {
		t.Fatalf("ordinary run mutation path changed: %s %s", output.String(), b)
	}
}

func TestCleanupUnresolvableHomeNeverReadsCWDRecovery(t *testing.T) {
	for _, home := range []string{"", "."} {
		t.Run("home="+home, func(t *testing.T) {
			cwd := t.TempDir()
			t.Chdir(cwd)
			t.Setenv("HOME", home)
			p := filepath.Join(cwd, ".local/state/seaweed-cloud/runs/probe")
			if e := os.MkdirAll(p, 0700); e != nil {
				t.Fatal(e)
			}
			if e := os.WriteFile(filepath.Join(p, "recovery.json"), []byte(`{}`), 0600); e != nil {
				t.Fatal(e)
			}
			var output bytes.Buffer
			if run([]string{"cleanup", "--run", "probe", "--dry-run"}, &output) == 0 {
				t.Fatalf("read cwd-relative recovery with unresolved HOME: %s", output.String())
			}
			var result smoke.Result
			if json.Unmarshal(output.Bytes(), &result) != nil || result.Code != "INPUT_REFUSED" || len(result.Effects) != 0 {
				t.Fatal(output.String())
			}
		})
	}
}
