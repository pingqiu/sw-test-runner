package smoke

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"testing/synctest"
)

func TestReviewerAmbiguousFirstCreateMustStayUnresolvedAfterRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := fixture(t)
		f := &fake{}
		ctl := controller(f)
		release, finished := make(chan struct{}), make(chan struct{})
		var attempts [][]string
		ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
			if tool == "terraform" && args[0] == "apply" {
				attempts = append(attempts, append([]string{dir}, args...))
				if len(attempts) == 1 {
					go func() { <-release; f.live = true; close(finished) }()
					return []byte("HTTP response was nil; connection may have been reset"), errors.New("exit 1")
				}
			}
			return f.command(ctx, dir, tool, args...)
		}
		r := ctl.Execute(context.Background(), "run", c)
		if len(attempts) != 2 || !reflect.DeepEqual(attempts[0], attempts[1]) {
			close(release)
			<-finished
			t.Fatal("control did not retry the same plan/state")
		}
		t.Logf("before_late_completion code=%s unresolved=%v observed=%v remaining=%v", r.Code, r.CreateUnresolved, r.RemainingObserved, r.Remaining)
		close(release)
		<-finished
		later := ctl.Execute(context.Background(), "verify", c)
		t.Logf("after_original_create_completes code=%s unresolved=%v observed=%v remaining=%v", later.Code, later.CreateUnresolved, later.RemainingObserved, later.Remaining)
		if r.Code == "COMPLETE" || !r.CreateUnresolved {
			t.Error("retry exit zero erased a still-pending first create and falsely certified clean")
		}
		if len(later.Remaining) == 0 {
			t.Fatal("control did not produce the delayed resource")
		}
	})
}

func TestReviewerMixedNonTransportDiagnosticMustNotRetry(t *testing.T) {
	for _, message := range []string{
		"Error: creating public IP: HTTP response was nil; connection may have been reset\nError: Provider produced inconsistent result after apply",
		"Error: creating public IP: Put \"https://management.azure.com/ip\": EOF\nError: creating VM: Code=\"InvalidParameter\" Message=\"Size is not supported\"",
		"WARNING: previous network attempt reported ECONNRESET\nERROR: (BadRequest) The requested operation is invalid",
	} {
		t.Run(message, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := fixture(t)
				f := &fake{}
				ctl := controller(f)
				calls := 0
				ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
					if tool == "terraform" && args[0] == "apply" {
						calls++
						if calls == 1 {
							return []byte(message), errors.New("exit 1")
						}
					}
					return f.command(ctx, dir, tool, args...)
				}
				r := ctl.Execute(context.Background(), "run", c)
				t.Logf("apply_calls=%d code=%s unresolved=%v", calls, r.Code, r.CreateUnresolved)
				if calls != 1 {
					t.Error("non-transport failure retried because a different diagnostic contained a transport substring")
				}
			})
		})
	}
}
