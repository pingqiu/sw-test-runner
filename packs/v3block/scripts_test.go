package v3block

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Every script the description names is a file of scripts/ (and every such
// file is named), and its own unit tests (scripts/tests) pass.
func TestDeclaredScripts(t *testing.T) {
	var d any
	if err := json.Unmarshal(actionsJSON, &d); err != nil {
		t.Fatal(err)
	}
	named := map[string]bool{}
	var walk func(any)
	walk = func(o any) {
		switch o := o.(type) {
		case map[string]any:
			if p, ok := o["script"].(string); ok {
				named[p] = true
			}
			for _, v := range o {
				walk(v)
			}
		case []any:
			for _, v := range o {
				walk(v)
			}
		}
	}
	walk(d)
	files := declaredScripts()
	if len(named) != 4 || len(files) != len(named) {
		t.Fatalf("named %v, files %d", named, len(files))
	}
	for p := range named {
		if _, ok := files[p]; !ok {
			t.Errorf("%s is named and not a file of scripts/", p)
		}
	}
	// No inline program is left in the description.
	if strings.Contains(string(actionsJSON), "import ") {
		t.Error("an inline program is left in product/actions.json")
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		if py, err = exec.LookPath("python"); err != nil {
			t.Skip("no python: scripts/tests not run")
		}
	}
	cmd := exec.Command(py, "-m", "unittest", "discover", "-s", "scripts/tests")
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("scripts/tests: %v\n%s", err, out)
	}
}
