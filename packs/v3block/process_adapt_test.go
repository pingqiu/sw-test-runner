package v3block

// The fault and surface tests' entry points to the declared process actions
// (product/actions.json v3_op_attach, v3_op_kill, v3_op_restart): each runs
// the registered handler on the given node with the test's opEnv as the
// step's configuration params, and a bound as the step's int seconds param
// (whole seconds, as a scenario writes it).

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

// params is e as a step's configuration params.
func (e opEnv) params() map[string]string {
	p := map[string]string{"bin_dir": e.bin, "master": e.master, "filer": e.filer, "chunk_path": e.chunkPath, "root": e.root,
		"client": e.client, "own_script": e.ownScript, "work": e.work, "owner": e.owner}
	if p["chunk_path"] == "" {
		p["chunk_path"] = "/chunks"
	}
	if e.lockDirs {
		p["lock_dirs_test_only"] = "true"
	}
	return p
}

func secs(d time.Duration) string { return strconv.Itoa(int(d / time.Second)) }

func (e opEnv) runDeclared(ctx context.Context, actx *tr.ActionContext, node runner, action string, params map[string]string) (map[string]string, error) {
	old := opNode
	opNode = func(*tr.ActionContext, string) (runner, error) { return node, nil }
	defer func() { opNode = old }()
	p := e.params()
	for k, v := range params {
		p[k] = v
	}
	h := blockActions.Handler
	if action == "v3_op_restart" {
		return v3OpRestart(ctx, actx, tr.Action{Params: p})
	}
	return h(action)(ctx, actx, tr.Action{Params: p})
}

// kill is the declared v3_op_kill of process on node.
func (e opEnv) kill(ctx context.Context, actx *tr.ActionContext, node runner, process string, bound time.Duration) (map[string]string, error) {
	return e.runDeclared(ctx, actx, node, "v3_op_kill", map[string]string{"process": process, "kill_seconds": secs(bound)})
}

// restart is the declared v3_op_restart of process on node (the blockmaster
// handed to its Go handler).
func (e opEnv) restart(ctx context.Context, actx *tr.ActionContext, node runner, process string, bound time.Duration) (map[string]string, error) {
	return e.runDeclared(ctx, actx, node, "v3_op_restart", map[string]string{"process": process, "ready_seconds": secs(bound)})
}

// attachOn runs the declared v3_op_attach of v on node (blocks, the ready
// bound) and returns its outcome and completed result; v is updated with
// the runner's record after it.
func attachOn(t *testing.T, e opEnv, node runner, v *opVolume, blocks int, bound time.Duration) (string, map[string]any) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "history.jsonl")
	h, err := tr.OpenHistory(p)
	if err != nil {
		t.Fatal(err)
	}
	actx := &tr.ActionContext{History: h, Vars: map[string]string{}, Log: func(string, ...interface{}) {}}
	saveVolume(actx, v)
	out, err := e.runDeclared(context.Background(), actx, node, "v3_op_attach", map[string]string{"volume": v.Name, "blocks": strconv.Itoa(blocks), "ready_seconds": secs(bound)})
	if err != nil {
		t.Fatal(err)
	}
	_ = h.Close()
	got, err := loadVolume(actx, v.Name)
	if err != nil {
		t.Fatal(err)
	}
	*v = *got
	for _, r := range records(t, p) {
		if r.Kind == tr.HistoryComplete {
			return out["value"], r.Result
		}
	}
	t.Fatal("no complete record")
	return "", nil
}
