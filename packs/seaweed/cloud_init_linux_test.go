//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClient2CloudInitActualCommand(t *testing.T) {
	for _, allowed := range []bool{true, false} {
		t.Run(fmt.Sprint(allowed), func(t *testing.T) {
			bin := t.TempDir()
			t.Setenv("REVIEW_CAPTURE_BIN", bin)
			warning := client2Warning
			if !allowed {
				warning = "unapproved package installation warning"
			}
			raw := fmt.Sprintf(`{"status":"done","extended_status":"degraded done","errors":[],"recoverable_errors":{"WARNING":[%q]},"modules-final":{"errors":[],"recoverable_errors":{}}}`, warning)
			// Real sh -> executable -> rc 2 over loopback SSH; no production parser mocked.
			script := "#!/bin/sh\ncase \"$*\" in 'status --wait') printf 'status: done\\n'; exit 2;; 'status --wait --format json') ;; *) exit 93;; esac\ncat <<'JSON'\n" + raw + "\nJSON\nexit 2\n"
			if e := os.WriteFile(filepath.Join(bin, "cloud-init"), []byte(script), 0700); e != nil {
				t.Fatal(e)
			}
			input, c, loads := clusterInput(t, false, "cloud-init-shell")
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			_, err := RunCloudHTTP(ctx, input, c)
			if (err == nil) != allowed {
				t.Fatalf("allowed=%t error=%v", allowed, err)
			}
			if !allowed && loads.Load() != 0 {
				t.Fatal("load reached after unknown cloud-init error")
			}
			captured, e := os.ReadFile(filepath.Join(c.Output, "prepare/client2/cloud-init-status.json"))
			if e != nil || strings.TrimSpace(string(captured)) != raw {
				t.Fatalf("raw evidence lost: %s %v", captured, e)
			}
			step, e := os.ReadFile(filepath.Join(c.Output, "prepare/client2/cloud-init-step.json"))
			var record struct {
				RC    int    `json:"exit_code"`
				Error string `json:"error"`
			}
			if e != nil || json.Unmarshal(step, &record) != nil || record.RC != 2 || (record.Error == "") != allowed {
				t.Fatalf("original rc/gate error lost: %s %v", step, e)
			}
		})
	}
}

func TestCancelledPeerWithoutServicesStillCollects(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("REVIEW_CAPTURE_BIN", bin)
	// No unit is expected when a peer fails during prepare. A forbidden journal
	// invocation would fail, proving MISSING is distinguished from journal errors.
	sudo := "#!/bin/sh\nif [ \"$1\" = systemctl ]; then exit 0; fi\nprintf 'unexpected journal' >&2\nexit 23\n"
	for name, script := range map[string]string{"sudo": sudo, "tail": "#!/bin/sh\nprintf 'collected cloud-init output\\n'\n"} {
		if e := os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); e != nil {
			t.Fatal(e)
		}
	}
	raw, c, _ := clusterInput(t, false, "cancelled-peer-no-services")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if _, err := RunCloudHTTP(ctx, raw, c); err == nil || !strings.Contains(err.Error(), "cloud-init") || strings.Contains(err.Error(), "collect/") {
		t.Fatalf("primary failure lost or peer cancellation poisoned collection: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(c.Output, "collect/meta/cloud-failure-fetch.json"))
	if err != nil || !strings.Contains(string(b), "MISSING: run services not started") {
		t.Fatalf("%s %v", b, err)
	}
}
