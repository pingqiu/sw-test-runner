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
// A Terraform schema may name these fields (`ed25519_private = string`); only a key with a value is refused. The raw
// pattern also matches quotes escaped inside text (`\"accessToken\":`); holdsSecret decodes JSON for the rest.
var keyMaterial = regexp.MustCompile(`PRIVATE KEY-----|\\*"(?i:access_?token|refresh_?token|id_?token|client_?secret|secret|password|ed25519_private|private_?key)\\*"\s*:`)

// secretNames are the decoded JSON keys refused, compared lower-case without '_', '-' and '.'.
var secretNames = map[string]bool{"accesstoken": true, "refreshtoken": true, "idtoken": true, "clientsecret": true, "secret": true, "password": true, "ed25519private": true, "privatekey": true}

// jsonDepth bounds the walk, including JSON decoded from string values (a journal's stdout); deeper is refused.
const jsonDepth = 32

// holdsSecret refuses raw key material, and any JSON key or string value that is or holds a secret: whole-file JSON,
// else each JSON line, with string values that are themselves JSON walked in turn (escapes, \u sequences and nested
// stdout all decode to the same keys). The walk reads tokens, so it sees every pair as written: a duplicate key
// (also a case alias) is refused outright, since a decoder would keep only one of the values the bundle exports.
func holdsSecret(b []byte) bool {
	if keyMaterial.Match(b) {
		return true
	}
	if valid, refuse := jsonSecret(b, 0); valid {
		return refuse
	}
	for _, line := range bytes.Split(b, []byte("\n")) {
		if _, refuse := jsonSecret(line, 0); refuse {
			return true
		}
	}
	return false
}

// jsonSecret walks one JSON text token by token. valid is false when b is not exactly one JSON value; refuse is true on
// a secret, a duplicate key, or depth beyond jsonDepth.
func jsonSecret(b []byte, depth int) (valid, refuse bool) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	refuse, err := walkTokens(d, depth)
	if refuse {
		return true, true
	}
	if err != nil {
		return false, false
	}
	if _, err = d.Token(); err != io.EOF {
		return false, false
	}
	return true, false
}

func walkTokens(d *json.Decoder, depth int) (bool, error) {
	if depth > jsonDepth {
		return true, nil
	}
	tok, err := d.Token()
	if err != nil {
		return false, err
	}
	switch t := tok.(type) {
	case json.Delim:
		object := t == '{'
		seen := map[string]bool{}
		for d.More() {
			if object {
				k, err := d.Token()
				if err != nil {
					return false, err
				}
				key, _ := k.(string)
				lower := strings.ToLower(key)
				if seen[lower] || secretNames[strings.NewReplacer("_", "", "-", "", ".", "").Replace(lower)] {
					return true, nil
				}
				seen[lower] = true
			}
			if refuse, err := walkTokens(d, depth+1); refuse || err != nil {
				return refuse, err
			}
		}
		_, err = d.Token() // the closing delimiter
		return false, err
	case string:
		return stringSecret(t, depth), nil
	}
	return false, nil
}

// stringSecret checks a string value raw, then as JSON (whole, else per line) when it looks like JSON.
func stringSecret(s string, depth int) bool {
	if keyMaterial.MatchString(s) {
		return true
	}
	if t := strings.TrimSpace(s); len(t) < 2 || (t[0] != '{' && t[0] != '[') {
		return false
	}
	if valid, refuse := jsonSecret([]byte(s), depth+1); valid {
		return refuse
	}
	for _, line := range strings.Split(s, "\n") {
		if _, refuse := jsonSecret([]byte(line), depth+1); refuse {
			return true
		}
	}
	return false
}

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
		if holdsSecret(b) {
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
