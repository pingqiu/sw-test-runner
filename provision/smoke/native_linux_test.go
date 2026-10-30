//go:build linux

package smoke

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeChainReachesRealAdmissionRefusalAndCleans(t *testing.T) {
	c := fixture(t)
	bin := filepath.Join(t.TempDir(), "bin")
	if e := os.Mkdir(bin, 0700); e != nil {
		t.Fatal(e)
	}
	// Fake CLIs at the actual local exec seam cannot authenticate/contact Azure.
	scripts := map[string]string{
		"az":        "#!/bin/sh\n[ \"$1 $2\" = 'resource list' ] || exit 2\nprintf '[]'\n",
		"terraform": "#!/bin/sh\ncase \"$1\" in\ninit|plan|apply|destroy) exit 0;;\noutput) printf '{\"local\":\"export\"}';;\n*) exit 2;;\nesac\n",
	}
	for name, s := range scripts {
		if e := os.WriteFile(filepath.Join(bin, name), []byte(s), 0700); e != nil {
			t.Fatal(e)
		}
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	r := (Controller{}).Execute(context.Background(), "run", c)
	if r.Code != "PACK_FAILED" || !r.RemainingObserved || len(r.Remaining) != 0 || !strings.Contains(r.Error, "request.json") {
		t.Fatalf("%+v", r)
	}
	matches, _ := filepath.Glob(filepath.Join(c.Output, "*-status"))
	if len(matches) != 7 {
		t.Fatalf("expected 7 native operations, got %d", len(matches))
	}
}

func TestNativeUsesExitNotWarningAndDropsTerraformOverrides(t *testing.T) {
	t.Setenv("TF_VAR_subscription_id", "foreign")
	t.Setenv("TF_CLI_ARGS_apply", "-destroy")
	b, e := Native(context.Background(), t.TempDir(), "/bin/sh", "-c", `printf warning >&2; [ -z "$TF_VAR_subscription_id$TF_CLI_ARGS_apply" ] || exit 2; printf ok`)
	if e != nil || string(b) != "ok" {
		t.Fatalf("%s %v", b, e)
	}
	if _, e = Native(context.Background(), t.TempDir(), "/bin/sh", "-c", "exit 9"); e == nil {
		t.Fatal("failed native command accepted")
	}
}
