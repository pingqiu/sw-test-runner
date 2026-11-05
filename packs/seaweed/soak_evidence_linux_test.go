package seaweed

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pingqiu/sw-test-runner/infra"
)

// codex01 R2 on #118: the M02 evidence file is fetched through the real step path (cloudStep on a native node,
// as the direct path's ssh node runs it), bounded at the source. A 2 h (151 KB) and an 8 h (600 KB) file arrive
// whole; a file over the 4 MiB bound is refused, and no more than the bound + 1 byte reaches memory or the journal.
func TestM02EvidenceFetchThroughTheRealStepPathIsBounded(t *testing.T) {
	ctx := context.Background()
	node := &infra.Node{IsNative: true}
	for name, rotations := range map[string]int{"2 h (151 KB)": 150, "8 h (600 KB)": 595} {
		root, out := t.TempDir(), t.TempDir()
		body := collectRow(t, rotations)
		if err := os.WriteFile(filepath.Join(root, "m02-collect.json"), body, 0600); err != nil {
			t.Fatal(err)
		}
		got, err := m02Evidence(refRow(body, nil), "m02-collect.json", func(file string) (string, error) {
			return fetchM02Evidence(ctx, node, out, root, file)
		})
		if err != nil || string(got) != string(body) {
			t.Fatalf("%s: %d of %d bytes: %v", name, len(got), len(body), err)
		}
		t.Logf("%s: %d bytes fetched whole", name, len(body))
	}

	root, out := t.TempDir(), t.TempDir()
	big := []byte(strings.Repeat("a", m02EvidenceBound+4096))
	if err := os.WriteFile(filepath.Join(root, "m02-collect.json"), big, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := fetchM02Evidence(ctx, node, out, root, "m02-collect.json")
	if err == nil || !strings.Contains(err.Error(), "exceeds") || len(got) != m02EvidenceBound+1 {
		t.Fatalf("an over-bound file: %d bytes read, err=%v", len(got), err)
	}
	journal, err := os.Stat(filepath.Join(out, "soak-fetch-m02-collect.json"))
	if err != nil || journal.Size() > m02EvidenceBound+64<<10 {
		t.Fatalf("the step journal holds %v bytes: %v", journal, err)
	}
	// And through m02Evidence with a reference that claims the bound: refused, never published.
	if _, err := m02Evidence(refRow(big[:m02EvidenceBound], nil), "m02-collect.json", func(file string) (string, error) {
		return fetchM02Evidence(ctx, node, t.TempDir(), root, file)
	}); err == nil {
		t.Fatal("an over-bound file was published")
	}
	if _, err := fetchM02Evidence(ctx, node, t.TempDir(), root, "absent.json"); err == nil {
		t.Fatal("a missing file was fetched")
	}
}
