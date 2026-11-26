package testrunner

// The run history (runner spec S2): one JSONL file per run, a record per
// operation invocation, per completion, per observation and per fault. The
// checkers judge this file alone, after the run.
//
// One clock: every record is stamped by THIS process's monotonic clock when
// the runner invokes, sees a completion or observes. No host clock is an
// ordering fact, so any two records compare.
//
// Sequence numbers are assigned here, in file order, so the file has no gap.
// A completion names the invocation it ends and is refused for an unknown or
// already completed one; an invocation left open is the checkers' finding.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Record kinds.
const (
	HistoryInvoke   = "invoke"
	HistoryComplete = "complete"
	HistoryObserve  = "observe"
	HistoryFault    = "fault"
)

// Outcomes of a completion.
const (
	OutcomeOK      = "ok"
	OutcomeRefused = "refused"
	OutcomeUnknown = "unknown"
)

// HistoryRecord is one line of the history file.
type HistoryRecord struct {
	Seq     uint64         `json:"seq"`
	Kind    string         `json:"kind"`
	Of      uint64         `json:"of,omitempty"` // a completion: the seq of its invocation
	Client  string         `json:"client,omitempty"`
	Op      string         `json:"op,omitempty"`   // Module.Action of the model-to-code trace
	Verb    string         `json:"verb,omitempty"` // the operator verb (create, write, ...)
	Volume  string         `json:"volume,omitempty"`
	Args    map[string]any `json:"args,omitempty"`
	MonoNs  int64          `json:"mono_ns"` // this process's monotonic clock since the file opened
	Outcome string         `json:"outcome,omitempty"`
	Result  map[string]any `json:"result,omitempty"`
}

// History appends records to the run's history file.
type History struct {
	mu    sync.Mutex
	f     *os.File
	path  string
	start time.Time
	next  uint64
	open  map[uint64]HistoryRecord
}

// OpenHistory creates the history file; an existing file is refused, so a
// run never appends to another run's history.
func OpenHistory(path string) (*History, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, fmt.Errorf("history: %w", err)
	}
	return &History{f: f, path: path, start: time.Now(), next: 1, open: map[uint64]HistoryRecord{}}, nil
}

// HistoryFile is the history's name in a run bundle.
const HistoryFile = "history.jsonl"

// OpenHistory creates the run's history file in the bundle directory.
func (b *RunBundle) OpenHistory() (*History, error) {
	return OpenHistory(filepath.Join(b.Dir, HistoryFile))
}

// Path is the history file's path.
func (h *History) Path() string { return h.path }

// Invoke records the start of an operation and returns its seq.
func (h *History) Invoke(r HistoryRecord) (uint64, error) {
	if r.Op == "" || r.Verb == "" {
		return 0, errors.New("history: an invocation names its op and verb")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	r.Kind, r.Of, r.Outcome, r.Result = HistoryInvoke, 0, "", nil
	if err := h.appendLocked(&r); err != nil {
		return 0, err
	}
	h.open[r.Seq] = r
	return r.Seq, nil
}

// Complete records the end of invocation of with its outcome and result.
func (h *History) Complete(of uint64, outcome string, result map[string]any) error {
	if outcome != OutcomeOK && outcome != OutcomeRefused && outcome != OutcomeUnknown {
		return fmt.Errorf("history: outcome %q", outcome)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	inv, ok := h.open[of]
	if !ok {
		return fmt.Errorf("history: no open invocation %d", of)
	}
	r := HistoryRecord{Kind: HistoryComplete, Of: of, Client: inv.Client, Op: inv.Op, Verb: inv.Verb, Volume: inv.Volume, Outcome: outcome, Result: result}
	if err := h.appendLocked(&r); err != nil {
		return err
	}
	delete(h.open, of)
	return nil
}

// Observe records an observation (HEAD, the authority line, a listing) or,
// with kind HistoryFault, a fault and its receipt.
func (h *History) Observe(kind string, r HistoryRecord) error {
	if kind != HistoryObserve && kind != HistoryFault {
		return fmt.Errorf("history: kind %q is not an observation", kind)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	r.Kind, r.Of, r.Outcome = kind, 0, ""
	return h.appendLocked(&r)
}

// Open reports the invocations not completed yet.
func (h *History) Open() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.open)
}

// Close closes the file. Open invocations stay open in the file: C0 reports
// them.
func (h *History) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.f == nil {
		return nil
	}
	err := h.f.Close()
	h.f = nil
	return err
}

// appendLocked stamps seq and the clock, writes one line and syncs it.
func (h *History) appendLocked(r *HistoryRecord) error {
	if h.f == nil {
		return errors.New("history: closed")
	}
	r.Seq, r.MonoNs = h.next, time.Since(h.start).Nanoseconds()
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("history: %w", err)
	}
	if _, err := h.f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("history: write: %w", err)
	}
	if err := h.f.Sync(); err != nil {
		return fmt.Errorf("history: sync: %w", err)
	}
	h.next++
	return nil
}
