package smoke

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A run without a well-formed LAUNCH token is refused before ANY tool call and
// before the output directory exists (2026-11-18 incident: a packet build
// reached the real az without a launch). The command below is a tripwire.
func TestRunRefusesWithoutLaunchToken(t *testing.T) {
	for _, tok := range []string{"", "zz", "0123456789abcdef0123456", "0123456789ABCDEF01234567",
		"0123456789abcdef012345678", "0123456789abcdef01234567 ", " 0123456789abcdef01234567", "0123456789abcdef0123456g"} {
		c := fixture(t)
		c.LaunchToken = tok
		calls := 0
		ctl := Controller{Command: func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
			calls++
			return nil, nil
		}}
		r := ctl.Execute(context.Background(), "run", c)
		if r.Code != "LAUNCH_REFUSED" {
			t.Errorf("token %q: code %s, want LAUNCH_REFUSED", tok, r.Code)
		}
		if calls != 0 {
			t.Errorf("token %q: %d tool call(s) before the refusal, want 0", tok, calls)
		}
		if _, e := os.Stat(c.Output); !os.IsNotExist(e) {
			t.Errorf("token %q: output dir exists after the refusal (stat err %v)", tok, e)
		}
	}
}

// plan stays local and needs no token.
func TestPlanNeedsNoLaunchToken(t *testing.T) {
	c := fixture(t)
	c.LaunchToken = ""
	r := (Controller{Command: func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
		t.Fatalf("plan made a %s call", tool)
		return nil, nil
	}}).Execute(context.Background(), "plan", c)
	if r.Code != "PLAN_ONLY" {
		t.Fatalf("plan without a token: code %s (%s), want PLAN_ONLY", r.Code, r.Error)
	}
}

// A run with a token records it in launch.json before its first tool call.
func TestRunRecordsLaunchToken(t *testing.T) {
	c := fixture(t)
	f := &fake{}
	seen := false
	ctl := controller(f)
	inner := ctl.Command
	ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
		if !seen {
			seen = true
			b, e := os.ReadFile(filepath.Join(c.Output, "launch.json"))
			var rec map[string]string
			if e == nil {
				e = json.Unmarshal(b, &rec)
			}
			if e != nil || rec["launch_token"] != testLaunchToken || rec["schema"] != "seaweed.cloud-launch/v1" {
				t.Errorf("launch.json before the first call: %v %v", rec, e)
			}
		}
		return inner(ctx, dir, tool, args...)
	}
	ctl.Execute(context.Background(), "run", c)
	if !seen {
		t.Fatal("the run made no tool call")
	}
}

// Recovery is never blocked by the guard: destroy and verify run without a token.
func TestDestroyAndVerifyNeedNoLaunchToken(t *testing.T) {
	for _, mode := range []string{"destroy", "verify"} {
		c := fixture(t)
		f := &fake{}
		if r := controller(f).Execute(context.Background(), "run", c); r.Code == "LAUNCH_REFUSED" {
			t.Fatalf("setup run refused: %+v", r)
		}
		c.LaunchToken = ""
		r := controller(f).Execute(context.Background(), mode, c)
		if r.Code == "LAUNCH_REFUSED" {
			t.Errorf("%s without a token was refused by the launch guard", mode)
		}
	}
}
