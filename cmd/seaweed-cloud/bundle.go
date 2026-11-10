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

// runBundle is `seaweed-cloud bundle --run ID --dest DIR`: the shareable evidence of a saved run. Only an explicit
// allowlist of the runner's own JSON artifacts is exported, by exact name; everything else (Terraform state, variables
// with the VMs' SSH host private keys, plans, raw logs and stdout copies) stays on the controller. An allowlisted file
// that is not complete JSON, or that holds a duplicate key or a secret, refuses the whole bundle, and nothing is
// written. Scanning arbitrary content for secrets cannot be made complete; the allowlist is the control.
func runBundle(args []string, w io.Writer) int {
	fs := flag.NewFlagSet("seaweed-cloud bundle", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	run := fs.String("run", "", "saved run ID under ~/.local/state/seaweed-cloud/runs")
	dest := fs.String("dest", "", "new absolute directory for the bundle")
	out := map[string]any{"schema": "seaweed.cloud-bundle/v2"}
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
	files, withheld, err := bundleRun(filepath.Join(home, ".local", "state", "seaweed-cloud", "runs", *run), *dest)
	out["run"], out["dest"], out["files"], out["withheld"] = *run, *dest, files, withheld
	if err != nil {
		return fail(err)
	}
	_ = json.NewEncoder(w).Encode(out)
	return 0
}

// bundleAllow is every file a bundle may carry, by exact name: the controller's call intents, the inventory and
// recovery record, each phase's per-host step records, and the mount soak's verdicts, steps and worker JSON.
var bundleAllow = []*regexp.Regexp{
	regexp.MustCompile(`^[0-9]{3}-(az|terraform)-intent\.json$`),
	regexp.MustCompile(`^(requested-inventory|recovery|mount-verdicts|cluster-result)\.json$`),
	regexp.MustCompile(`^(cloud-init|prepare|deploy|grow|before|after|collect|cleanup)/(meta|vol[1-6]|client[1-3])/(cloud-init-step|cloud-failure-fetch|cloud-postflight|status|cloud-step-[0-9]{3})\.json$`),
	regexp.MustCompile(`^mount/client[1-3]/(mount-step-[0-9]{3}|mount-verdict)\.json$`),
	regexp.MustCompile(`^mount/client[1-3]/run-root/(mount-journal\.jsonl|mount-(cohorts|faults|env|cleanup)\.json)$`),
}

// bundleWithhold is every file known to stay on the controller: raw call output and status text, apply markers, the
// Terraform working copy (state, variables with host keys, plans, provider cache), raw cloud-init stdout and service
// logs (their JSON step records carry the same output), and the worker's mount process logs. A file on neither list
// is unknown and refuses the whole bundle.
var bundleWithhold = []*regexp.Regexp{
	regexp.MustCompile(`^[0-9]{3}-(az|terraform)-(output|status)$`),
	regexp.MustCompile(`^apply-(started|succeeded)$`),
	regexp.MustCompile(`^provision/terraform/`),
	regexp.MustCompile(`^(cloud-init|prepare|deploy|grow|before|after|collect|cleanup)/(meta|vol[1-6]|client[1-3])/(cloud-init-status\.json|cloud-services\.log)$`),
	regexp.MustCompile(`^mount/client[1-3]/run-root/mount-[a-z0-9_-]+\.log$`),
}

func matches(list []*regexp.Regexp, rel string) bool {
	for _, re := range list {
		if re.MatchString(rel) {
			return true
		}
	}
	return false
}

// bundleRun admits every allowlisted file first and copies only if all are admitted; dest must not exist.
func bundleRun(src, dest string) (files, withheld []string, err error) {
	files, withheld = []string{}, []string{}
	bodies := map[string][]byte{}
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		rel, _ := filepath.Rel(src, p)
		rel = filepath.ToSlash(rel)
		switch {
		case rel == ".":
			return nil
		case d.IsDir() && rel == "provision/terraform":
			withheld = append(withheld, rel+"/")
			return filepath.SkipDir
		case d.IsDir():
			return nil
		case matches(bundleWithhold, rel):
			withheld = append(withheld, rel)
			return nil
		case !matches(bundleAllow, rel):
			return fmt.Errorf("refused: %s is not a known artifact; nothing was bundled", rel)
		case !d.Type().IsRegular():
			return fmt.Errorf("refused: %s is not a regular file", rel)
		}
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		if e = admitJSON(b, strings.HasSuffix(rel, ".jsonl")); e != nil {
			return fmt.Errorf("refused: %s: %v; nothing was bundled", rel, e)
		}
		bodies[rel] = b
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return nil, withheld, err
	}
	sort.Strings(files)
	if err = os.Mkdir(dest, 0700); err != nil {
		return nil, withheld, err
	}
	var sums bytes.Buffer
	for _, rel := range files {
		p := filepath.Join(dest, filepath.FromSlash(rel))
		if err = errors.Join(os.MkdirAll(filepath.Dir(p), 0700), os.WriteFile(p, bodies[rel], 0600)); err != nil {
			return nil, withheld, err
		}
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(bodies[rel]), rel)
	}
	return files, withheld, os.WriteFile(filepath.Join(dest, "SHA256SUMS"), sums.Bytes(), 0600)
}

// keyMaterial: a PEM private key, or a secret-named key with a value (escaped quotes too), in any raw text.
var keyMaterial = regexp.MustCompile(`PRIVATE KEY-----|\\*"(?i:access_?token|refresh_?token|id_?token|client_?secret|secret|password|ed25519_private|private_?key)\\*"\s*:`)

// secretNames are the decoded JSON keys refused, compared lower-case without '_', '-' and '.'.
var secretNames = map[string]bool{"accesstoken": true, "refreshtoken": true, "idtoken": true, "clientsecret": true, "secret": true, "password": true, "ed25519private": true, "privatekey": true}

// jsonDepth bounds nesting, counting JSON carried inside string values; deeper is refused.
const jsonDepth = 32

// admitJSON accepts exactly one complete JSON value (a .jsonl: one per line, each complete, no blank or trailing
// text) whose every key is unique and not secret-named and whose every string value is free of key material; a
// string value that looks like JSON (starts with '{', '[' or '"') must itself be admitted the same way.
func admitJSON(b []byte, lines bool) error {
	if keyMaterial.Match(b) {
		return errors.New("key material")
	}
	if !lines {
		return admitValue(b, 0)
	}
	body := bytes.TrimSuffix(b, []byte("\n"))
	for i, line := range bytes.Split(body, []byte("\n")) {
		if e := admitValue(line, 0); e != nil {
			return fmt.Errorf("line %d: %w", i+1, e)
		}
	}
	return nil
}

// admitValue: exactly one value, consumed to EOF.
func admitValue(b []byte, depth int) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if e := walkTokens(d, depth); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return errors.New("not exactly one complete JSON value")
	}
	return nil
}

func walkTokens(d *json.Decoder, depth int) error {
	if depth > jsonDepth {
		return errors.New("nested too deep")
	}
	tok, err := d.Token()
	if err != nil {
		return fmt.Errorf("not JSON: %w", err)
	}
	switch t := tok.(type) {
	case json.Delim:
		if t != '{' && t != '[' {
			return errors.New("unexpected delimiter")
		}
		seen := map[string]bool{}
		for d.More() {
			if t == '{' {
				k, err := d.Token()
				if err != nil {
					return fmt.Errorf("not JSON: %w", err)
				}
				key, _ := k.(string)
				lower := strings.ToLower(key)
				switch {
				case seen[lower]:
					return fmt.Errorf("duplicate key %q", key)
				case secretNames[strings.NewReplacer("_", "", "-", "", ".", "").Replace(lower)]:
					return fmt.Errorf("secret-named key %q", key)
				}
				seen[lower] = true
			}
			if e := walkTokens(d, depth+1); e != nil {
				return e
			}
		}
		if _, err = d.Token(); err != nil { // the closing delimiter
			return fmt.Errorf("not JSON: %w", err)
		}
	case string:
		if keyMaterial.MatchString(t) {
			return errors.New("key material in a string value")
		}
		if s := strings.TrimSpace(t); s != "" && strings.ContainsRune(`{["`, rune(s[0])) {
			if e := admitValue([]byte(s), depth+1); e != nil { // else complete JSON lines (a step's event stream)
				for i, line := range strings.Split(s, "\n") {
					if le := admitValue([]byte(line), depth+1); le != nil {
						return fmt.Errorf("JSON-like string value: %w (line %d: %v)", e, i+1, le)
					}
				}
			}
		}
	}
	return nil
}
