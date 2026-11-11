package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const fakePEM = "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----\n"

// savedRun is a saved run laid out as the first Azure attempt left it (journal, prepare and collect records, the
// Terraform working copy with host keys), plus the mount soak's success files.
func savedRun(t *testing.T) string {
	src := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for rel, body := range map[string]string{
		// exported
		"001-az-intent.json":                         `["resource","list","--output","json"]`,
		"003-terraform-intent.json":                  `["init","-backend=false"]`,
		"requested-inventory.json":                   `{"run_id":"r","hosts":[{"id":"vol1"}]}`,
		"recovery.json":                              `{"output":"/root/.local/state/seaweed-cloud/runs/r"}`,
		"prepare/vol1/cloud-init-step.json":          `{"command":"timeout -k 5 120 sh -ec 'cloud-init status --wait --format json'","exit_code":124,"stdout":"Failed to write to /dev/console\nReceived signal 15\n"}`,
		"prepare/vol6/cloud-init-step.json":          `{"exit_code":0,"stdout":"{\n  \"status\": \"done\",\n  \"errors\": []\n}\n"}`,
		"prepare/vol6/cloud-step-001.json":           `{"name":"prepare","exit_code":0,"stdout":""}`,
		"collect/meta/cloud-failure-fetch.json":      `{"step":"failure-log-fetch","stdout":"-- No entries --\n"}`,
		"mount-verdicts.json":                        `{"schema":"seaweed-mount-cluster/1","status":"FAIL"}`,
		"mount/client1/mount-step-001.json":          `{"name":"mount-soak","stdout":"{\"event\":\"ACK\",\"n\":1}\n{\"event\":\"ACK\",\"n\":2}\n"}`,
		"mount/client1/mount-verdict.json":           `{"schema":"seaweed-mount-verdict/1","status":"FAIL"}`,
		"mount/client1/run-root/mount-journal.jsonl": "{\"e\":1}\n{\"e\":2}\n",
		"mount/client1/run-root/mount-env.json":      `{"kernel":"6.8"}`,
		// withheld: known, stays on the controller
		"001-az-output":                                               `[]`,
		"001-az-status":                                               `OK`,
		"003-terraform-output":                                        "Terraform has been successfully initialized!\n",
		"apply-started":                                               "apply may have issued creates",
		"prepare/vol1/cloud-init-status.json":                         "Failed to write to /dev/console\n",
		"collect/meta/cloud-services.log":                             "-- No entries --\n",
		"mount/client1/run-root/mount-w1.log":                         "weed mount log\n",
		"provision/terraform/azure/variables.tf":                      `type = map(object({ ed25519_private = string }))`,
		"provision/terraform/azure-validation/host.auto.tfvars.json":  `{"node_host_keys":{"vol1":{"ed25519_private":"` + strings.ReplaceAll(fakePEM, "\n", `\n`) + `"}}}`,
		"provision/terraform/azure-validation/terraform.tfstate":      `{"resources":[]}`,
		"provision/terraform/azure-validation/.terraform/providers/p": "binary PRIVATE KEY----- strings",
	} {
		write(rel, body)
	}
	return src
}

func TestBundleExportsOnlyAllowlistedArtifacts(t *testing.T) {
	src := savedRun(t)
	dest := filepath.Join(t.TempDir(), "bundle")
	files, withheld, err := bundleRun(src, dest)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"001-az-intent.json", "003-terraform-intent.json", "collect/meta/cloud-failure-fetch.json",
		"mount-verdicts.json", "mount/client1/mount-step-001.json", "mount/client1/mount-verdict.json",
		"mount/client1/run-root/mount-env.json", "mount/client1/run-root/mount-journal.jsonl",
		"prepare/vol1/cloud-init-step.json", "prepare/vol6/cloud-init-step.json", "prepare/vol6/cloud-step-001.json",
		"recovery.json", "requested-inventory.json"}
	sort.Strings(want)
	if strings.Join(files, ",") != strings.Join(want, ",") {
		t.Fatalf("exported %v\nwant %v", files, want)
	}
	// 7 known raw files, plus the Terraform working copy as one subtree.
	if len(withheld) != 8 || !contains(withheld, "provision/terraform/") {
		t.Fatalf("withheld %v", withheld)
	}
	_ = filepath.WalkDir(dest, func(p string, d os.DirEntry, e error) error {
		if e == nil && !d.IsDir() {
			if b, _ := os.ReadFile(p); keyMaterial.Match(b) || strings.Contains(p, "tfvars") || strings.Contains(p, "provision") {
				t.Fatalf("controller-only content in the bundle: %s", p)
			}
		}
		return nil
	})
	if _, err := os.Stat(filepath.Join(dest, "SHA256SUMS")); err != nil {
		t.Fatal(err)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func refused(t *testing.T, src string) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "bundle")
	if _, _, err := bundleRun(src, dest); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("bundled: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("a refused bundle left a directory behind")
	}
}

// The first successful Azure mount soak left staged helper copies beside each client's steps and episode logs
// named with an upper-case episode id (mount-fresh-E1-60s.log); the bundle refused them as unknown. They are known
// controller-only files and are withheld, not exported.
func TestBundleWithholdsStagedHelpersAndEpisodeLogs(t *testing.T) {
	src := savedRun(t)
	extra := []string{"mount/client1/mount_soak.py", "mount/client1/mount_assess.py",
		"mount/client1/run-root/mount-fresh-E1-60s.log", "mount/client1/run-root/mount-fresh-E2-180s.log"}
	for _, rel := range extra {
		p := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.WriteFile(p, []byte("import json\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	files, withheld, err := bundleRun(src, filepath.Join(t.TempDir(), "bundle"))
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range extra {
		if !contains(withheld, rel) || contains(files, rel) {
			t.Fatalf("%s: exported %v withheld %v", rel, files, withheld)
		}
	}
	// Still unknown: a helper name the worker does not stage, and a log outside run-root.
	for _, rel := range []string{"mount/client1/other.py", "mount/client1/mount-fresh-E1-60s.log"} {
		t.Run(rel, func(t *testing.T) {
			src := savedRun(t)
			os.WriteFile(filepath.Join(src, filepath.FromSlash(rel)), []byte("x"), 0600)
			refused(t, src)
		})
	}
}

// A file on neither list is unknown: the whole bundle is refused, whatever it holds.
func TestBundleRefusesUnknownFiles(t *testing.T) {
	for _, rel := range []string{"evidence.json", "prepare/vol1/notes.txt", "mount/client1/run-root/extra.json", "prepare/vol7/cloud-step-001.json", "node-keys.json"} {
		t.Run(rel, func(t *testing.T) {
			src := savedRun(t)
			p := filepath.Join(src, filepath.FromSlash(rel))
			os.MkdirAll(filepath.Dir(p), 0700)
			os.WriteFile(p, []byte(`{}`), 0600)
			refused(t, src)
		})
	}
}

// An allowlisted file must be complete JSON (one value; a .jsonl one per line), with unique keys, no secret-named
// key, no key material, and every JSON-like string value admitted the same way.
func TestBundleRefusesAllowlistedFilesThatAreNotCleanJSON(t *testing.T) {
	encoded, _ := json.Marshal(`{"access\u005ftoken":"FAKE"}`)
	journal, _ := json.Marshal(map[string]string{"stdout": string(encoded)})
	step := "prepare/vol1/cloud-step-001.json"
	lines := "mount/client1/run-root/mount-journal.jsonl"
	for name, c := range map[string]struct{ rel, body string }{
		"pem":                 {step, `{"stdout":"` + strings.ReplaceAll(fakePEM, "\n", `\n`) + `"}`},
		"accessToken":         {step, `{"accessToken": "eyJ0eXAi"}`},
		"ed25519_private":     {step, `{"vol1":{"ed25519_private":"x"}}`},
		"client_secret":       {step, `{"client_secret" : "s"}`},
		"escaped key":         {step, `{"access\u005ftoken":"FAKE"}`},
		"unicode key":         {step, `{"\u0061ccessToken":"FAKE"}`},
		"stdout token":        {step, `{"stdout":"{\"accessToken\":\"FAKE\"}"}`},
		"double nested":       {step, `{"stdout":"{\"stdout\":\"{\\\"refresh_token\\\":\\\"FAKE\\\"}\"}"}`},
		"jsonl secret line":   {lines, "{\"ok\":1}\n{\"Client-Secret\":\"FAKE\"}\n"},
		"escaped in text":     {step, `{"stdout":"curl output: {\"idToken\": \"FAKE\"}"}`},
		"too deep":            {step, strings.Repeat("[", 40) + strings.Repeat("]", 40)},
		"duplicate stdout":    {step, `{"stdout":"{\"access\\u005ftoken\":\"FAKE\"}","stdout":"ordinary output"}`},
		"escaped duplicate":   {step, `{"stdout":"a","std\u006fut":"b"}`},
		"case duplicate":      {step, `{"stdout":"a","Stdout":"b"}`},
		"nested duplicate":    {step, `{"stdout":"{\"x\":1,\"x\":2}"}`},
		"trailing value":      {step, `{} {"access\u005ftoken":"FAKE"}`},
		"stdout trailing":     {step, `{"stdout":"{} {\"access\\u005ftoken\":\"FAKE\"}"}`},
		"encoded string":      {step, string(journal)},
		"not JSON":            {step, "Failed to write to /dev/console\n"},
		"empty":               {step, ""},
		"jsonl blank line":    {lines, "{\"e\":1}\n\n{\"e\":2}\n"},
		"jsonl trailing text": {lines, "{\"e\":1}\n{\"e\":2} tail\n"},
		"stdout broken JSON":  {step, `{"stdout":"{\"a\":"}`},
	} {
		t.Run(name, func(t *testing.T) {
			src := savedRun(t)
			if err := os.WriteFile(filepath.Join(src, filepath.FromSlash(c.rel)), []byte(c.body), 0600); err != nil {
				t.Fatal(err)
			}
			refused(t, src)
		})
	}
}
