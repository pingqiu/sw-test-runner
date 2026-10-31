package smoke

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
)

func TestReviewerNonTransportErrorWithTransportDetailMustNotRetry(t *testing.T) {
	for _, message := range []string{
		"Error: Provider produced inconsistent result after apply\nThe previous request diagnostic was ECONNRESET; the provider returned an inconsistent result.",
		"Error: InvalidParameter: the requested VM size is not supported\nPrevious network diagnostic: connection reset by peer",
		"Error: BadRequest: the requested operation is invalid; previous network diagnostic: EOF before response",
	} {
		t.Run(message, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := fixture(t)
				f := &fake{}
				ctl := controller(f)
				applies, plans := 0, 0
				ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
					if tool == "terraform" && args[0] == "plan" {
						plans++
					}
					if tool == "terraform" && args[0] == "apply" {
						applies++
						if applies == 1 {
							return []byte(message), errors.New("exit 1")
						}
					}
					return f.command(ctx, dir, tool, args...)
				}
				r := ctl.Execute(context.Background(), "run", c)
				t.Logf("applies=%d plans=%d code=%s unresolved=%v", applies, plans, r.Code, r.CreateUnresolved)
				if applies != 1 || plans != 1 {
					t.Error("a non-transport error title retried because its detail contained a transport substring")
				}
			})
		})
	}
}

func TestReviewerFailedRefreshPlanStopsApplyRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := fixture(t)
		f := &fake{}
		ctl := controller(f)
		applies, plans := 0, 0
		ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
			if tool == "terraform" && args[0] == "plan" {
				plans++
				if plans > 1 {
					if !strings.Contains(strings.Join(args, " "), "-refresh=true") {
						t.Fatal("refresh omitted")
					}
					return []byte("ECONNRESET"), errors.New("refresh plan failed")
				}
			}
			if tool == "terraform" && args[0] == "apply" {
				applies++
				return []byte("ECONNRESET"), errors.New("apply failed")
			}
			return f.command(ctx, dir, tool, args...)
		}
		r := ctl.Execute(context.Background(), "run", c)
		t.Logf("applies=%d plans=%d code=%s unresolved=%v", applies, plans, r.Code, r.CreateUnresolved)
		if applies != 1 || plans != 2 || !r.CreateUnresolved || r.Code == "COMPLETE" {
			t.Fatal("failed refresh plan did not stop apply retry with sticky uncertainty")
		}
	})
}
