package actions

import (
	"context"
	"fmt"
	tr "github.com/pingqiu/sw-test-runner"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// uploadPath sends a prepared file/archive; extraction remains a separate,
// visible host call. A legacy unbounded Upload is deliberately not a fallback.
func uploadPath(ctx context.Context, a *tr.ActionContext, act tr.Action) (map[string]string, error) {
	n, ok := a.Nodes[act.Node].(interface {
		UploadContext(context.Context, string, string) error
	})
	if !ok {
		return nil, fmt.Errorf("upload_path: node %q has no bounded upload", act.Node)
	}
	if act.Params["path"] == "" || act.Params["dest"] == "" {
		return nil, fmt.Errorf("upload_path: path and dest required")
	}
	if err := n.UploadContext(ctx, act.Params["path"], act.Params["dest"]); err != nil {
		return nil, fmt.Errorf("upload_path: %w", err)
	}
	return map[string]string{"value": act.Params["dest"]}, nil
}

// waitExecMatch polls a readable receipt, never interpreting a transport error
// or a failed command as completion. The caller must supply a bounded context.
func waitExecMatch(ctx context.Context, a *tr.ActionContext, act tr.Action) (map[string]string, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, fmt.Errorf("wait_exec_match: deadline required")
	}
	n := a.Nodes[act.Node]
	if n == nil {
		return nil, fmt.Errorf("wait_exec_match: node %q missing", act.Node)
	}
	if act.Params["cmd"] == "" || act.Params["match"] == "" {
		return nil, fmt.Errorf("wait_exec_match: cmd and match required")
	}
	match, err := regexp.Compile(act.Params["match"])
	if err != nil {
		return nil, err
	}
	var fail *regexp.Regexp
	if text := act.Params["fail_match"]; text != "" {
		fail, err = regexp.Compile(text)
		if err != nil {
			return nil, err
		}
	}
	pendingCode, pending := 0, (*regexp.Regexp)(nil)
	if text := act.Params["pending_code"]; text != "" {
		pendingCode, err = strconv.Atoi(text)
		if err != nil || pendingCode <= 0 || act.Params["pending_match"] == "" {
			return nil, fmt.Errorf("wait_exec_match: positive pending_code requires pending_match")
		}
		pending, err = regexp.Compile(act.Params["pending_match"])
		if err != nil {
			return nil, err
		}
	}
	interval := time.Second
	if text := act.Params["interval"]; text != "" {
		interval, err = time.ParseDuration(text)
		if err != nil || interval <= 0 {
			return nil, fmt.Errorf("wait_exec_match: invalid interval")
		}
	}
	last := ""
	for {
		if err := ctx.Err(); err != nil {
			return map[string]string{"value": last}, fmt.Errorf("wait_exec_match: last=%q: %w", last, err)
		}
		var stderr string
		var code int
		if act.Params["root"] == "true" {
			last, stderr, code, err = n.RunRoot(ctx, act.Params["cmd"])
		} else {
			last, stderr, code, err = n.Run(ctx, act.Params["cmd"])
		}
		last = strings.TrimSpace(last)
		if cause := ctx.Err(); cause != nil {
			return map[string]string{"value": last}, fmt.Errorf("wait_exec_match: %w", cause)
		}
		if err != nil {
			return map[string]string{"value": last}, fmt.Errorf("wait_exec_match: %w", err)
		}
		if code != 0 && !(code == pendingCode && pending != nil && pending.MatchString(last)) {
			return map[string]string{"value": last}, fmt.Errorf("wait_exec_match: code=%d stderr=%q", code, stderr)
		}
		if fail != nil && fail.MatchString(last) {
			return map[string]string{"value": last}, fmt.Errorf("wait_exec_match: terminal receipt %q", last)
		}
		if code == 0 && match.MatchString(last) {
			return map[string]string{"value": last}, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}
