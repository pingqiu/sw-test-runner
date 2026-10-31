package smoke

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"testing/synctest"
	"time"
)

func TestTransportApplyRetriesSamePlanAndJournalsEachAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := fixture(t)
		f := &fake{}
		ctl := controller(f)
		var attempts [][]string
		start := time.Now()
		ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
			if tool == "terraform" && args[0] == "apply" {
				attempts = append(attempts, append([]string{dir}, args...))
				if len(attempts) == 1 {
					return []byte("HTTP response was nil; connection may have been reset"), errors.New("exit 1")
				}
			}
			return f.command(ctx, dir, tool, args...)
		}
		r := ctl.Execute(context.Background(), "run", c)
		if r.Code != "COMPLETE" || len(attempts) != 2 || !reflect.DeepEqual(attempts[0], attempts[1]) || time.Since(start) != 10*time.Second {
			t.Fatalf("result=%+v attempts=%v elapsed=%v", r, attempts, time.Since(start))
		}
		outputs, _ := filepath.Glob(filepath.Join(c.Output, "*-output"))
		found := false
		for _, name := range outputs {
			b, _ := os.ReadFile(name)
			found = found || string(b) == "HTTP response was nil; connection may have been reset"
		}
		if !found {
			t.Fatal("failed attempt evidence lost")
		}
	})
}

func TestTransportRetriesAreBoundedAndNarrow(t *testing.T) {
	for _, tc := range []struct {
		name, tool, verb, message, want string
		failures, calls                 int
	}{
		{"three-resets", "terraform", "apply", "ECONNRESET", "APPLY_FAILED_OR_UNKNOWN", 3, 3},
		{"auth", "terraform", "apply", "AuthorizationFailed", "APPLY_FAILED_OR_UNKNOWN", 3, 1},
		{"quota", "terraform", "apply", "quota exceeded", "APPLY_FAILED_OR_UNKNOWN", 3, 1},
		{"validation", "terraform", "apply", "validation error", "APPLY_FAILED_OR_UNKNOWN", 3, 1},
		{"mixed-4xx", "terraform", "apply", "HTTP response was nil; StatusCode: 403", "APPLY_FAILED_OR_UNKNOWN", 3, 1},
		{"plan-never-retries", "terraform", "plan", "ECONNRESET", "PLAN_FAILED", 3, 1},
		{"destroy", "terraform", "destroy", "TLS handshake: connection reset", "COMPLETE", 1, 2},
		{"request-eof", "terraform", "apply", `Put "https://management.azure.com/resource": EOF`, "COMPLETE", 1, 2},
		{"list", "az", "resource", "EOF before response", "COMPLETE", 1, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := fixture(t)
				f := &fake{}
				ctl := controller(f)
				calls, destroyed := 0, false
				ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
					if tool == "terraform" && args[0] == "destroy" {
						destroyed = true
					}
					if tool == tc.tool && args[0] == tc.verb {
						calls++
						if calls <= tc.failures {
							return []byte(tc.message), errors.New("exit 1")
						}
					}
					return f.command(ctx, dir, tool, args...)
				}
				r := ctl.Execute(context.Background(), "run", c)
				if r.Code != tc.want || calls != tc.calls {
					t.Fatalf("%+v calls=%d", r, calls)
				}
				if tc.verb == "apply" && tc.want != "COMPLETE" && (!destroyed || !r.CreateUnresolved) {
					t.Fatalf("failed apply lost sticky UNKNOWN or cleanup: %+v", r)
				}
				if tc.name == "three-resets" && time.Now().Sub(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) != 40*time.Second {
					t.Fatal("wrong backoff")
				}
			})
		})
	}
}

func TestTransportRetryHonorsCancellationAndEvidenceFailure(t *testing.T) {
	for _, journalFailure := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			c := fixture(t)
			f := &fake{}
			ctl := controller(f)
			calls, destroyed := 0, false
			ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
				if tool == "terraform" && args[0] == "destroy" {
					destroyed = true
				}
				if tool == "terraform" && args[0] == "apply" {
					calls++
					if journalFailure {
						intents, _ := filepath.Glob(filepath.Join(c.Output, "*-terraform-intent.json"))
						name := intents[len(intents)-1]
						if e := os.Mkdir(name[:len(name)-len("-intent.json")]+"-output", 0700); e != nil {
							t.Fatal(e)
						}
					}
					return []byte("ConnectionResetError 104"), errors.New("exit 1")
				}
				return f.command(ctx, dir, tool, args...)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			r := ctl.Execute(ctx, "run", c)
			if calls != 1 || !destroyed || !r.CreateUnresolved || r.Code != "APPLY_FAILED_OR_UNKNOWN" {
				t.Fatalf("%+v calls=%d", r, calls)
			}
		})
	}
}
