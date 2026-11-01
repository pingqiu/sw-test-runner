package smoke

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestTransportApplyReplansSameStateAndKeepsUnknown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := fixture(t)
		f := &fake{}
		ctl := controller(f)
		var attempts [][]string
		plans := 0
		start := time.Now()
		ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
			if tool == "terraform" && args[0] == "plan" {
				plans++
				if plans > 1 && !strings.Contains(strings.Join(args, " "), "-refresh=true") {
					t.Fatal("retry did not refresh state")
				}
			}
			if tool == "terraform" && args[0] == "apply" {
				attempts = append(attempts, append([]string{dir}, args...))
				if len(attempts) == 1 {
					return []byte("HTTP response was nil; connection may have been reset"), errors.New("exit 1")
				}
			}
			return f.command(ctx, dir, tool, args...)
		}
		r := ctl.Execute(context.Background(), "run", c)
		if r.Code != "CREATE_UNRESOLVED" || !r.CreateUnresolved || plans != 2 || len(attempts) != 2 || !reflect.DeepEqual(attempts[0], attempts[1]) || time.Since(start) != 10*time.Second {
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
		for _, mode := range []string{"destroy", "verify"} {
			afterRestart := controller(f).Execute(context.Background(), mode, c)
			if !afterRestart.CreateUnresolved || afterRestart.Code == "COMPLETE" || afterRestart.Code == "EMPTY" {
				t.Fatalf("%s erased attempt ambiguity: %+v", mode, afterRestart)
			}
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
		{"unknown-provider-code", "terraform", "apply", "Error: creating VM: Code=\"NewProviderError\"; HTTP response was nil", "APPLY_FAILED_OR_UNKNOWN", 3, 1},
		{"status-200", "terraform", "apply", "Error: HTTP 200; EOF before response", "APPLY_FAILED_OR_UNKNOWN", 3, 1},
		{"mixed-plain", "terraform", "apply", "ECONNRESET\nunrecognized fatal failure", "APPLY_FAILED_OR_UNKNOWN", 3, 1},
		{"plan-never-retries", "terraform", "plan", "ECONNRESET", "PLAN_FAILED", 3, 1},
		{"destroy", "terraform", "destroy", "TLS handshake: connection reset", "COMPLETE", 1, 2},
		{"request-eof", "terraform", "apply", `Put "https://management.azure.com/resource": EOF`, "CREATE_UNRESOLVED", 1, 2},
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

func TestTransportRetryEvidenceFailureIsImmediate(t *testing.T) {
	for _, destination := range []string{"output", "status"} {
		synctest.Test(t, func(t *testing.T) {
			c := fixture(t)
			f := &fake{}
			ctl := controller(f)
			calls, destroyed := 0, false
			var statusPath string
			start := time.Now()
			ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
				if tool == "terraform" && args[0] == "destroy" {
					destroyed = true
				}
				if tool == "terraform" && args[0] == "apply" {
					calls++
					if calls == 1 {
						intents, _ := filepath.Glob(filepath.Join(c.Output, "*-terraform-intent.json"))
						name := intents[len(intents)-1]
						statusPath = name[:len(name)-len("-intent.json")] + "-status"
						if e := os.Mkdir(name[:len(name)-len("-intent.json")]+"-"+destination, 0700); e != nil {
							t.Fatal(e)
						}
					}
					return []byte("ConnectionResetError 104"), errors.New("exit 1")
				}
				return f.command(ctx, dir, tool, args...)
			}
			r := ctl.Execute(context.Background(), "run", c)
			if calls != 1 || !destroyed || !r.CreateUnresolved || r.Code != "APPLY_FAILED_OR_UNKNOWN" || time.Since(start) != 0 {
				t.Fatalf("%+v calls=%d", r, calls)
			}
			if destination == "output" {
				body, e := os.ReadFile(statusPath)
				if e != nil || !strings.Contains(string(body), "exit 1") || !strings.Contains(string(body), "directory") {
					t.Fatalf("writable status lost output/command failure: %s %v", body, e)
				}
			}
		})
	}
}

func TestTransportRetryCancellationPreservesCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := fixture(t)
		f := &fake{}
		ctl := controller(f)
		calls := 0
		ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
			if tool == "terraform" && args[0] == "apply" {
				calls++
				return []byte("ECONNRESET"), errors.New("exit 1")
			}
			return f.command(ctx, dir, tool, args...)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		start := time.Now()
		r := ctl.Execute(ctx, "run", c)
		if calls != 1 || !f.cleanContext || !r.CreateUnresolved || time.Since(start) != 5*time.Second {
			t.Fatalf("%+v calls=%d elapsed=%v", r, calls, time.Since(start))
		}
	})
}
