package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// runBundle is `seaweed-cloud bundle --run ID --dest DIR`: the shareable evidence of a saved run. Terraform state,
// variables and plans never leave the controller (host.auto.tfvars.json carries the VMs' SSH host private keys);
// any other file holding key or token material refuses the whole bundle, and nothing is written.
func runBundle(args []string, w io.Writer) int {
	fs := flag.NewFlagSet("seaweed-cloud bundle", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	run := fs.String("run", "", "saved run ID under ~/.local/state/seaweed-cloud/runs")
	dest := fs.String("dest", "", "new absolute directory for the bundle")
	out := map[string]any{"schema": "seaweed.cloud-bundle/v1"}
	fail := func(e error) int {
		out["error"] = e.Error()
		_ = json.NewEncoder(w).Encode(out)
		return 1
	}
	if fs.Parse(args) != nil || fs.NArg() != 0 || !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,49}$`).MatchString(*run) || !filepath.IsAbs(*dest) {
		return fail(errors.New("use: bundle --run ID --dest /absolute/new/dir"))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(err)
	}
	files, excluded, err := bundleRun(filepath.Join(home, ".local", "state", "seaweed-cloud", "runs", *run), *dest)
	out["run"], out["dest"], out["files"], out["excluded"] = *run, *dest, files, excluded
	if err != nil {
		return fail(err)
	}
	_ = json.NewEncoder(w).Encode(out)
	return 0
}

// keyMaterial is what must never be in a shared bundle.
// A Terraform schema may name these fields (`ed25519_private = string`); only a value is refused.
var keyMaterial = regexp.MustCompile(`PRIVATE KEY-----|"(?i:access_?token|refresh_?token|client_?secret|password|ed25519_private)"\s*:`)

// withheld names the controller-only files: Terraform state, variables, plans and the provider cache.
func withheld(rel string, dir bool) bool {
	base := filepath.Base(rel)
	if dir {
		return base == ".terraform"
	}
	return strings.Contains(base, ".tfstate") || strings.Contains(base, ".tfvars") || strings.HasSuffix(base, ".tfplan") || base == ".terraform.lock.hcl.bak"
}

// bundleRun scans src first and copies only if every remaining file is clean; dest must not exist.
func bundleRun(src, dest string) (files, excluded []string, err error) {
	files, excluded = []string{}, []string{}
	bodies := map[string][]byte{}
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		rel, _ := filepath.Rel(src, p)
		switch {
		case rel == ".":
			return nil
		case withheld(rel, d.IsDir()):
			excluded = append(excluded, filepath.ToSlash(rel))
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		case d.IsDir():
			return nil
		case !d.Type().IsRegular():
			return fmt.Errorf("refused: %s is not a regular file", rel)
		}
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		if keyMaterial.Match(b) {
			return fmt.Errorf("refused: %s holds key or token material; nothing was bundled", filepath.ToSlash(rel))
		}
		bodies[filepath.ToSlash(rel)] = b
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, excluded, err
	}
	sort.Strings(files)
	if err = os.Mkdir(dest, 0700); err != nil {
		return nil, excluded, err
	}
	var sums bytes.Buffer
	for _, rel := range files {
		p := filepath.Join(dest, filepath.FromSlash(rel))
		if err = errors.Join(os.MkdirAll(filepath.Dir(p), 0700), os.WriteFile(p, bodies[rel], 0600)); err != nil {
			return nil, excluded, err
		}
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(bodies[rel]), rel)
	}
	return files, excluded, os.WriteFile(filepath.Join(dest, "SHA256SUMS"), sums.Bytes(), 0600)
}
