package v3block

// The citations of the block description, checked by a machine. Every cite
// in product/*.json names a file by its FULL repository path and a line or
// line range ("core/host/master/services.go:89-91", several per field). A
// later ":N" or ", N" refers to the same file. The test parses every cite
// and checks that each file exists and each range lies within it at the
// pinned product commit (read_at, the same in every file), through `git -C
// $PRODUCT_SRC show <pin>:<path>`. Paths under the runner's own directories
// (packs/, scenarios/, ...) are checked in this repository.
//
// PRODUCT_SRC is a product checkout that has the pinned commit. Without it
// the check is SKIPPED, loudly; CI sets it.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type citeRef struct {
	path   string
	lo, hi int
}

// citeFields are the keys whose values are citations.
var citeFields = map[string]bool{"cite": true, "side_effect_cite": true, "code_cite": true, "page_read": true,
	"pinned_by": true, "shared_source": true, "source": true, "page_cite": true}

var (
	citeToken = regexp.MustCompile(`(?P<path>[A-Za-z0-9_.\-/]*[A-Za-z0-9_\-]\.(?:go|md|sh|yaml|yml|json|proto))(?::(?P<r1>\d+(?:-\d+)?))?` +
		`|(?:^|[\s(]):(?P<r2>\d+(?:-\d+)?)` +
		`|,\s*(?P<r3>\d+(?:-\d+)?)\b`)
	citeAbbrev = regexp.MustCompile(`\b[A-Z]{2,4}:\d`)
	productPR  = regexp.MustCompile(`#\d+`)
)

// runnerDirs are this repository's own top directories: a cite under one
// names a runner file, not a product one.
var runnerDirs = []string{"packs/", "scenarios/", "checkers/", "declared/", "randomrun/"}

func parseRange(s string) (int, int, error) {
	lo, hi, found := strings.Cut(s, "-")
	a, err := strconv.Atoi(lo)
	if err != nil {
		return 0, 0, err
	}
	b := a
	if found {
		if b, err = strconv.Atoi(hi); err != nil {
			return 0, 0, err
		}
	}
	if a < 1 || b < a {
		return 0, 0, fmt.Errorf("range %s", s)
	}
	return a, b, nil
}

// parseCite reads one cite field: its references, or why it is malformed.
func parseCite(s string) ([]citeRef, error) {
	if m := citeAbbrev.FindString(s); m != "" {
		return nil, fmt.Errorf("an abbreviated file %q: write the full repository path", m)
	}
	var refs []citeRef
	path, lastEnd, lastWasRange := "", -1, false
	pathHasLine := true
	names := citeToken.SubexpNames()
	for _, m := range citeToken.FindAllStringSubmatchIndex(s, -1) {
		g := map[string]string{}
		for i, n := range names {
			if n != "" && m[2*i] >= 0 {
				g[n] = s[m[2*i]:m[2*i+1]]
			}
		}
		var r string
		switch {
		case g["path"] != "":
			if !pathHasLine {
				return nil, fmt.Errorf("%s names no line", path)
			}
			path = g["path"]
			if !strings.Contains(path, "/") {
				return nil, fmt.Errorf("%q is not a full repository path", path)
			}
			r, pathHasLine = g["r1"], g["r1"] != ""
		case g["r2"] != "":
			r = g["r2"]
		case g["r3"] != "":
			if !lastWasRange || m[0] != lastEnd {
				lastWasRange = false
				continue // a comma in prose
			}
			r = g["r3"]
		}
		lastEnd, lastWasRange = m[1], r != ""
		if r == "" {
			continue
		}
		if path == "" {
			return nil, fmt.Errorf("line %s with no file before it", r)
		}
		lo, hi, err := parseRange(r)
		if err != nil {
			return nil, err
		}
		refs = append(refs, citeRef{path, lo, hi})
		pathHasLine = true
	}
	if !pathHasLine {
		return nil, fmt.Errorf("%s names no line", path)
	}
	if len(refs) == 0 && !productPR.MatchString(s) {
		return nil, fmt.Errorf("cites no file (and names no product change #n)")
	}
	return refs, nil
}

// descriptionCites reads every cite field of product/*.json, by file and
// JSON path, and each file's read_at.
func descriptionCites(t *testing.T) (map[string]string, map[string]string) {
	t.Helper()
	files, _ := filepath.Glob("product/*.json")
	cites, readAt := map[string]string{}, map[string]string{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var d any
		if err := json.Unmarshal(b, &d); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		n := 0
		var walk func(o any, path string)
		walk = func(o any, path string) {
			switch o := o.(type) {
			case map[string]any:
				for k, v := range o {
					if s, ok := v.(string); ok && citeFields[k] && !strings.Contains(path, "/record/args") {
						cites[f+" "+path+"/"+k] = s
						n++
					}
					walk(v, path+"/"+k)
				}
			case []any:
				for i, v := range o {
					walk(v, fmt.Sprintf("%s/%d", path, i))
				}
			}
		}
		walk(d, "")
		if top, ok := d.(map[string]any); ok {
			if r, ok := top["read_at"].(string); ok {
				readAt[f] = r
			} else if n > 0 {
				t.Errorf("%s has %d cites and no read_at", f, n)
			}
		}
	}
	return cites, readAt
}

// productPin is the commit every read_at names.
func productPin(t *testing.T, readAt map[string]string) string {
	t.Helper()
	pins := map[string]bool{}
	for f, r := range readAt {
		sha, ok := strings.CutPrefix(r, "seaweed-block ")
		if !ok || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(sha) {
			t.Fatalf("%s: read_at %q is not \"seaweed-block <40-hex commit>\"", f, r)
		}
		pins[sha] = true
	}
	if len(pins) != 1 {
		t.Fatalf("the description files are read at different commits: %v", readAt)
	}
	for p := range pins {
		return p
	}
	return ""
}

func TestCitationsParse(t *testing.T) {
	refs, err := parseCite("core/host/master/services.go:89-91; services in prose, core/lifecycle/store.go:140-148, :175-176, 179 (before :182); docs/x.md:3,5")
	want := []citeRef{{"core/host/master/services.go", 89, 91}, {"core/lifecycle/store.go", 140, 148}, {"core/lifecycle/store.go", 175, 176},
		{"core/lifecycle/store.go", 179, 179}, {"core/lifecycle/store.go", 182, 182}, {"docs/x.md", 3, 3}, {"docs/x.md", 5, 5}}
	if err != nil || fmt.Sprint(refs) != fmt.Sprint(want) {
		t.Fatalf("%v %v", refs, err)
	}
	if refs, err := parseCite("a proposed page (product change #662)"); err != nil || len(refs) != 0 {
		t.Fatalf("a product change: %v %v", refs, err)
	}
	// Red controls.
	for name, s := range map[string]string{
		"a bare file name":           "delete.go:250",
		"an abbreviated page":        "VL:410",
		"a file without a line":      "docs/operator-guide.md, docs/volume-lifecycle.md:224",
		"a line before any file":     "see :12",
		"no file at all":             "the product says so",
		"a range that runs backward": "core/a/b.go:9-3",
	} {
		if _, err := parseCite(s); err == nil {
			t.Errorf("%s (%q): parsed", name, s)
		}
	}
	// Every cite of the description parses.
	cites, readAt := descriptionCites(t)
	if len(cites) < 50 {
		t.Fatalf("only %d cites found", len(cites))
	}
	productPin(t, readAt)
	keys := make([]string, 0, len(cites))
	for k := range cites {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, err := parseCite(cites[k]); err != nil {
			t.Errorf("%s: %v\n  %s", k, err, cites[k])
		}
	}
}

// lineCounter counts the lines of a file at the pin (product) or in this
// repository (runner paths); -1 when the file does not exist.
type lineCounter struct {
	src, pin, root string
	cache          map[string]int
}

func (c *lineCounter) lines(path string) int {
	if n, ok := c.cache[path]; ok {
		return n
	}
	var b []byte
	var err error
	runner := false
	for _, d := range runnerDirs {
		runner = runner || strings.HasPrefix(path, d)
	}
	if runner {
		b, err = os.ReadFile(filepath.Join(c.root, filepath.FromSlash(path)))
	} else {
		b, err = exec.Command("git", "-C", c.src, "show", c.pin+":"+path).Output()
	}
	n := -1
	if err == nil {
		n = strings.Count(string(b), "\n")
		if len(b) > 0 && b[len(b)-1] != '\n' {
			n++
		}
	}
	c.cache[path] = n
	return n
}

func (c *lineCounter) check(refs []citeRef) error {
	for _, r := range refs {
		n := c.lines(r.path)
		switch {
		case n < 0:
			return fmt.Errorf("%s does not exist at %s", r.path, c.pin[:12])
		case r.hi > n:
			return fmt.Errorf("%s:%d-%d is past its end (%d lines at %s)", r.path, r.lo, r.hi, n, c.pin[:12])
		}
	}
	return nil
}

func TestCitationsAtThePin(t *testing.T) {
	src := os.Getenv("PRODUCT_SRC")
	if src == "" {
		t.Skip("PRODUCT_SRC is NOT SET: the description's citations are NOT checked against the product. " +
			"Set PRODUCT_SRC to a seaweed-block checkout that has the read_at commit (CI does).")
	}
	cites, readAt := descriptionCites(t)
	pin := productPin(t, readAt)
	if err := exec.Command("git", "-C", src, "cat-file", "-e", pin+"^{commit}").Run(); err != nil {
		t.Fatalf("PRODUCT_SRC %s does not have the pinned commit %s: %v", src, pin, err)
	}
	root, _ := filepath.Abs("../..")
	c := &lineCounter{src: src, pin: pin, root: root, cache: map[string]int{}}
	keys := make([]string, 0, len(cites))
	for k := range cites {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	checked := 0
	for _, k := range keys {
		refs, err := parseCite(cites[k])
		if err == nil {
			err = c.check(refs)
		}
		if err != nil {
			t.Errorf("%s: %v\n  %s", k, err, cites[k])
		}
		checked += len(refs)
	}
	t.Logf("%d references in %d cite fields checked at %s", checked, len(cites), pin)
	// Red controls: a missing file and a line past a file's end are caught.
	for name, s := range map[string]string{
		"a missing file":         "core/host/master/no_such_file.go:1",
		"a line past the end":    "core/host/master/services.go:99999",
		"a missing runner file":  "packs/v3block/no_such_file.go:1",
		"a runner line past end": "packs/v3block/ops.go:99999",
	} {
		refs, err := parseCite(s)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.check(refs) == nil {
			t.Errorf("red control %s (%q): not caught", name, s)
		}
	}
}
