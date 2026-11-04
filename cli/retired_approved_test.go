package cli

import (
	tr "github.com/pingqiu/sw-test-runner"
	"io"
	"os"
	"strings"
	"testing"
)

func TestSeaweedApprovedCLIIsRetired(t *testing.T) {
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	called := false
	code := Run(func(*tr.Registry) { called = true }, []string{"approved-run", "--request", "/absent", "--policy", "/absent"})
	w.Close()
	os.Stderr = old
	raw, err := io.ReadAll(r)
	r.Close()
	if err != nil || code == 0 || called || !strings.Contains(string(raw), "unknown command: approved-run") {
		t.Fatalf("retired entry did not refuse as unknown: code=%d called=%v stderr=%s err=%v", code, called, raw, err)
	}
}
