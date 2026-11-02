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

// r3 and r4 (11-02) stopped a healthy soak at the first lease renewal: a liveness read of the renew record saw it
// change during capture. On M01's CIFS mount a freshly renamed record keeps its inode while its mtime/ctime settle
// for about a second (the "cifs-late" case worker_lease_settle_linux_test.go already models for the writer), so the
// ledger reader treats any capture change as transient: it re-captures after a pause, within the 6 s settle bound,
// and accepts only a capture that stayed unchanged and decodes as this lease's own record.
func TestLiveLeaseLedgerCaptureSettles(t *testing.T) {
	dir := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	now := float64(time.Now().UnixNano()) / 1e9
	renew := filepath.Join(dir, id+"-renew.json")
	write := func(name string, v any) {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(name, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	replace := func(raw []byte) { // exactly the worker's renewal: a complete temp file renamed over the record
		t.Helper()
		tmp := filepath.Join(dir, id+"-renew-x.tmp")
		if err := os.WriteFile(tmp, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, renew); err != nil {
			t.Fatal(err)
		}
	}
	fresh := func(at float64) []byte {
		raw, _ := json.Marshal(map[string]any{"lease_id": id, "agent": "claude02", "renewed": at})
		return raw
	}
	settle := func(n int) { // cifs-late: the same inode, its mtime moves after the read
		t.Helper()
		info, err := os.Lstat(renew)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.Chtimes(renew, info.ModTime(), info.ModTime().Add(time.Duration(42+n)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	defer func(h func(string), n func() time.Time, s func(time.Duration)) {
		recordCaptured, ledgerNow, ledgerSleep = h, n, s
	}(recordCaptured, ledgerNow, ledgerSleep)
	type outcome struct {
		r        LeaseObservation
		captures int
		elapsed  time.Duration
		err      error
	}
	var clock time.Time
	var onSleep func()
	run := func(on func(n int)) outcome {
		write(filepath.Join(dir, id+"-acquire.json"), map[string]any{"lease_id": id, "agent": "claude02", "started": now - 240})
		if err := os.WriteFile(renew, fresh(now-200), 0600); err != nil {
			t.Fatal(err)
		}
		clock = time.Unix(1000, 0)
		onSleep = func() {}
		ledgerNow = func() time.Time { return clock }
		ledgerSleep = func(d time.Duration) {
			if d != 250*time.Millisecond {
				t.Fatal("invalid re-capture pause", d)
			}
			clock = clock.Add(d)
			onSleep()
		}
		n := 0
		recordCaptured = func(name string) {
			if name == renew {
				n++
				on(n)
			}
		}
		r, err := ReadLiveLease(dir, id, "claude02")
		recordCaptured = func(string) {}
		return outcome{r, n, clock.Sub(time.Unix(1000, 0)), err}
	}

	o := run(func(n int) {
		if n == 1 {
			settle(n)
		}
	})
	if o.err != nil || o.captures != 2 || o.r.Stamp != now-200 {
		t.Fatalf("cifs-late (same inode, mtime settles once) must re-capture and pass: %+v", o)
	}
	o = run(func(n int) {
		if n <= 3 {
			settle(n)
		}
	})
	if o.err != nil || o.captures != 4 {
		t.Fatalf("metadata settling over several captures must pass once stable: %+v", o)
	}
	o = run(func(n int) {
		if n == 1 {
			replace(fresh(now - 1))
		}
	})
	if o.err != nil || o.captures != 2 || o.r.Stamp != now-1 {
		t.Fatalf("a rename-replace mid-capture must re-capture and read the new renewal: %+v", o)
	}
	o = run(func(n int) {
		if n == 1 {
			replace(fresh(now - 1))
		} else if n == 2 {
			settle(n)
		}
	})
	if o.err != nil || o.captures != 3 || o.r.Stamp != now-1 {
		t.Fatalf("a replace followed by CIFS metadata settling must pass: %+v", o)
	}
	o = run(settle)
	if o.err == nil || !errors.Is(o.err, errRecordChanged) || o.captures != 24 || o.elapsed != 5750*time.Millisecond {
		t.Fatalf("metadata that never settles must fail within the 6 s bound: %+v", o)
	}
	o = run(func(n int) { replace(fresh(now - float64(n))) })
	if o.err == nil || !errors.Is(o.err, errRecordReplaced) || o.captures != 24 || o.elapsed != 5750*time.Millisecond {
		t.Fatalf("a record replaced on every capture must fail within the 6 s bound: %+v", o)
	}
	o = run(func(n int) {
		if n == 1 {
			raw, _ := json.Marshal(map[string]any{"lease_id": id, "agent": "codex03", "renewed": now - 1})
			replace(raw)
		}
	})
	if o.err == nil || !strings.Contains(o.err.Error(), "foreign lease record") || o.captures != 2 {
		t.Fatalf("a foreign record behind the change must still be refused: %+v", o)
	}
	o = run(func(n int) {
		if n == 1 {
			replace([]byte(`{"lease_id": "` + id + `", "agent": "clau`))
		}
	})
	if o.err == nil || o.captures != 2 {
		t.Fatalf("a torn record behind the change must still be refused, without further retries: %+v", o)
	}

	// codex01 R1: a record that vanished after its read is unprovable, never "settling", even if it comes back.
	moved := renew + ".moved"
	o = run(func(n int) {
		if n == 1 {
			if err := os.Rename(renew, moved); err != nil {
				t.Fatal(err)
			}
			onSleep = func() { os.Rename(moved, renew) }
		}
	})
	if o.err == nil || errors.Is(o.err, errRecordChanged) || errors.Is(o.err, errRecordReplaced) || o.captures != 1 || o.elapsed != 0 {
		t.Fatalf("a failed post-read stat must be terminal: %+v", o)
	}
	os.Remove(moved)
	acquired, err := os.Lstat(filepath.Join(dir, id+"-acquire.json"))
	if err != nil {
		t.Fatal(err)
	}
	if e := recordChangeError(acquired, acquired, nil, 1<<20+1); e == nil || errors.Is(e, errRecordChanged) || errors.Is(e, errRecordReplaced) {
		t.Fatalf("an oversized read must be terminal: %v", e)
	}
	if e := recordChangeError(acquired, nil, os.ErrNotExist, 1); e == nil || errors.Is(e, errRecordChanged) || errors.Is(e, errRecordReplaced) {
		t.Fatalf("a failed stat must be terminal: %v", e)
	}
	if e := recordChangeError(acquired, acquired, nil, 1); e != nil {
		t.Fatalf("an unchanged record is no change: %v", e)
	}

	// codex01 R2: nothing is accepted, and no capture starts, past the settle deadline.
	o = run(func(n int) { clock = clock.Add(7 * time.Second) })
	if o.err == nil || !strings.Contains(o.err.Error(), "settle bound") || o.captures != 1 {
		t.Fatalf("an unchanged capture that ended past the deadline must not be accepted: %+v", o)
	}
	o = run(func(n int) {
		settle(n)
		onSleep = func() { clock = clock.Add(7 * time.Second) }
	})
	if o.err == nil || !strings.Contains(o.err.Error(), "settle bound") || !errors.Is(o.err, errRecordChanged) || o.captures != 1 {
		t.Fatalf("an overslept pause must not start another capture: %+v", o)
	}

	// Every other caller keeps the strict capture: one change is one refusal.
	recordCaptured = func(name string) { settle(1) }
	if _, err := snapshotRecord(renew, nil, false); err == nil || !errors.Is(err, errRecordChanged) {
		t.Fatalf("snapshotRecord itself must still refuse a change during capture: %v", err)
	}
	recordCaptured = func(string) {}
}
