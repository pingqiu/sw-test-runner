package smoke

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"testing/synctest"
)

func TestReviewerUnknownDiagnosticBeforeKnownErrorMustRefuse(t *testing.T) {
	raw, err := os.ReadFile("testdata/arm-public-ip-transport.json")
	if err != nil {
		t.Fatal(err)
	}
	var forms map[string]string
	if err = json.Unmarshal(raw, &forms); err != nil {
		t.Fatal(err)
	}
	for _, message := range []string{
		"FATAL: InvalidParameter: requested VM size is not supported\nError: ECONNRESET",
		"unrecognized provider failure\n" + forms["exact_apply_error"],
		"WrapperFailure: provider produced inconsistent result after apply\nError: EOF before response",
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
					t.Error("unknown/fatal prefix discarded before complete Error-block validation")
				}
			})
		})
	}
}
