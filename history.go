package testrunner

// The run history (runner spec S2): one JSONL file per run, a record per
// operation invocation, per completion and per observation. A fault is not a
// kind of its own: it is an operation of the module Runner (its apply and its
// restore each an invoke/complete pair), so one mechanism records everything. The
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
	"sync/atomic"
	"time"
)

// Record kinds.
const (
	HistoryInvoke   = "invoke"
	HistoryComplete = "complete"
	HistoryObserve  = "observe"
	HistoryEnd      = "end" // the last record, written when the run closes its history
)

// Outcomes of a completion.
const (
	OutcomeOK      = "ok"
	OutcomeRefused = "refused"
	OutcomeUnknown = "unknown"
	// OutcomeRefusedEffect is a refusal with a documented effect: the
	// product refused, and its page states what the refused call changed
	// anyway (the completion's result.effect names it).
	OutcomeRefusedEffect = "refused_effect"
)

// HistoryRecord is one line of the history file.
type HistoryRecord struct {
	Seq    uint64         `json:"seq"`
	Kind   string         `json:"kind"`
	Of     uint64         `json:"of,omitempty"` // a completion: the seq of its invocation
	Client string         `json:"client,omitempty"`
	Op     string         `json:"op,omitempty"`   // Module.Action of the model-to-code trace
	Verb   string         `json:"verb,omitempty"` // the operator verb (create, write, ...)
	Volume string         `json:"volume,omitempty"`
	Args   map[string]any `json:"args,omitempty"`
	MonoNs int64          `json:"mono_ns"` // this process's monotonic clock since the file opened
	// FromNs, on an observation: the clock (as MonoNs) when its reading
	// began. MonoNs stamps the record after the reading, so the reading lies
	// between FromNs and MonoNs. Absent: the record does not say.
	FromNs int64 `json:"from_mono_ns,omitempty"`
	// FromWallNs, on an observation: this runner's wall clock (Unix ns) when
	// its reading began, taken with FromNs. The reading's end in wall time is
	// FromWallNs + (MonoNs - FromNs): a product fact printed in wall time (a
	// lease expiry) is compared with it. Absent: the record does not say.
	FromWallNs int64          `json:"from_wall_ns,omitempty"`
	Outcome    string         `json:"outcome,omitempty"`
	Result     map[string]any `json:"result,omitempty"`
}

// History appends records to the run's history file.
type History struct {
	mu    sync.Mutex
	f     *os.File
	path  string
	start time.Time
	next  uint64
	open  map[uint64]HistoryRecord
	// broken latches the first failed append: the file may end in a torn
	// line, so every later record is refused and the run fails to record.
	// A history with a hole never reaches a checker as if it were whole.
	broken error
	// onInvoke, when set (OnInvoke), is told of each invocation recorded.
	onInvoke func(HistoryRecord)
	// guards are the named flags a driver keeps on this history (Guard):
	// owned by it, they end with it.
	guards map[string]*atomic.Int32
}

// Guard is the history's own flag of that name, created on first use: a
// driver's "one operation at a time" guard lives in the History it guards,
// so nothing outside it keeps a History alive after it is closed and
// dropped.
func (h *History) Guard(name string) *atomic.Int32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.guards == nil {
		h.guards = map[string]*atomic.Int32{}
	}
	g := h.guards[name]
	if g == nil {
		g = new(atomic.Int32)
		h.guards[name] = g
	}
	return g
}

// OnInvoke sets f to be called with each invocation the history records,
// right after it is appended and outside the history's lock (nil: none). A
// driver uses it to time a fault from an operation's own invocation record,
// with no polling of the file.
func (h *History) OnInvoke(f func(HistoryRecord)) {
	h.mu.Lock()
	h.onInvoke = f
	h.mu.Unlock()
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

// Now reads the history's clock, as a record's MonoNs would be stamped now.
func (h *History) Now() int64 { return time.Since(h.start).Nanoseconds() }

// Invoke records the start of an operation and returns its seq.
//
// The callers' rule: record the invocation BEFORE the command is issued, and
// the completion (Complete) AFTER its answer, or the runner's own deadline,
// is in hand. An action that records both after the fact turns every
// concurrent history into a sequential one, and the checkers would judge an
// order that never happened.
func (h *History) Invoke(r HistoryRecord) (uint64, error) {
	if r.Op == "" || r.Verb == "" {
		return 0, errors.New("history: an invocation names its op and verb")
	}
	h.mu.Lock()
	r.Kind, r.Of, r.Outcome, r.Result = HistoryInvoke, 0, "", nil
	if err := h.appendLocked(&r); err != nil {
		h.mu.Unlock()
		return 0, err
	}
	h.open[r.Seq] = r
	f := h.onInvoke
	h.mu.Unlock()
	if f != nil {
		f(r)
	}
	return r.Seq, nil
}

// Complete records the end of invocation of with its outcome and result.
func (h *History) Complete(of uint64, outcome string, result map[string]any) error {
	if outcome != OutcomeOK && outcome != OutcomeRefused && outcome != OutcomeRefusedEffect && outcome != OutcomeUnknown {
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

// Observe records an observation (HEAD, the authority line, a listing). An
// observation decides nothing; the checkers read it.
func (h *History) Observe(kind string, r HistoryRecord) error {
	if kind != HistoryObserve {
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
//
// Close first appends the end record, the evidence that the history is the
// whole run: a file that stops without it (a crashed runner, a truncated
// copy) is incomplete, and no checker passes it. A latched history gets no end
// record.
func (h *History) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.f == nil {
		return nil
	}
	var endErr error
	if h.broken == nil {
		endErr = h.appendLocked(&HistoryRecord{Kind: HistoryEnd, Result: map[string]any{"open_invocations": len(h.open)}})
	}
	err := h.f.Close()
	h.f = nil
	return errors.Join(endErr, err)
}

// appendLocked stamps seq and the clock, writes one line and syncs it. Any
// write or sync failure latches the history broken.
func (h *History) appendLocked(r *HistoryRecord) error {
	if h.broken != nil {
		return fmt.Errorf("history: failed to record earlier, refusing every later record: %w", h.broken)
	}
	if h.f == nil {
		return errors.New("history: closed")
	}
	r.Seq, r.MonoNs = h.next, time.Since(h.start).Nanoseconds()
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("history: %w", err) // nothing was written; the file is whole
	}
	if _, err := h.f.Write(append(b, '\n')); err != nil {
		h.broken = fmt.Errorf("write of seq %d: %w", r.Seq, err)
		return fmt.Errorf("history: %w", h.broken)
	}
	if err := h.f.Sync(); err != nil {
		h.broken = fmt.Errorf("sync of seq %d: %w", r.Seq, err)
		return fmt.Errorf("history: %w", h.broken)
	}
	h.next++
	return nil
}
