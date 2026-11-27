package randomrun

// The import guard of the platform seam: the generic machine imports no
// product pack, and no package outside the block pack itself and the
// commands imports the block pack.

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/pingqiu/sw-test-runner"

// moduleRoot is the directory holding go.mod above this package.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		up := filepath.Dir(dir)
		if up == dir {
			t.Fatal("no go.mod above the package")
		}
		dir = up
	}
}

// imports maps every Go file under root (tests included; testdata, hidden
// and underscore directories skipped, as the go tool skips them) to its
// import paths. Paths are slash-separated and relative to root.
func imports(t *testing.T, root string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			n := d.Name()
			if p != root && (n == "testdata" || n == "vendor" || strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		for _, im := range f.Imports {
			path, _ := strconv.Unquote(im.Path.Value)
			out[rel] = append(out[rel], path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// violations applies the two rules to a file -> imports map.
func violations(files map[string][]string) []string {
	var bad []string
	for file, ims := range files {
		for _, im := range ims {
			if (strings.HasPrefix(file, "randomrun/") || strings.HasPrefix(file, "declared/")) && strings.HasPrefix(im, module+"/packs/") {
				bad = append(bad, file+" imports "+im+" (the generic machine imports no pack)")
			}
			if (im == module+"/packs/v3block" || strings.HasPrefix(im, module+"/packs/v3block/")) &&
				!strings.HasPrefix(file, "packs/v3block/") && !strings.HasPrefix(file, "cmd/") {
				bad = append(bad, file+" imports "+im+" (only the block pack and the commands may)")
			}
		}
	}
	return bad
}

func TestImportGuard(t *testing.T) {
	files := imports(t, moduleRoot(t))
	if len(files["randomrun/model.go"]) == 0 && len(files["randomrun/run.go"]) == 0 {
		t.Fatal("the walk did not reach this package")
	}
	if len(files["declared/declared.go"]) == 0 {
		t.Fatal("the walk did not reach the declared-action executor")
	}
	if bad := violations(files); len(bad) != 0 {
		t.Fatalf("import guard:\n%s", strings.Join(bad, "\n"))
	}
}

// Red controls: each seeded import must be caught; its green twin passes.
func TestImportGuardRedControls(t *testing.T) {
	for name, c := range map[string]struct{ red, green map[string][]string }{
		"the generic machine imports the block pack": {
			red:   map[string][]string{"randomrun/run.go": {module + "/packs/v3block"}},
			green: map[string][]string{"randomrun/run.go": {module}},
		},
		"the declared-action executor imports the block pack": {
			red:   map[string][]string{"declared/declared.go": {module + "/packs/v3block"}},
			green: map[string][]string{"declared/declared.go": {module}},
		},
		"the generic machine imports another pack": {
			red:   map[string][]string{"randomrun/x_test.go": {module + "/packs/seaweed"}},
			green: map[string][]string{"randomrun/x_test.go": {"testing"}},
		},
		"a checker-side package imports the block pack": {
			red:   map[string][]string{"internal/x/x.go": {module + "/packs/v3block"}},
			green: map[string][]string{"cmd/m1-nightly/main.go": {module + "/packs/v3block"}},
		},
		"the root package imports a block subpackage": {
			red:   map[string][]string{"engine.go": {module + "/packs/v3block/v3ctl"}},
			green: map[string][]string{"packs/v3block/v3ctl/main.go": {module + "/packs/v3block"}},
		},
	} {
		if len(violations(c.red)) == 0 {
			t.Errorf("%s: red control not caught", name)
		}
		if bad := violations(c.green); len(bad) != 0 {
			t.Errorf("%s: green twin rejected: %v", name, bad)
		}
	}
}
