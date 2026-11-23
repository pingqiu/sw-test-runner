// Package hostloss holds the pure judges of the host-loss drill: the writer ledger (per-write LSN evidence), the
// image check by LSN (F5), the lost-range readout and the partition stop bound. They read bytes and return a Row;
// they never touch a host. Actions wire them to the runner.
package hostloss

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Outcome of one judged row. NotCounted means "no failure, but the rep carries no exact evidence for this row".
type Outcome string

const (
	Pass       Outcome = "PASS"
	Fail       Outcome = "FAIL"
	NotCounted Outcome = "NOT_COUNTED"
)

// Row is one judged row: its name, outcome and the evidence text the verdict prints.
type Row struct {
	Name    string
	Outcome Outcome
	Text    string
}

func (r Row) String() string { return fmt.Sprintf("%s %s: %s", r.Outcome, r.Name, r.Text) }

// Record is one ledger line. The writer appends, per write:
//
//	I seq block sha h0 mono wall      intent (before the write; h0 = HeadLSN read before it)
//	A seq block sha h0 h1 mono wall   acknowledged (h1 = HeadLSN read after the ack)
//	E seq block errno mono wall       failed
//
// h0/h1 are "NA" when the status read gave no HeadLSN.
type Record struct {
	Kind        byte // 'I', 'A' or 'E'
	Seq, Block  int64
	SHA         string
	H0, H1      int64
	H0OK, H1OK  bool
	Mono, Wall  int64
	Errno       int64
	Line        int
	lsnInferred bool
}

// Exact reports whether an acknowledged write has an exact LSN: H1 = H0 + 1.
func (r *Record) Exact() bool { return r.Kind == 'A' && r.H0OK && r.H1OK && r.H1 == r.H0+1 }

var (
	decimal = regexp.MustCompile(`^[0-9]+$`)
	hexSHA  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func num(s string) (int64, bool) {
	if !decimal.MatchString(s) {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	return v, err == nil
}

func numOrNA(s string) (int64, bool, bool) { // value, present, wellformed
	if s == "NA" {
		return 0, false, true
	}
	v, ok := num(s)
	return v, ok, ok
}

// parseLine parses one ledger line against the writer's schema; ok is false for a malformed line.
func parseLine(line string) (Record, bool) {
	p := strings.Split(line, "\t")
	var r Record
	if len(p) == 0 || len(p[0]) != 1 {
		return r, false
	}
	r.Kind = p[0][0]
	ints := func(idx ...int) ([]int64, bool) {
		out := make([]int64, len(idx))
		for i, k := range idx {
			v, ok := num(p[k])
			if !ok {
				return nil, false
			}
			out[i] = v
		}
		return out, true
	}
	switch {
	case r.Kind == 'I' && len(p) == 7 && hexSHA.MatchString(p[3]):
		v, ok := ints(1, 2, 5, 6)
		h0, h0ok, wf := numOrNA(p[4])
		if !ok || !wf {
			return r, false
		}
		r.Seq, r.Block, r.SHA, r.H0, r.H0OK, r.Mono, r.Wall = v[0], v[1], p[3], h0, h0ok, v[2], v[3]
	case r.Kind == 'A' && len(p) == 8 && hexSHA.MatchString(p[3]):
		v, ok := ints(1, 2, 6, 7)
		h0, h0ok, wf0 := numOrNA(p[4])
		h1, h1ok, wf1 := numOrNA(p[5])
		if !ok || !wf0 || !wf1 {
			return r, false
		}
		r.Seq, r.Block, r.SHA, r.H0, r.H0OK, r.H1, r.H1OK, r.Mono, r.Wall = v[0], v[1], p[3], h0, h0ok, h1, h1ok, v[2], v[3]
	case r.Kind == 'E' && len(p) == 6:
		v, ok := ints(1, 2, 3, 4, 5)
		if !ok {
			return r, false
		}
		r.Seq, r.Block, r.Errno, r.Mono, r.Wall = v[0], v[1], v[2], v[3], v[4]
	default:
		return r, false
	}
	return r, true
}

// Window is an inclusive wall-clock window in ns (the race window of the one forgiven write).
type Window struct{ Lo, Hi int64 }

// Scan is one pass over a ledger.
type Scan struct {
	Acked, Exact, ChainBreaks, Malformed int
	TornTail                             bool // final mode: an unterminated last line (incomplete evidence)
	Gaps                                 int  // intents with no A/E record, other than the last intent
	Raced                                bool // final mode with a window: the last A is "acked, no LSN (kill raced the probe)"
	Snapshot                             []byte
	LastA                                *Record
}

// ScanLive scans while the writer may still be appending: only the bytes up to the LAST newline are read, so a
// half-written tail line is never counted (neither as acked nor as non-exact).
func ScanLive(b []byte) Scan {
	return scan(b[:bytes.LastIndexByte(b, '\n')+1], false, nil)
}

// ScanFinal scans after the writer has stopped: the whole file; any unterminated last line is reported as TornTail
// and never counted. With race != nil, the LAST A of the ledger, when H0 is readable, H1 is NA and its wall time is
// in race, is Raced: counted in Acked, never in Exact. At most that one write; any other NA stays non-exact.
func ScanFinal(b []byte, race *Window) Scan {
	torn := len(b) > 0 && b[len(b)-1] != '\n'
	if torn {
		b = b[:bytes.LastIndexByte(b, '\n')+1]
	}
	s := scan(b, true, race)
	s.TornTail = torn
	return s
}

func scan(b []byte, final bool, race *Window) Scan {
	s := Scan{Snapshot: b}
	var prev *int64
	var intents []int64
	resolved := map[int64]bool{}
	for _, raw := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if raw == "" {
			continue
		}
		r, ok := parseLine(raw)
		if !ok {
			s.Malformed++
			continue
		}
		switch r.Kind {
		case 'I':
			intents = append(intents, r.Seq)
			continue
		case 'E':
			resolved[r.Seq] = true
			continue
		}
		resolved[r.Seq] = true
		s.Acked++
		rc := r
		s.LastA = &rc
		if r.Exact() {
			s.Exact++
		}
		if prev != nil && (!r.H0OK || r.H0 != *prev) {
			s.ChainBreaks++
		}
		if r.H1OK {
			h := r.H1
			prev = &h
		} else {
			prev = nil
		}
	}
	if len(intents) > 0 {
		for _, q := range intents[:len(intents)-1] {
			if !resolved[q] {
				s.Gaps++
			}
		}
	}
	if final && race != nil && s.LastA != nil && s.LastA.H0OK && !s.LastA.H1OK &&
		race.Lo <= s.LastA.Wall && s.LastA.Wall <= race.Hi {
		s.Raced = true
	}
	return s
}

// MinRow1Acked is the least number of acknowledged writes row 1 needs in its first window.
const MinRow1Acked = 100

// JudgeRow1 judges d1-lsn-exact on a live scan: >= MinRow1Acked acked, every one exact, no chain break, no
// malformed line.
func JudgeRow1(s Scan) Row {
	ok := s.Acked >= MinRow1Acked && s.Exact == s.Acked && s.ChainBreaks == 0 && s.Malformed == 0
	return Row{"d1-lsn-exact", pf(ok), fmt.Sprintf("%d acked writes, %d with an exact LSN (H1 = H0 + 1), %d chain breaks (H0 = previous H1), %d malformed lines",
		s.Acked, s.Exact, s.ChainBreaks, s.Malformed)}
}

// JudgeFinal judges d1-ledger-final on a final scan once the writer is proven stopped: every acked write exact
// (except at most the one Raced write), no chain break, no malformed line, no torn tail, every intent but the last
// resolved, and at least row1 acked writes.
func JudgeFinal(s Scan, row1 int, race *Window) Row {
	raced := 0
	if s.Raced {
		raced = 1
	}
	ok := s.Acked >= row1 && s.Exact+raced == s.Acked && s.ChainBreaks == 0 && s.Malformed == 0 && !s.TornTail && s.Gaps == 0
	w := "none"
	if race != nil {
		w = fmt.Sprintf("%d..%d", race.Lo, race.Hi)
	}
	return Row{"d1-ledger-final", pf(ok), fmt.Sprintf("%d acked writes in the whole ledger, %d exact, %d acked, no LSN (kill raced the probe; window %s), %d chain breaks, %d malformed lines, torn tail %t, %d unresolved intents before the last (row 1 counted %d)",
		s.Acked, s.Exact, raced, w, s.ChainBreaks, s.Malformed, s.TornTail, s.Gaps, row1)}
}

// RaceBefore is the race window of a SIGKILL at killNs: the one second before it.
func RaceBefore(killNs int64) *Window {
	lo := killNs - 1_000_000_000
	if lo < 0 {
		lo = 0
	}
	return &Window{lo, killNs}
}

// RaceAroundSecond is the race window of a fence line whose journal time has whole-second resolution: the write
// must be acked within 1 s before the line, whose true time lies in [sec, sec+1), so [sec-1 s, sec+1 s).
func RaceAroundSecond(sec int64) *Window {
	return &Window{(sec - 1) * 1_000_000_000, (sec+1)*1_000_000_000 - 1}
}

func pf(ok bool) Outcome {
	if ok {
		return Pass
	}
	return Fail
}

// Ledger is a parsed, consistency-checked ledger: seq -> record (the A or E replaces its I).
type Ledger struct {
	BySeq   map[int64]*Record
	ByBlock map[int64]*Record
}

// ParseLedger parses a whole ledger strictly: a malformed line, a second intent for one seq, a block written twice,
// or an A/E without its matching intent is an error (the evidence is unusable).
func ParseLedger(b []byte) (*Ledger, error) {
	l := &Ledger{BySeq: map[int64]*Record{}, ByBlock: map[int64]*Record{}}
	for n, raw := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if raw == "" {
			continue
		}
		r, ok := parseLine(raw)
		if !ok {
			return nil, fmt.Errorf("line %d: malformed record %q", n+1, trunc(raw, 60))
		}
		r.Line = n + 1
		switch r.Kind {
		case 'I':
			if _, dup := l.BySeq[r.Seq]; dup {
				return nil, fmt.Errorf("line %d: second intent for seq %d", n+1, r.Seq)
			}
			if _, dup := l.ByBlock[r.Block]; dup {
				return nil, fmt.Errorf("line %d: block %d written twice", n+1, r.Block)
			}
			rc := r
			l.BySeq[r.Seq], l.ByBlock[r.Block] = &rc, &rc
		default:
			i := l.BySeq[r.Seq]
			if i == nil || i.Kind != 'I' || i.Block != r.Block || (r.Kind == 'A' && i.SHA != r.SHA) {
				return nil, fmt.Errorf("line %d: %c without its matching intent (seq %d)", n+1, r.Kind, r.Seq)
			}
			if r.Kind == 'E' {
				r.SHA = i.SHA
			}
			*i = r
		}
	}
	return l, nil
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
