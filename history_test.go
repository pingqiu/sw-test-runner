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
	if len(rs) != 3 {
		t.Fatalf("%d records", len(rs))
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
	// The open invocation stays in the file, without a completion.
	rs := readHistory(t, path)
	last := rs[len(rs)-1]
	if last.Kind != HistoryInvoke {
		t.Errorf("last record %+v", last)
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
	if len(rs) != 16*20*2 {
		t.Fatalf("%d records", len(rs))
	}
	for i, r := range rs {
		if r.Seq != uint64(i+1) {
			t.Fatalf("record %d has seq %d", i, r.Seq)
		}
	}
}
