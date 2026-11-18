package smoke

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The configured pack is admitted before any cloud call (plan and run).
func TestPackAdmission(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "pack.sh")
	os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0700)
	sum, err := fileSHA256(exe)
	if err != nil {
		t.Fatal(err)
	}
	other := strings.Repeat("0", 64)
	for _, tc := range []struct {
		name string
		pack string
		spec *ExecPack
		want string // "" = admitted
	}{
		{"unknown name", "nope", nil, "unknown pack"},
		{"exec without spec", "exec", nil, "needs pack_exec"},
		{"exec relative path", "exec", &ExecPack{Path: "pack.sh", SHA256: sum}, "must be absolute"},
		{"exec bad digest shape", "exec", &ExecPack{Path: exe, SHA256: "ABC"}, "64 lowercase hex"},
		{"exec digest mismatch", "exec", &ExecPack{Path: exe, SHA256: other}, "!= pinned"},
		{"exec missing file", "exec", &ExecPack{Path: exe + ".gone", SHA256: sum}, "pack_exec.path"},
		{"seaweed with a stray pack_exec", "", &ExecPack{Path: exe, SHA256: sum}, "name the exec pack explicitly"},
		{"exec pinned", "exec", &ExecPack{Path: exe, SHA256: sum}, ""},
		{"seaweed default", "", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"plan", "run"} {
				c := fixture(t)
				c.PackName, c.PackExec = tc.pack, tc.spec
				calls := 0
				r := (Controller{Command: func(context.Context, string, string, ...string) ([]byte, error) {
					calls++
					return nil, nil
				}}).Execute(context.Background(), mode, c)
				if tc.want != "" {
					if r.Code != "INPUT_REFUSED" || !strings.Contains(r.Error, tc.want) || calls != 0 {
						t.Fatalf("%s: code %s error %q calls %d, want INPUT_REFUSED containing %q and 0 calls", mode, r.Code, r.Error, calls, tc.want)
					}
				} else if r.Code == "INPUT_REFUSED" {
					t.Fatalf("%s: refused an admissible pack: %s", mode, r.Error)
				}
			}
		})
	}
}

// packFor picks the executable for exec and the built-in pack otherwise.
func TestPackForSelection(t *testing.T) {
	if packName(Config{}) != "seaweed" || packName(Config{PackName: "exec"}) != "exec" {
		t.Fatal("packName")
	}
	if packFor(Config{PackName: "exec"}) == nil || packFor(Config{}) == nil {
		t.Fatal("packFor returned nil")
	}
}
