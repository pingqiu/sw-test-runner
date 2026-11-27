package testrunner

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func readHistory(t *testing.T, path string) []HistoryRecord {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []HistoryRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r HistoryRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("line %d: %v", len(out)+1, err)
		}
		out = append(out, r)
	}
	return out
}

func TestHistoryRecordsInFileOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	h, err := OpenHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := h.Invoke(HistoryRecord{Client: "c1", Op: "HolderInterface.Write", Verb: "write", Volume: "v1", Args: map[string]any{"lba": 0}})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Observe(HistoryObserve, HistoryRecord{Volume: "v1", Result: map[string]any{"head_gen": 3}}); err != nil {
		t.Fatal(err)
	}
	if err := h.Complete(a, OutcomeOK, map[string]any{"sha256": "x"}); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	rs := readHistory(t, path)
	if len(rs) != 4 || rs[3].Kind != HistoryEnd {
		t.Fatalf("%d records, last %+v; want 3 and the end record", len(rs), rs[len(rs)-1])
	}
	for i, r := range rs {
		if r.Seq != uint64(i+1) {
			t.Errorf("record %d has seq %d", i, r.Seq)
		}
		if i > 0 && r.MonoNs < rs[i-1].MonoNs {
			t.Errorf("clock went back at record %d", i)
		}
	}
	c := rs[2]
	if c.Kind != HistoryComplete || c.Of != a || c.Op != "HolderInterface.Write" || c.Volume != "v1" || c.Outcome != OutcomeOK {
		t.Errorf("completion %+v", c)
	}
}

func TestHistoryRefuses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.jsonl")
	h, err := OpenHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenHistory(path); err == nil {
		t.Error("a second history on the same file was opened")
	}
	if _, err := h.Invoke(HistoryRecord{Verb: "write"}); err == nil {
		t.Error("an invocation without its op was recorded")
	}
	s, _ := h.Invoke(HistoryRecord{Op: "HolderInterface.Write", Verb: "write"})
	if err := h.Complete(s, "maybe", nil); err == nil {
		t.Error("an outcome outside ok/refused/unknown was recorded")
	}
	if err := h.Complete(s+7, OutcomeOK, nil); err == nil {
		t.Error("a completion of an unknown invocation was recorded")
	}
	if err := h.Complete(s, OutcomeUnknown, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.Complete(s, OutcomeOK, nil); err == nil {
		t.Error("a second completion was recorded")
	}
	if err := h.Observe(HistoryInvoke, HistoryRecord{}); err == nil {
		t.Error("an invocation was recorded as an observation")
	}
	open, _ := h.Invoke(HistoryRecord{Op: "HolderInterface.Write", Verb: "write"})
	if h.Open() != 1 {
		t.Errorf("open invocations: %d", h.Open())
	}
	_ = open
	_ = h.Close()
	if _, err := h.Invoke(HistoryRecord{Op: "HolderInterface.Write", Verb: "write"}); err == nil {
		t.Error("a closed history accepted a record")
	}
	// The open invocation stays in the file, without a completion; the end
	// record says one was open.
	rs := readHistory(t, path)
	inv, end := rs[len(rs)-2], rs[len(rs)-1]
	if inv.Kind != HistoryInvoke || end.Kind != HistoryEnd || end.Result["open_invocations"] != float64(1) {
		t.Errorf("last records %+v %+v", inv, end)
	}
}

// After a failed append the history refuses every later record, even once
// the file would accept writes again: a torn line is never followed.
func TestHistoryLatchesAFailedAppend(t *testing.T) {
	dir := t.TempDir()
	h, err := OpenHistory(filepath.Join(dir, "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Invoke(HistoryRecord{Op: "HolderInterface.Write", Verb: "write"}); err != nil {
		t.Fatal(err)
	}
	good := h.f
	_ = good.Close() // the next write fails
	if _, err := h.Invoke(HistoryRecord{Op: "HolderInterface.Write", Verb: "write"}); err == nil {
		t.Fatal("a write to a failed file was accepted")
	}
	f, err := os.OpenFile(filepath.Join(dir, "other.jsonl"), os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h.mu.Lock()
	h.f = f // a writable file again
	h.mu.Unlock()
	for name, call := range map[string]func() error{
		"invoke":   func() error { _, e := h.Invoke(HistoryRecord{Op: "HolderInterface.Write", Verb: "write"}); return e },
		"complete": func() error { return h.Complete(1, OutcomeOK, nil) },
		"observe":  func() error { return h.Observe(HistoryObserve, HistoryRecord{}) },
	} {
		if err := call(); err == nil {
			t.Errorf("%s accepted after a failed append", name)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "other.jsonl")); len(b) != 0 {
		t.Errorf("records written after the failure: %q", b)
	}
	// A latched history gets no end record: it never looks whole.
	_ = h.Close()
	if b, _ := os.ReadFile(filepath.Join(dir, "other.jsonl")); len(b) != 0 {
		t.Errorf("the latched history was closed with an end record: %q", b)
	}
}

// Concurrent writers still produce a gapless sequence in file order.
func TestHistoryConcurrentAppendsHaveNoGap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	h, err := OpenHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				s, err := h.Invoke(HistoryRecord{Op: "HolderInterface.Write", Verb: "write"})
				if err != nil {
					t.Error(err)
					return
				}
				if err := h.Complete(s, OutcomeOK, nil); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	_ = h.Close()
	rs := readHistory(t, path)
	if len(rs) != 16*20*2+1 || rs[len(rs)-1].Kind != HistoryEnd {
		t.Fatalf("%d records", len(rs))
	}
	for i, r := range rs {
		if r.Seq != uint64(i+1) {
			t.Fatalf("record %d has seq %d", i, r.Seq)
		}
	}
}

// The four outcomes complete; any other value is refused.
func TestHistoryOutcomes(t *testing.T) {
	h, err := OpenHistory(filepath.Join(t.TempDir(), "h.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for _, o := range []string{OutcomeOK, OutcomeRefused, OutcomeRefusedEffect, OutcomeUnknown} {
		seq, err := h.Invoke(HistoryRecord{Op: "M.A", Verb: "v"})
		if err != nil {
			t.Fatal(err)
		}
		if err := h.Complete(seq, o, map[string]any{"effect": map[string]any{"part": "workspace"}}); err != nil {
			t.Errorf("%s: %v", o, err)
		}
	}
	seq, _ := h.Invoke(HistoryRecord{Op: "M.A", Verb: "v"})
	if err := h.Complete(seq, "refused_maybe", nil); err == nil {
		t.Error("an unknown outcome value completed")
	}
}
