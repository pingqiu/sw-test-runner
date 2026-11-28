//go:build linux

package v3block

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pauseState is the state letter of /proc/<pid>/stat (T: stopped).
func pauseState(t *testing.T, pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	return strings.Fields(s[strings.LastIndex(s, ")")+2:])[0]
}

// m0-own.sh pause / continue: the store process found in the ownership
// ledger only, stopped, continued by the DETACHED bounded continue whatever
// the caller does, and by continue; their JSON answers carry the pid, the
// times and the bound. Red controls: a name not in the ledger, a bound out
// of range and a foreign token pause nothing and answer unknown (exit 1).
func TestOwnPauseIsBoundedAndFromTheLedger(t *testing.T) {
	needLinux(t)
	work := filepath.Join(t.TempDir(), "work")
	tok, code := own(t, nil, "claim", work)
	tok = strings.TrimSpace(tok)
	if code != 0 {
		t.Fatalf("claim: %d %q", code, tok)
	}
	pid := sleeper(t)
	if out, code := own(t, nil, "record", work, tok, "weed-filer", strconv.Itoa(pid)); code != 0 {
		t.Fatalf("record: %d %q", code, out)
	}
	out, code := own(t, nil, "pause", work, tok, "weed-filer", "2")
	var ans map[string]any
	if code != 0 || json.Unmarshal([]byte(strings.TrimSpace(out)), &ans) != nil || ans["outcome"] != "ok" ||
		ans["pid"] != float64(pid) || ans["bound_seconds"] != float64(2) || ans["paused_ns"].(float64) == 0 {
		t.Fatalf("pause: %d %q", code, out)
	}
	if st := pauseState(t, pid); st != "T" {
		t.Fatalf("not stopped after pause: %s", st)
	}
	// The detached continue ends the pause by its bound, with no continue call.
	deadline := time.Now().Add(6 * time.Second)
	for pauseState(t, pid) == "T" && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if st := pauseState(t, pid); st == "T" {
		t.Fatal("the bounded continue did not resume the process")
	}
	out, code = own(t, nil, "pause", work, tok, "weed-filer", "60")
	if code != 0 || pauseState(t, pid) != "T" {
		t.Fatalf("second pause: %d %q", code, out)
	}
	out, code = own(t, nil, "continue", work, tok, "weed-filer")
	ans = nil
	if code != 0 || json.Unmarshal([]byte(strings.TrimSpace(out)), &ans) != nil || ans["outcome"] != "ok" || ans["continued_ns"].(float64) == 0 || pauseState(t, pid) == "T" {
		t.Fatalf("continue: %d %q", code, out)
	}
	for name, args := range map[string][]string{
		"not in the ledger": {"pause", work, tok, "weed-volume", "2"},
		"bound 0":           {"pause", work, tok, "weed-filer", "0"},
		"bound 601":         {"pause", work, tok, "weed-filer", "601"},
		"a foreign token":   {"pause", work, strings.Repeat("a", 32), "weed-filer", "2"},
	} {
		out, code := own(t, nil, args...)
		ans = nil
		if code != 1 || json.Unmarshal([]byte(strings.TrimSpace(out)), &ans) != nil || ans["outcome"] != "unknown" || pauseState(t, pid) == "T" {
			t.Errorf("%s: %d %q", name, code, out)
		}
	}
}
