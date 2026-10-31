//go:build linux

package testrunner

import (
	"context"
	"testing"
)

func TestReviewerFiniteTaskRequiresObservedTerminal(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		wait bool
		want ResultStatus
	}{
		{"unwaited-failed", 2, false, StatusFail},
		{"unwaited-zero-no-output-proof", 0, false, StatusFail},
		{"awaited-zero-positive", 0, true, StatusPass},
		{"awaited-failure-positive", 2, true, StatusFail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, n := generationFixture(t)
			node := &taskNode{generationNode: n, release: make(chan struct{}), taskWaiting: make(chan struct{}), taskCode: tc.code}
			close(node.release)
			f.runtime.Jobs = node
			_, result := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
				h, err := a.StartApprovedPackTask(ctx, "volume0")
				if err != nil {
					return err
				}
				if tc.wait {
					_, _ = a.WaitApprovedPackTask(ctx, h)
				}
				return nil
			})
			if result.Status != tc.want {
				t.Fatalf("finite task final=%s want=%s; task exit=%d awaited=%v", result.Status, tc.want, tc.code, tc.wait)
			}
		})
	}
}

func TestApprovedTaskEarlyCleanupCannotHideTerminal(t *testing.T) {
	for _, closeScope := range []bool{false, true} {
		f, n := generationFixture(t)
		node := &taskNode{generationNode: n, release: make(chan struct{}), taskWaiting: make(chan struct{})}
		close(node.release)
		f.runtime.Jobs = node
		_, result := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
			h, err := a.StartApprovedPackTask(ctx, "volume0")
			if err != nil {
				return err
			}
			if closeScope {
				_ = a.Safety.Close()
			} else {
				_ = a.Safety.Stop(ctx, h)
			}
			// Neither swallowing cleanup's failure nor retrying Wait can clear it.
			_, _ = a.WaitApprovedPackTask(ctx, h)
			return nil
		})
		if result.Status != StatusFail {
			t.Fatalf("early cleanup close=%v hid missing terminal", closeScope)
		}
	}
}

type corruptTaskOutput struct{ *taskNode }

func (n *corruptTaskOutput) Wait(ctx context.Context, id string) (ExitInfo, error) {
	e, err := n.taskNode.Wait(ctx, id)
	e.Output.Complete = false
	return e, err
}
func TestApprovedTaskRawWaitFailureStaysSticky(t *testing.T) {
	f, n := generationFixture(t)
	task := &taskNode{generationNode: n, release: make(chan struct{}), taskWaiting: make(chan struct{})}
	close(task.release)
	f.runtime.Jobs = &corruptTaskOutput{task}
	_, result := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
		h, err := a.StartApprovedPackTask(ctx, "volume0")
		if err != nil {
			return err
		}
		_, _ = a.Safety.Wait(ctx, h) // Even bypassing the wrapper cannot hide invalid output.
		_ = a.Safety.Stop(ctx, h)
		return nil
	})
	if result.Status != StatusFail {
		t.Fatal("invalid captured output hidden by retirement")
	}
}
