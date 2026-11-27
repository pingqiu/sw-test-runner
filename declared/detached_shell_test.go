package declared

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// shell runs commands with a local shell, as a node does (bash for Run;
// a node's RunRoot wraps in sh -c, dash on Debian and Ubuntu).
type shell struct{ sh string }

func (s shell) Run(ctx context.Context, cmd string) (string, string, int, error) {
	c := exec.CommandContext(ctx, s.sh, "-c", cmd)
	var o, e bytes.Buffer
	c.Stdout, c.Stderr = &o, &e
	err := c.Run()
	var x *exec.ExitError
	if errors.As(err, &x) {
		return o.String(), e.String(), x.ExitCode(), nil
	}
	return o.String(), e.String(), 0, err
}

func (s shell) RunRoot(ctx context.Context, cmd string) (string, string, int, error) {
	return s.Run(ctx, cmd)
}

// The protocol against a real shell (Linux only): a detached command's
// stdout, stderr and exit code come back exactly; one that outlives its
// deadline is killed with its session and reported as a timeout.
func TestRunDetachedRealShell(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs a Linux shell with setsid and pgrep")
	}
	for _, sh := range []string{"bash", "sh"} {
		t.Run(sh, func(t *testing.T) { realShell(t, shell{sh}) })
	}
}

func realShell(t *testing.T, sh shell) {
	dir := t.TempDir()
	ctx := context.Background()
	out, errOut, code, err := RunDetached(ctx, sh, false, dir, `printf 'a b\n'"'"'q'"'"'\n'; echo err >&2; exit 3`, 10*time.Second, 50*time.Millisecond)
	if err != nil || out != "a b\n'q'\n" || errOut != "err\n" || code != 3 {
		t.Fatalf("%q %q %d %v", out, errOut, code, err)
	}
	out, _, code, err = RunDetached(ctx, sh, false, dir, `printf ''`, 10*time.Second, 50*time.Millisecond)
	if err != nil || out != "" || code != 0 {
		t.Fatalf("empty: %q %d %v", out, code, err)
	}
	start := time.Now()
	_, _, _, err = RunDetached(ctx, sh, false, dir, `sleep 300; echo late`, time.Second, 50*time.Millisecond)
	d, ok := AsDetached(err)
	if !ok || len(d.Abandoned) != 0 || time.Since(start) > 10*time.Second {
		t.Fatalf("hung command: %v %+v after %s", err, d, time.Since(start))
	}
	if o, _, _, _ := sh.Run(ctx, "pgrep -f 'slee[p] 300; echo late' || true"); strings.TrimSpace(o) != "" {
		t.Fatalf("the killed session survives: %q", o)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("exchange directories left: %d (only the killed one stays)", len(entries))
	}
}
