package v3block

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
)

// buildStep is the m2-faults build phase's exec that makes the binaries: the
// one carrying env.PREBUILT.
func buildStep(t *testing.T) tr.Action {
	t.Helper()
	s, err := tr.ParseFile("../../scenarios/v3block/m2-faults.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Phases {
		for _, a := range p.Actions {
			if _, ok := a.Params["env.PREBUILT"]; ok && p.Name == "build" {
				return a
			}
		}
	}
	t.Fatal("no build exec with env.PREBUILT in m2-faults.yaml")
	return tr.Action{}
}

// runBuild runs the step's command with bash, as the exec action does, with
// SRC a git checkout at one commit, WORK and PREBUILT as given.
func runBuild(t *testing.T, prebuilt string) (string, error, string) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	src, work := filepath.Join(dir, "src"), filepath.Join(dir, "work")
	runner := filepath.Join(dir, "runner", "packs", "v3block", "v3ctl")
	for _, d := range []string{src, work, runner} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runner, "main.go"), []byte("//\npackage main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][]string{{"init", "-q"}, {"add", "go.mod"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "x"}} {
		if out, err := exec.Command("git", append([]string{"-C", src}, c...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", c, err, out)
		}
	}
	head, err := exec.Command("git", "-C", src, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	a := buildStep(t)
	cmd := exec.Command(bash, "-c", a.Params["cmd"])
	cmd.Env = append(os.Environ(), "SRC="+src, "WORK="+work, "V3CTL="+filepath.Join(runner, "main.go"), "PREBUILT="+prebuilt)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(head)), err, string(out)
}

// prebuiltDir writes the four binaries and a product.sha naming sha.
func prebuiltDir(t *testing.T, sha string, skip string) string {
	t.Helper()
	d := t.TempDir()
	for _, b := range []string{"blockmaster", "blockvolume", "sw-block", "v3ctl"} {
		if b == skip {
			continue
		}
		if err := os.WriteFile(filepath.Join(d, b), []byte("#!/bin/sh\necho "+b+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if sha != "" {
		if err := os.WriteFile(filepath.Join(d, "product.sha"), []byte(sha+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// A host that must not build takes binaries built elsewhere, but only from the
// commit the product checkout is at, and only all four; otherwise the run
// refuses to start. Commit dates are fixed so every fixture checkout has the
// same HEAD.
func TestPrebuiltBinariesComeFromTheCheckoutsCommit(t *testing.T) {
	t.Setenv("GIT_AUTHOR_DATE", "2026-01-01T00:00:00Z")
	t.Setenv("GIT_COMMITTER_DATE", "2026-01-01T00:00:00Z")
	// Red: no product.sha. This run also tells the fixture's HEAD.
	head, err, out := runBuild(t, prebuiltDir(t, "", ""))
	if err == nil || !strings.Contains(out, "prebuilt_bin has no product.sha") {
		t.Fatalf("no product.sha: err %v, out %q", err, out)
	}
	// Red: binaries built from another commit.
	_, err, out = runBuild(t, prebuiltDir(t, "0000000000000000000000000000000000000000", ""))
	if err == nil || !strings.Contains(out, "was built from 0000000000000000000000000000000000000000") {
		t.Fatalf("another commit: err %v, out %q", err, out)
	}
	// Red: one binary missing.
	_, err, out = runBuild(t, prebuiltDir(t, head, "v3ctl"))
	if err == nil || !strings.Contains(out, "prebuilt_bin lacks an executable v3ctl") {
		t.Fatalf("missing v3ctl: err %v, out %q", err, out)
	}
	// Green: the checkout's commit, all four: copied, and each one hashed.
	_, err, out = runBuild(t, prebuiltDir(t, head, ""))
	if err != nil {
		t.Fatalf("matching prebuilt refused: %v %s", err, out)
	}
	if !strings.Contains(out, "binaries: prebuilt from") {
		t.Fatalf("not reported as prebuilt: %q", out)
	}
	hashed := regexp.MustCompile(`(?m)^[0-9a-f]{64} [ *](blockmaster|blockvolume|sw-block|v3ctl)$`).FindAllString(out, -1)
	if len(hashed) != 4 {
		t.Fatalf("%d of 4 binaries hashed: %q", len(hashed), out)
	}
}
