package declared

import (
	"context"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
)

const scriptDesc = head + `
 "actions": [
  {"action": "p_run", "verb": "run", "op": "Svc.Run", "runs_on": "step_node",
   "params": [{"name": "volume", "kind": "subject", "vars": true}],
   "record": {"volume": "{{volume.name}}", "args": {}},
   "variants": [{"command": ["python3", "-c", {"script": "packs/p/scripts/x.py"}, "{{volume.name}}"], "runner_deadline_seconds": 5, "cite": "x.go:1",
     "decode": {"kind": "error_body", "path": "run"}}]}
 ]}`

// A script word is the script file's text without its final newline, one
// shell-quoted word; a description naming a script that is not supplied,
// or whose text has a carriage return or no final newline, does not load.
func TestScriptWord(t *testing.T) {
	prog := "import sys\nprint(sys.argv[1])"
	f, err := ParseWith([]byte(scriptDesc), map[string]string{"packs/p/scripts/x.py": prog + "\n"})
	if err != nil {
		t.Fatal(err)
	}
	n := &node{answers: []answer{{}}}
	x := &Executor{File: f, Lookup: lookup,
		Node: func(*tr.ActionContext, string) (Runner, error) { return &SimHost{Inner: n}, nil },
		Subject: func(_ *tr.ActionContext, name string) (Subject, error) {
			return &subject{f: map[string]string{"name": name}}, nil
		}}
	_, actx := setup(t, n, &subject{f: map[string]string{}})
	if _, err := x.Handler("p_run")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1"}}); err != nil {
		t.Fatal(err)
	}
	if want := "python3 -c " + ShellQuote(prog) + " v1"; n.calls[0] != want {
		t.Fatalf("command %q, want %q", n.calls[0], want)
	}
	for name, scripts := range map[string]map[string]string{
		"not supplied":      {},
		"a carriage return": {"packs/p/scripts/x.py": "import sys\r\nprint(1)\n"},
		"no final newline":  {"packs/p/scripts/x.py": prog},
		"plain Parse, none": nil,
	} {
		if _, err := ParseWith([]byte(scriptDesc), scripts); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
	if _, err := Parse([]byte(strings.Replace(scriptDesc, `{"script": "packs/p/scripts/x.py"}`, `{"script": "packs/p/scripts/x.py", "raw": "&&"}`, 1))); err == nil {
		t.Error("a script word with another part loaded")
	}
}
