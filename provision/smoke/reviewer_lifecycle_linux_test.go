//go:build linux

package smoke

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReviewerRecoveryMustNotDeclareUnknownCreateClean(t *testing.T) {
	c := fixture(t)
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	release, created := filepath.Join(bin, "release"), filepath.Join(bin, "created")
	// A local fake asynchronous provider accepts a create, loses its CLI reply,
	// and completes only after the independent recovery-empty observation.
	scripts := map[string]string{
		"terraform": "#!/bin/sh\ncase \"$1\" in\ninit|plan|destroy) exit 0;;\napply) (while [ ! -f '" + release + "' ]; do sleep 0.01; done; touch '" + created + "') </dev/null >/dev/null 2>&1 &\nexit 9;;\n*) exit 2;;\nesac\n",
		"az":        "#!/bin/sh\nif [ -f '" + created + "' ]; then printf '[{\"id\":\"/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/dedicated-test/providers/Microsoft.Compute/virtualMachines/late\",\"type\":\"Microsoft.Compute/virtualMachines\"}]'; else printf '[]'; fi\n",
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	// Always release and join the fake child, including a failed assertion.
	defer func() {
		_ = os.WriteFile(release, []byte("release"), 0600)
		until := time.Now().Add(3 * time.Second)
		for time.Now().Before(until) {
			if _, e := os.Stat(created); e == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("fake asynchronous create did not settle")
	}()
	first := (Controller{}).Execute(context.Background(), "run", c)
	if first.Code != "APPLY_FAILED_OR_UNKNOWN" {
		t.Fatalf("first=%+v", first)
	}
	recovered := (Controller{}).Execute(context.Background(), "destroy", c)
	_ = os.WriteFile(release, []byte("release"), 0600)
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		if _, e := os.Stat(created); e == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	observed := (Controller{}).Execute(context.Background(), "verify", c)
	t.Logf("original=%s recovery=%s observed=%t remaining_at_recovery=%v later=%s ids=%v", first.Code, recovered.Code, recovered.RemainingObserved, recovered.Remaining, observed.Code, observed.Remaining)
	if recovered.Code == "COMPLETE" {
		t.Fatal("unresolved create was reported clean before its provider-side completion")
	}
}

func TestReviewerVerifyNeedsALocalDeadline(t *testing.T) {
	c := fixture(t)
	first := &fake{}
	if r := controller(first).Execute(context.Background(), "run", c); r.Code != "COMPLETE" {
		t.Fatal(r)
	}
	bounded := false
	ctl := Controller{Command: func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
		if tool != "az" || !strings.HasPrefix(strings.Join(args, " "), "resource list ") {
			t.Fatal("verify mutated")
		}
		deadline, ok := ctx.Deadline()
		bounded = ok && deadline.After(time.Now()) && time.Until(deadline) <= 15*time.Minute
		return []byte(`[]`), nil
	}}
	r := ctl.Execute(context.Background(), "verify", c)
	if !bounded {
		t.Fatalf("verify reported %s without any local command deadline", r.Code)
	}
}
