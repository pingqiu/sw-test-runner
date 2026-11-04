//go:build linux

package testrunner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultBuildProducesSeaweedCloud(t *testing.T) {
	root := t.TempDir()
	fake := filepath.Join(root, "fake-bin")
	os.Mkdir(fake, 0700)
	os.WriteFile(filepath.Join(fake, "go"), []byte(`#!/bin/sh
while [ "$#" -gt 0 ]; do if [ "$1" = -o ]; then shift; touch "$1"; exit; fi; shift; done
exit 1
`), 0700)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	outdir := filepath.Join(root, "bin")
	cmd := exec.CommandContext(ctx, "make", "build", "BIN="+outdir)
	cmd.Env = append(os.Environ(), "PATH="+fake+":"+os.Getenv("PATH"))
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("make build: %v %s", e, out)
	}
	if _, e := os.Stat(filepath.Join(outdir, "seaweed-cloud")); e != nil {
		t.Fatal("default build omitted direct entry", e)
	}
	cmd = exec.CommandContext(ctx, "make", "build-sweedrdma", "BIN="+outdir)
	cmd.Env = append(os.Environ(), "PATH="+fake+":"+os.Getenv("PATH"))
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("manual build regression: %v %s", e, out)
	}
	if _, e := os.Stat(filepath.Join(outdir, "sweedrdma")); e != nil {
		t.Fatal("ad-hoc target lost", e)
	}
}
