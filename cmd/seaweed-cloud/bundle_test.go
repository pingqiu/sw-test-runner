package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A saved run as the first Azure attempt left it: journal, prepare evidence, recovery config, and the Terraform
// directory whose host.auto.tfvars.json holds the VMs' SSH host private keys.
func savedRun(t *testing.T) string {
	src := t.TempDir()
	for p, body := range map[string]string{
		"001-az-intent.json":                                            `["resource","list"]`,
		"prepare/vol1/cloud-init-step.json":                             `{"exit_code":124}`,
		"recovery.json":                                                 `{"output":"/root/.local/state/seaweed-cloud/runs/x"}`,
		"provision/terraform/azure-validation/main.tf":                  `resource "x" "y" {}`,
		"provision/terraform/azure-validation/host.auto.tfvars.json":    `{"node_host_keys":{"vol1":{"ed25519_private":"-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----\n"}}}`,
		"provision/terraform/azure-validation/run.auto.tfvars.json":     `{"operator_cidr":"192.0.2.1/32"}`,
		"provision/terraform/azure-validation/terraform.tfstate":        `{"resources":[]}`,
		"provision/terraform/azure-validation/terraform.tfstate.backup": `{}`,
		"provision/terraform/azure-validation/smoke.tfplan":             `plan`,
		"provision/terraform/azure-validation/.terraform/providers/p":   `binary PRIVATE KEY----- strings`,
	} {
		p = filepath.Join(src, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return src
}

func TestBundleWithholdsTerraformVariablesStateAndKeys(t *testing.T) {
	src := savedRun(t)
	dest := filepath.Join(t.TempDir(), "bundle")
	files, excluded, err := bundleRun(src, dest)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.Contains(f, ".tfvars") || strings.Contains(f, ".tfstate") || strings.Contains(f, ".terraform/") || strings.HasSuffix(f, ".tfplan") {
			t.Fatalf("bundled a controller-only file: %s", f)
		}
	}
	if len(files) != 4 || len(excluded) != 6 {
		t.Fatalf("files %v excluded %v", files, excluded)
	}
	_ = filepath.WalkDir(dest, func(p string, d os.DirEntry, e error) error {
		if e == nil && !d.IsDir() {
			if b, _ := os.ReadFile(p); keyMaterial.Match(b) || strings.Contains(p, "tfvars") {
				t.Fatalf("key material in the bundle: %s", p)
			}
		}
		return nil
	})
	if _, err := os.Stat(filepath.Join(dest, "SHA256SUMS")); err != nil {
		t.Fatal(err)
	}
}

// Key or token material anywhere else refuses the whole bundle, and nothing is written.
func TestBundleRefusesKeyMaterialOutsideTheWithheldFiles(t *testing.T) {
	for name, body := range map[string]string{
		"prepare/vol1/stray.json": "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----\n",
		"collect/meta/env.json":   `{"accessToken": "eyJ0eXAi"}`,
		"node-keys.json":          `{"vol1":{"ed25519_private":"x"}}`,
		"az/secret.json":          `{"client_secret" : "s"}`,
	} {
		t.Run(name, func(t *testing.T) {
			src := savedRun(t)
			p := filepath.Join(src, filepath.FromSlash(name))
			os.MkdirAll(filepath.Dir(p), 0700)
			os.WriteFile(p, []byte(body), 0600)
			dest := filepath.Join(t.TempDir(), "bundle")
			if _, _, err := bundleRun(src, dest); err == nil || !strings.Contains(err.Error(), "refused") {
				t.Fatalf("key material was bundled: %v", err)
			}
			if _, err := os.Stat(dest); !os.IsNotExist(err) {
				t.Fatal("a refused bundle left a directory behind")
			}
		})
	}
}
