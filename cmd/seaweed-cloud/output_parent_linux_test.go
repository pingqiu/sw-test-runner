package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/pingqiu/sw-test-runner/provision/smoke"
)

func TestDryRunRefusesUnusableOutputParent(t *testing.T) {
	for _, kind := range []string{"missing", "file", "readonly-sysfs"} {
		t.Run(kind, func(t *testing.T) {
			cp, calls := dryRunFixture(t)
			body, _ := os.ReadFile(cp)
			var c smoke.Config
			if err := json.Unmarshal(body, &c); err != nil {
				t.Fatal(err)
			}
			parent := filepath.Join(filepath.Dir(cp), "runs")
			if kind == "file" {
				if err := os.WriteFile(parent, []byte("not a directory"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "readonly-sysfs" {
				if info, err := os.Stat("/sys"); err != nil || !info.IsDir() {
					t.Skip("sysfs absent")
				}
				parent = "/sys"
			}
			c.Output = filepath.Join(parent, "test-run")
			body, _ = json.Marshal(c)
			if err := os.WriteFile(cp, body, 0600); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			rc := run([]string{"run", "--dry-run", "--config", cp, "--json"}, &output)
			var got smoke.Result
			if json.Unmarshal(output.Bytes(), &got) != nil || rc == 0 || got.Code != "OUTPUT_PARENT_REFUSED" || got.Remedy == "" || len(got.Effects) != 0 {
				t.Fatalf("rc=%d result=%s", rc, output.String())
			}
			if b, _ := os.ReadFile(calls); len(b) != 0 {
				t.Fatalf("preflight issued Native calls: %s", b)
			}
			if _, err := os.Stat(c.Output); !os.IsNotExist(err) && !(kind == "file" && errors.Is(err, syscall.ENOTDIR)) {
				t.Fatalf("created run output: %v", err)
			}
			if kind == "missing" {
				if _, err := os.Stat(parent); !os.IsNotExist(err) {
					t.Fatalf("created missing parent: %v", err)
				}
			}
		})
	}
}

func TestDryRunWritableParentLeavesNoRunOrProbe(t *testing.T) {
	cp, calls := dryRunFixture(t)
	parent := filepath.Dir(cp)
	before, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if rc := run([]string{"run", "--dry-run", "--config", cp, "--json"}, &output); rc != 0 {
		t.Fatalf("rc=%d %s", rc, output.String())
	}
	after, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatal("dry run left probe or run output")
	}
	for i := range before {
		if before[i].Name() != after[i].Name() {
			t.Fatal("parent contents changed")
		}
	}
	if b, _ := os.ReadFile(calls); len(b) != 0 {
		t.Fatalf("unexpected Native calls: %s", b)
	}
}
