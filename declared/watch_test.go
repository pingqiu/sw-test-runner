package declared

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

// gate holds every command until release, then answers code.
type gate struct {
	release chan struct{}
	code    int
}

func (g *gate) Run(ctx context.Context, cmd string) (string, string, int, error) {
	<-g.release
	return "", "", g.code, nil
}

func (g *gate) RunRoot(ctx context.Context, cmd string) (string, string, int, error) {
	return g.Run(ctx, cmd)
}

func watchSetup(t *testing.T, host Runner) (*Executor, *tr.ActionContext, string) {
	t.Helper()
	f, err := Parse([]byte(desc))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "h.jsonl")
	h, err := tr.OpenHistory(p)
	if err != nil {
		t.Fatal(err)
	}
	x := &Executor{File: f, Lookup: lookup, Poll: 5 * time.Millisecond,
		Deadline: func(time.Duration) time.Duration { return 100 * time.Millisecond },
		Node:     func(*tr.ActionContext, string) (Runner, error) { return host, nil },
		Subject: func(*tr.ActionContext, string) (Subject, error) {
			return &subject{f: map[string]string{"name": "v1"}}, nil
		}}
	return x, &tr.ActionContext{History: h, Vars: map[string]string{"bin": "/b"}, Log: func(string, ...interface{}) {}}, p
}

func historyOf(t *testing.T, p string) []tr.HistoryRecord {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var out []tr.HistoryRecord
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var r tr.HistoryRecord
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func childExits(rs []tr.HistoryRecord) []tr.HistoryRecord {
	var out []tr.HistoryRecord
	for _, r := range rs {
		if r.Verb == "child_exit" {
			out = append(out, r)
		}
	}
	return out
}

// An abandoned child that later exits is recorded at the next operation
// boundary, after the abandoned operation, with its outcome decoded from
// its exit: 0 ok, another failed, the runner's SIGKILL unknown.
func TestAbandonedChildExitIsRecorded(t *testing.T) {
	for code, want := range map[int]string{0: tr.OutcomeOK, 3: "failed", 137: tr.OutcomeUnknown} {
		g := &gate{release: make(chan struct{}), code: code}
		host := &SimHost{Inner: g, Unkillable: true}
		x, actx, p := watchSetup(t, host)
		put := tr.Action{Params: map[string]string{"volume": "v1", "key": "k"}}
		if out, err := x.Handler("p_put")(context.Background(), actx, put); err != nil || out["value"] != tr.OutcomeUnknown {
			t.Fatalf("%d: abandoned op %v %v", code, out, err)
		}
		if Watched(actx.History) != 1 {
			t.Fatalf("%d: %d watched, want 1", code, Watched(actx.History))
		}
		// Still running: the next boundary records nothing for it.
		if err := Check(context.Background(), host, actx.History, false); err != nil || Watched(actx.History) != 1 {
			t.Fatalf("%d: alive child: %v, %d watched", code, err, Watched(actx.History))
		}
		close(g.release)
		time.Sleep(20 * time.Millisecond)
		if _, err := x.Handler("p_put")(context.Background(), actx, put); err != nil {
			t.Fatal(err)
		}
		actx.History.Close()
		rs := historyOf(t, p)
		ce := childExits(rs)
		if len(ce) != 1 || Watched(actx.History) != 0 {
			t.Fatalf("%d: child_exit records %+v", code, ce)
		}
		r := ce[0]
		if r.Client != "runner" || r.Volume != "v1" || r.Result["outcome"] != want || r.Result["op_seq"] != float64(1) ||
			r.Result["pid"] != "40002" || r.Result["start"] != "777" || r.Result["exit"] != float64(code) || r.Seq <= 2 {
			t.Fatalf("%d: %+v", code, r)
		}
		if code == 137 && r.Result["killed"] != true {
			t.Fatalf("137 not marked killed: %+v", r)
		}
		// The record comes before the next operation's invocation.
		if rs[2].Verb != "child_exit" || rs[3].Kind != tr.HistoryInvoke {
			t.Fatalf("%d: order %+v", code, rs)
		}
	}
}

// A child still alive at the run's end is recorded unknown, alive true.
func TestAbandonedChildAliveAtTheEnd(t *testing.T) {
	g := &gate{release: make(chan struct{})}
	defer close(g.release)
	host := &SimHost{Inner: g, Unkillable: true}
	x, actx, p := watchSetup(t, host)
	if _, err := x.Handler("p_put")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "key": "k"}}); err != nil {
		t.Fatal(err)
	}
	if err := Check(context.Background(), host, actx.History, true); err != nil {
		t.Fatal(err)
	}
	actx.History.Close()
	ce := childExits(historyOf(t, p))
	if len(ce) != 1 || ce[0].Result["outcome"] != tr.OutcomeUnknown || ce[0].Result["alive"] != true || ce[0].Result["exit"] != nil {
		t.Fatalf("%+v", ce)
	}
	if Watched(actx.History) != 0 {
		t.Fatal("still watched after the end")
	}
	// No node at the end: recorded unknown, alive null.
	x, actx, p = watchSetup(t, host)
	_, _ = x.Handler("p_put")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "key": "k"}})
	_ = Check(context.Background(), nil, actx.History, true)
	actx.History.Close()
	if ce := childExits(historyOf(t, p)); len(ce) != 1 || ce[0].Result["alive"] != nil {
		t.Fatalf("no node: %+v", ce)
	}
}

// A run without abandoned children issues no watch command.
func TestNoWatchWithoutAbandoned(t *testing.T) {
	n := &node{answers: []answer{{}}}
	x, actx := setup(t, n, &subject{f: map[string]string{}})
	for i := 0; i < 2; i++ {
		_, _ = x.Handler("p_put")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "key": "k"}})
	}
	for _, c := range n.calls {
		if strings.Contains(c, "runner-io watch") {
			t.Fatalf("a watch command without abandoned children: %q", c)
		}
	}
}

// Abandon without an operation to record against says so; with one, the
// children are watched.
func TestAbandonRegisters(t *testing.T) {
	d := &Detached{Deadline: time.Second, Abandoned: []Proc{{PID: "7", Start: "1"}}, Dir: "/io/x"}
	if res := Abandon(context.Background(), d); res["unwatched"] != true {
		t.Fatalf("no op ref: %v", res)
	}
	h, _ := tr.OpenHistory(filepath.Join(t.TempDir(), "h.jsonl"))
	defer h.Close()
	res := Abandon(WithOp(context.Background(), OpRef{History: h, Seq: 3, Volume: "v"}), d)
	if res["unwatched"] != nil || Watched(h) != 1 {
		t.Fatalf("with op ref: %v, %d watched", res, Watched(h))
	}
}

// abandonedLiterals lists the non-test Go files under root, other than this
// package's watch.go, that write the "abandoned" result field themselves.
func abandonedLiterals(files map[string][]string) []string {
	var bad []string
	for f, lits := range files {
		if f == "declared/watch.go" || strings.HasSuffix(f, "_test.go") {
			continue
		}
		for _, l := range lits {
			if l == `"abandoned"` {
				bad = append(bad, f)
			}
		}
	}
	return bad
}

// The abandoned list is written only by Abandon, which registers the
// children with the watcher. Red control: a seeded writer is caught.
func TestAbandonedOnlyThroughTheWatcher(t *testing.T) {
	root, _ := filepath.Abs("..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skip("not run from the module (a copied test binary)")
	}
	files := map[string][]string{}
	fset := token.NewFileSet()
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && p != root && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		ast.Inspect(f, func(n ast.Node) bool {
			if b, ok := n.(*ast.BasicLit); ok && b.Kind == token.STRING {
				files[rel] = append(files[rel], b.Value)
			}
			return true
		})
		return nil
	})
	if len(files["declared/watch.go"]) == 0 {
		t.Fatal("the walk did not reach the watcher")
	}
	if bad := abandonedLiterals(files); len(bad) != 0 {
		t.Fatalf("writes the abandoned list without the watcher: %v", bad)
	}
	if bad := abandonedLiterals(map[string][]string{"packs/v3block/ops.go": {`"abandoned"`}}); len(bad) != 1 {
		t.Fatal("red control: a seeded writer was not caught")
	}
}
