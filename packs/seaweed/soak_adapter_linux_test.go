//go:build linux

package seaweed

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testAdapter(t *testing.T) *soakAdapter {
	t.Helper()
	return &soakAdapter{root: t.TempDir(), runID: "run-1", bootID: "boot-1", roles: map[string]string{}}
}

func TestSoakAdapterPublishIsExclusiveAndReadBoundIsStrict(t *testing.T) {
	s := testAdapter(t)
	p := filepath.Join(s.root, "x.json")
	if err := publishFile(p, map[string]any{"a": 1}); err != nil {
		t.Fatal(err)
	}
	if err := publishFile(p, map[string]any{"a": 2}); err == nil {
		t.Fatal("second publish of the same name admitted")
	}
	var v map[string]int
	if err := readBound(p, &v); err != nil || v["a"] != 1 {
		t.Fatalf("read back %v %v", v, err)
	}
	entries, _ := os.ReadDir(s.root)
	if len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
	big := filepath.Join(s.root, "big.json")
	os.WriteFile(big, []byte(`{"a":1}`+strings.Repeat(" ", 4096)+"garbage"), 0o600)
	if err := readBound(big, &v); err == nil {
		t.Fatal("oversized file admitted")
	}
	link := filepath.Join(s.root, "link.json")
	os.Symlink(p, link)
	if err := readBound(link, &v); err == nil {
		t.Fatal("symlink admitted")
	}
	if err := readBound(s.root, &v); err == nil {
		t.Fatal("directory admitted")
	}
}

// codex01's control: a symlink planted where the temp (or the final file) goes
// must never be followed: its target stays byte-identical and nothing is
// published through it.
func TestPublishNeverFollowsAPlantedSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "reviewer-owned")
	os.WriteFile(victim, []byte("keep"), 0o600)
	p := filepath.Join(dir, "m02-collect.json")
	os.Symlink(victim, fmt.Sprintf("%s.%d.tmp", p, os.Getpid())) // the r1 predictable temp name
	if err := publishEvidence(p, []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(dir, "faults.json")
	os.Symlink(victim, final)
	if err := publishEvidence(final, []byte(`{"b":2}`)); err == nil {
		t.Fatal("publish through an existing symlink admitted")
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatalf("planted symlink target changed: %q", b)
	}
	if b, _ := os.ReadFile(p); string(b) != `{"a":1}` {
		t.Fatalf("published %q", b)
	}
}

func TestSoakAdapterLoadStartChecksBootAndClock(t *testing.T) {
	for name, body := range map[string]string{
		"foreign boot": `{"boot_id":"boot-2","mono":10}`,
		"zero clock":   `{"boot_id":"boot-1","mono":0}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := testAdapter(t)
			os.WriteFile(filepath.Join(s.root, "soak-load-start.json"), []byte(body), 0o600)
			if _, err := s.LoadStart(context.Background()); err == nil {
				t.Fatal("admitted")
			}
		})
	}
	s := testAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := s.LoadStart(ctx); err == nil {
		t.Fatal("a missing witness must time out")
	}
	os.WriteFile(filepath.Join(s.root, "soak-load-start.json"), []byte(`{"boot_id":"boot-1","mono":12.5}`), 0o600)
	if m, err := s.LoadStart(context.Background()); err != nil || m != 12.5 {
		t.Fatalf("%v %v", m, err)
	}
}

func TestSoakAdapterAckChecksTheFullBinding(t *testing.T) {
	s := testAdapter(t)
	if err := s.Pause("E1", "volume", 100); err != nil {
		t.Fatal(err)
	}
	good := `{"run_id":"run-1","boot_id":"boot-1","episode":"E1","role":"volume","pause_mono":100,"observed_mono":100.1,"ack_mono":100.2,"inflight_sets":0,"pending_gets":[{"op":"7","submitted":99}]}`
	for name, body := range map[string]string{
		"wrong role": strings.Replace(good, `"role":"volume"`, `"role":"master"`, 1),
		"wrong run":  strings.Replace(good, `"run-1"`, `"run-2"`, 1),
		"no episode": strings.Replace(good, `"episode":"E1",`, ``, 1),
	} {
		t.Run(name, func(t *testing.T) {
			os.Remove(filepath.Join(s.root, "fault-ack-E1.json"))
			os.WriteFile(filepath.Join(s.root, "fault-ack-E1.json"), []byte(body), 0o600)
			if _, err := s.Ack(context.Background(), "E1"); err == nil {
				t.Fatal("admitted")
			}
		})
	}
	os.Remove(filepath.Join(s.root, "fault-ack-E1.json"))
	os.WriteFile(filepath.Join(s.root, "fault-ack-E1.json"), []byte(good), 0o600)
	a, err := s.Ack(context.Background(), "E1")
	if err != nil || a.PauseMono != 100 || len(a.PendingGETs) != 1 || a.PendingGETs[0].Op != "7" {
		t.Fatalf("%+v %v", a, err)
	}
}

// The Go-published pause and close are exactly what the worker's own
// read_bound accepts with the worker's binding (one contract, two languages).
func TestSoakAdapterFilesSatisfyTheWorkersReader(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	s := testAdapter(t)
	if err := s.Pause("E1", "volume", 100); err != nil {
		t.Fatal(err)
	}
	if err := s.Close("E1", 160); err != nil {
		t.Fatal(err)
	}
	worker, _ := filepath.Abs("worker")
	script := `import sys, pathlib; sys.path.insert(0, sys.argv[1]); import soak_load
root = pathlib.Path(sys.argv[2]); b = dict(run_id='run-1', boot_id='boot-1', episode='E1')
p = soak_load.read_bound(root / 'fault-pause-E1.json', b); c = soak_load.read_bound(root / 'fault-close-E1.json', dict(b, role='volume'))
assert p['role'] == 'volume' and p['pause_mono'] == 100 and c['end_mono'] == 160, (p, c)
print('ok')`
	out, err := exec.Command("python3", "-B", "-c", script, worker, s.root).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("worker reader refused the Go files: %v %s", err, out)
	}
}
