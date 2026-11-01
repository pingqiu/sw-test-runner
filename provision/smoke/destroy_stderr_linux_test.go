//go:build linux

package smoke

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
)

// Terraform writes refresh/plan/progress text to stdout and diagnostics to
// stderr; Native returns both. The run5 destroy failed this way: thousands of
// stdout lines, then one NIC Delete reset block on stderr.
func TestNativeDestroyClassifiesTerraformStderr(t *testing.T) {
	body, err := os.ReadFile("testdata/arm-public-ip-transport.json")
	if err != nil {
		t.Fatal(err)
	}
	var captured map[string]string
	if err = json.Unmarshal(body, &captured); err != nil {
		t.Fatal(err)
	}
	progress := "module.run.azurerm_network_interface.node[\"vol4\"]: Refreshing state... [id=/subscriptions/x]\n\nPlan: 0 to add, 0 to change, 46 to destroy.\nmodule.run.azurerm_network_interface.node[\"vol2\"]: Destruction complete after 6s\n"
	for _, tc := range []struct {
		name, stderr, want string
		calls              int
	}{
		{"run5-nic-delete", "\n" + captured["exact_nic_destroy_error"], "COMPLETE", 2},
		{"unknown-stderr-prefix", "provider crashed\n" + captured["exact_nic_destroy_error"], "CLEANUP_INCOMPLETE", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, text := range map[string]string{"stdout": progress, "stderr": tc.stderr} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			tool := filepath.Join(dir, "terraform")
			script := "#!/bin/sh\ncat " + filepath.Join(dir, "stdout") + "\ncat " + filepath.Join(dir, "stderr") + " >&2\nexit 1\n"
			if err := os.WriteFile(tool, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			synctest.Test(t, func(t *testing.T) {
				c := fixture(t)
				f := &fake{}
				ctl := controller(f)
				calls := 0
				ctl.Command = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
					if name == "terraform" && args[0] == "destroy" {
						calls++
						if calls == 1 {
							return Native(ctx, dir, tool, args...)
						}
					}
					return f.command(ctx, dir, name, args...)
				}
				r := ctl.Execute(context.Background(), "run", c)
				if calls != tc.calls || r.Code != tc.want {
					t.Fatalf("calls=%d result=%+v", calls, r)
				}
			})
		})
	}
}

// Controls from claude03's #45 review: no stdout text can make a retry fire, and an
// unknown stderr line still refuses. Drives the real Native boundary with a
// fake terraform that splits stdout/stderr.
func TestNativeStdoutNeverRetries(t *testing.T) {
	body, _ := os.ReadFile("testdata/arm-public-ip-transport.json")
	var captured map[string]string
	json.Unmarshal(body, &captured)
	block := captured["exact_nic_destroy_error"]
	for _, tc := range []struct {
		name, stdout, stderr, want string
		calls                      int
	}{
		{"block-on-stdout-empty-stderr", block, "", "CLEANUP_INCOMPLETE", 1},
		{"block-on-stdout-foreign-stderr", block, "Error: Provider produced inconsistent result\n", "CLEANUP_INCOMPLETE", 1},
		{"stderr-warning-before-block", block, "Warning: deprecated\n\n" + block, "CLEANUP_INCOMPLETE", 1},
		{"block-twice-on-stderr", "", block + block, "COMPLETE", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "stdout"), []byte(tc.stdout), 0600)
			os.WriteFile(filepath.Join(dir, "stderr"), []byte(tc.stderr), 0600)
			tool := filepath.Join(dir, "terraform")
			os.WriteFile(tool, []byte("#!/bin/sh\ncat "+filepath.Join(dir, "stdout")+"\ncat "+filepath.Join(dir, "stderr")+" >&2\nexit 1\n"), 0700)
			synctest.Test(t, func(t *testing.T) {
				c := fixture(t)
				f := &fake{}
				ctl := controller(f)
				calls := 0
				ctl.Command = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
					if name == "terraform" && args[0] == "destroy" {
						calls++
						if calls == 1 {
							return Native(ctx, dir, tool, args...)
						}
					}
					return f.command(ctx, dir, name, args...)
				}
				r := ctl.Execute(context.Background(), "run", c)
				if calls != tc.calls || r.Code != tc.want {
					t.Fatalf("calls=%d result=%+v", calls, r)
				}
			})
		})
	}
}
