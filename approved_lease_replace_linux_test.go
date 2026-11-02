//go:build linux

package testrunner

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// r3 (11-02): the worker renews by temp file + rename; a liveness read that spanned the rename failed with
// "renewal: record changed during capture" and stopped a healthy soak. A rename-replace is a new complete record,
// so the reader retries it a bounded number of times; an in-place change, a torn or a foreign record still fails.
func TestLiveLeaseRenewalReplacedDuringCapture(t *testing.T) {
	dir := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	now := float64(time.Now().UnixNano()) / 1e9
	put := func(suffix string, v any) {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, id+suffix+".json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	renew := filepath.Join(dir, id+"-renew.json")
	replace := func(v any) { // exactly the worker's renewal: a complete temp file renamed over the record
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		tmp := filepath.Join(dir, id+"-renew-x.tmp")
		if err = os.WriteFile(tmp, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err = os.Rename(tmp, renew); err != nil {
			t.Fatal(err)
		}
	}
	fresh := func(at float64) map[string]any {
		return map[string]any{"lease_id": id, "agent": "claude02", "renewed": at}
	}
	setup := func() {
		put("-acquire", map[string]any{"lease_id": id, "agent": "claude02", "started": now - 240})
		put("-renew", fresh(now-200))
	}
	defer func(h func(string)) { recordCaptured = h }(recordCaptured)
	run := func(on func(n int)) (LeaseObservation, int, error) {
		n := 0
		recordCaptured = func(name string) {
			if name == renew {
				n++
				on(n)
			}
		}
		r, err := ReadLiveLease(dir, id, "claude02")
		recordCaptured = func(string) {}
		return r, n, err
	}

	setup()
	r, n, err := run(func(n int) {
		if n == 1 {
			replace(fresh(now - 1))
		}
	})
	if err != nil || n != 2 || r.Stamp != now-1 {
		t.Fatalf("one rename-replace mid-capture must retry and read the new renewal: stamp %v captures %d err %v", r.Stamp, n, err)
	}

	setup()
	_, n, err = run(func(n int) { replace(fresh(now - float64(n))) })
	if err == nil || !errors.Is(err, errRecordReplaced) || n != ledgerCaptureAttempts {
		t.Fatalf("a record replaced on every attempt must fail after %d captures: captures %d err %v", ledgerCaptureAttempts, n, err)
	}

	setup()
	_, n, err = run(func(n int) {
		if n == 1 {
			f, err := os.OpenFile(renew, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			f.WriteString(" ")
			f.Close()
		}
	})
	if err == nil || errors.Is(err, errRecordReplaced) || !strings.Contains(err.Error(), "record changed during capture") || n != 1 {
		t.Fatalf("an in-place change is not a replacement and is never retried: captures %d err %v", n, err)
	}

	setup()
	_, n, err = run(func(n int) {
		if n == 1 {
			replace(map[string]any{"lease_id": id, "agent": "codex03", "renewed": now - 1})
		}
	})
	if err == nil || !strings.Contains(err.Error(), "foreign lease record") || n != 2 {
		t.Fatalf("a foreign record behind the rename must still be refused: captures %d err %v", n, err)
	}

	setup()
	_, n, err = run(func(n int) {
		if n == 1 {
			tmp := filepath.Join(dir, id+"-renew-x.tmp")
			os.WriteFile(tmp, []byte(`{"lease_id": "`+id+`", "agent": "clau`), 0600)
			os.Rename(tmp, renew)
		}
	})
	if err == nil || n != 2 {
		t.Fatalf("a torn record behind the rename must still be refused: captures %d err %v", n, err)
	}
}
