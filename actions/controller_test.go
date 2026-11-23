package actions

import (
	"context"
	"errors"
	"fmt"
	tr "github.com/pingqiu/sw-test-runner"
	"testing"
	"time"
)

type controllerNode struct {
	run    func(context.Context, string) (string, string, int, error)
	upload func(context.Context, string, string) error
}

func (n *controllerNode) Run(c context.Context, s string) (string, string, int, error) {
	return n.run(c, s)
}
func (n *controllerNode) RunRoot(c context.Context, s string) (string, string, int, error) {
	return n.Run(c, s)
}
func (n *controllerNode) Upload(string, string) error {
	return errors.New("unbounded upload must not be called")
}
func (n *controllerNode) UploadContext(c context.Context, a, b string) error {
	return n.upload(c, a, b)
}
func (n *controllerNode) Close() {}

func TestControllerActions(t *testing.T) {
	r := tr.NewRegistry()
	RegisterSystemActions(r)
	for _, name := range []string{"upload_path", "wait_exec_match"} {
		if !r.Has(name) {
			t.Fatalf("missing %s", name)
		}
	}
}
func TestWaitExecMatch(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outputs []string
		code    int
		wantErr bool
	}{
		{"pending then done", []string{"RUNNING load", "DONE load rc=0"}, 0, false},
		{"terminal failure", []string{"DONE load rc=1"}, 0, true},
		{"nonzero cannot succeed", []string{"DONE load rc=0"}, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			calls := 0
			n := &controllerNode{run: func(context.Context, string) (string, string, int, error) {
				v := tc.outputs[calls]
				calls++
				return v, "", tc.code, nil
			}}
			a := tr.Action{Node: "m01", Params: map[string]string{"cmd": "status load", "match": "^DONE load rc=0$", "fail_match": "^DONE load rc=[1-9]", "interval": "1ns"}}
			out, err := waitExecMatch(ctx, &tr.ActionContext{Nodes: map[string]tr.NodeRunner{"m01": n}}, a)
			if (err != nil) != tc.wantErr || (!tc.wantErr && out["value"] != "DONE load rc=0") {
				t.Fatalf("output=%v err=%v", out, err)
			}
			if calls != len(tc.outputs) {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}
func TestWaitExecMatchCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bounded, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	calls := 0
	n := &controllerNode{run: func(context.Context, string) (string, string, int, error) {
		calls++
		cancel()
		return "RUNNING load", "", 0, nil
	}}
	_, err := waitExecMatch(bounded, &tr.ActionContext{Nodes: map[string]tr.NodeRunner{"m01": n}}, tr.Action{Node: "m01", Params: map[string]string{"cmd": "status", "match": "^DONE$", "interval": "1h"}})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestWaitExecMatchCanceledDoneCannotSucceed(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	n := &controllerNode{run: func(context.Context, string) (string, string, int, error) {
		cancel()
		return "DONE", "", 0, nil
	}}
	_, err := waitExecMatch(ctx, &tr.ActionContext{Nodes: map[string]tr.NodeRunner{"m01": n}}, tr.Action{Node: "m01", Params: map[string]string{"cmd": "status", "match": "^DONE$"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled DONE accepted: %v", err)
	}
}
func TestUploadPathUsesCallerContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	want := fmt.Errorf("transfer canceled: %w", ctx.Err())
	n := &controllerNode{upload: func(c context.Context, a, b string) error {
		if c != ctx || a != "packet.tgz" || b != "/run/packet.tgz" {
			t.Fatal("wrong transfer binding")
		}
		return want
	}}
	_, err := uploadPath(ctx, &tr.ActionContext{Nodes: map[string]tr.NodeRunner{"m01": n}}, tr.Action{Node: "m01", Params: map[string]string{"path": "packet.tgz", "dest": "/run/packet.tgz"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

// host.sh status returns 4 while the recorded phase invocation is active.
// A matching RUNNING is pending, never completion; DONE still needs rc=0.
func TestWaitExecMatchHostPhasePendingExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	calls := 0
	n := &controllerNode{run: func(context.Context, string) (string, string, int, error) {
		calls++
		if calls == 1 {
			return "RUNNING d-load pid 123", "", 4, nil
		}
		return "DONE d-load rc=0", "", 0, nil
	}}
	a := tr.Action{Node: "m01", Params: map[string]string{"cmd": "status d-load", "match": "^DONE d-load rc=0$", "pending_code": "4", "pending_match": "^RUNNING d-load pid [0-9]+$", "interval": "1ns"}}
	out, err := waitExecMatch(ctx, &tr.ActionContext{Nodes: map[string]tr.NodeRunner{"m01": n}}, a)
	if err != nil || calls != 2 || out["value"] != "DONE d-load rc=0" {
		t.Fatalf("calls=%d out=%v err=%v", calls, out, err)
	}
}
