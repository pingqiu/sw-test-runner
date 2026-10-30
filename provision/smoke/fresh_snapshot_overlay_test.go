//go:build linux && fresh_review_overlay

package smoke

// These actual loadFresh/read controls use an external Go overlay ONLY to
// relocate the protected approval and inject read interleavings. No production
// CLI/environment flag selects those seams. See the corrective evidence packet.
import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
)

func installFreshTestApproval(t *testing.T, a *freshAuthority, binary []byte) string {
	t.Helper()
	dir := t.TempDir()
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	tpl, recipe, approval := filepath.Join(dir, "template"), filepath.Join(dir, "recipe"), filepath.Join(dir, "approval")
	b, _ := json.Marshal(a.template)
	if e = os.WriteFile(tpl, b, 0600); e != nil {
		t.Fatal(e)
	}
	code := []byte("#!/bin/sh\nexit 0\n")
	if e = os.WriteFile(recipe, code, 0700); e != nil {
		t.Fatal(e)
	}
	p := freshApproval{Version: "seaweed.fresh-approval/v1", Template: tr.FilePin{Path: tpl, SHA256: digest(b)}, Executable: tr.FilePin{Path: exe, SHA256: digest(binary)}, Recipe: tr.FilePin{Path: recipe, SHA256: digest(code)}}
	b, _ = json.Marshal(p)
	if e = os.WriteFile(approval, b, 0600); e != nil {
		t.Fatal(e)
	}
	old := freshApprovalPath
	freshApprovalPath = approval
	t.Cleanup(func() { freshApprovalPath = old; reviewerFreshRead = nil; reviewerFreshAfterRead = nil })
	return tpl
}

func TestFreshLoadDecodesExactlyPinnedSnapshot(t *testing.T) {
	c, a, _, _ := freshFixture(t)
	exe, _ := os.Executable()
	binary, e := os.ReadFile(exe)
	if e != nil {
		t.Fatal(e)
	}
	tpl := installFreshTestApproval(t, a, binary)
	p := a.template.Policy.Jobs["master"]
	p.Argv = []string{"UNAPPROVED"}
	a.template.Policy.Jobs["master"] = p
	b, _ := json.Marshal(a.template)
	reads := 0
	reviewerFreshRead = func(name string) ([]byte, error) {
		if name == tpl {
			reads++
			if reads == 2 {
				return b, nil
			}
		}
		return reviewerOriginalFresh(name)
	}
	got, e := loadFresh(c)
	if e != nil {
		t.Fatal(e)
	}
	if got.template.Policy.Jobs["master"].Argv[0] == "UNAPPROVED" {
		t.Fatalf("decoded unpinned B in A/B/A sequence (%d reads)", reads)
	}
}

func TestFreshLoadChecksActualRunningImage(t *testing.T) {
	if os.Getenv("CODEX02_FRESH_IMAGE_CHILD") == "1" {
		c, a, _, _ := freshFixture(t)
		exe, _ := os.Executable()
		dir := filepath.Dir(exe)
		if !strings.HasPrefix(dir, os.Getenv("TMPDIR")+"/") {
			t.Fatal("child image outside own temp")
		}
		other, e := os.ReadFile("/bin/true")
		if e != nil {
			t.Fatal(e)
		}
		installFreshTestApproval(t, a, other)
		if e = os.Remove(exe); e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(exe, other, 0755); e != nil {
			t.Fatal(e)
		}
		if _, e = loadFresh(c); e == nil {
			t.Fatal("load accepted replacement pathname as running image")
		}
		return
	}
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	binary, e := os.ReadFile(exe)
	if e != nil {
		t.Fatal(e)
	}
	child := filepath.Join(t.TempDir(), "image-child")
	if e = os.WriteFile(child, binary, 0755); e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(child, "-test.run=^TestFreshLoadChecksActualRunningImage$", "-test.v")
	cmd.Env = append(os.Environ(), "CODEX02_FRESH_IMAGE_CHILD=1")
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("%s: %v", out, e)
	}
}

func TestFreshProtectedSnapshotDetectsMetadataAndPathChange(t *testing.T) {
	for _, mode := range []string{"mode", "owner", "ctime-restore", "path"} {
		t.Run(mode, func(t *testing.T) {
			name := filepath.Join(t.TempDir(), "input")
			if e := os.WriteFile(name, []byte("AAAA"), 0600); e != nil {
				t.Fatal(e)
			}
			reviewerFreshAfterRead = func() {
				switch mode {
				case "mode":
					if e := os.Chmod(name, 0666); e != nil {
						t.Fatal(e)
					}
				case "owner":
					if os.Geteuid() != 0 {
						t.Skip("chown control needs root")
					}
					if e := os.Chown(name, 2001, 2001); e != nil {
						t.Fatal(e)
					}
				case "ctime-restore":
					if e := os.Chmod(name, 0644); e != nil {
						t.Fatal(e)
					}
					if e := os.Chmod(name, 0600); e != nil {
						t.Fatal(e)
					}
				case "path":
					if e := os.Rename(name, name+".original"); e != nil {
						t.Fatal(e)
					}
					if e := os.WriteFile(name, []byte("BBBB"), 0600); e != nil {
						t.Fatal(e)
					}
				}
			}
			t.Cleanup(func() { reviewerFreshAfterRead = nil })
			if _, e := protectedFresh(name); e == nil {
				t.Fatal("changed metadata/path accepted")
			}
		})
	}
}
