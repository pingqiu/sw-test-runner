package hostloss

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Fixtures in testdata/ are slices of REAL writer ledgers and product output from lab reps (host names and run ids
// replaced by generic ones; ledgers carry only numbers and hashes):
//
//	torn-live.tsv          the ledger as the live row-1 read saw it: its last line half-written (no newline)
//	complete-final.tsv     a complete ledger after the writer stopped (includes failed writes and NA intents)
//	raced-kill.tsv         the kill raced the writer's H1 probe: the LAST ack has H1 = NA, 9.22 ms before the kill
//	partition-ledger.tsv   a partition rep's ledger tail; partition-journal.txt its old owner's lease lines
//	grant.txt, range.txt   a forced move's grant output and lost-range readout
//
// Expected counts equal the reference implementation's on the same bytes.

const raceKillNs = 1795408847399928288 // the kill receipt time of the raced-kill rep

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type counts struct {
	acked, exact, breaks, malformed, gaps int
	torn, raced                           bool
}

func countsOf(s Scan) counts {
	return counts{s.Acked, s.Exact, s.ChainBreaks, s.Malformed, s.Gaps, s.TornTail, s.Raced}
}

func TestRealLedgersScan(t *testing.T) {
	for _, c := range []struct {
		file        string
		live, final counts
	}{
		{"torn-live.tsv", counts{acked: 199, exact: 199}, counts{acked: 199, exact: 199, torn: true}},
		{"complete-final.tsv", counts{acked: 187, exact: 187}, counts{acked: 187, exact: 187}},
		{"raced-kill.tsv", counts{acked: 199, exact: 198}, counts{acked: 199, exact: 198}},
		{"partition-ledger.tsv", counts{acked: 29, exact: 29}, counts{acked: 29, exact: 29}},
	} {
		b := fixture(t, c.file)
		if got := countsOf(ScanLive(b)); got != c.live {
			t.Errorf("%s live: %+v, want %+v", c.file, got, c.live)
		}
		if got := countsOf(ScanFinal(b, nil)); got != c.final {
			t.Errorf("%s final: %+v, want %+v", c.file, got, c.final)
		}
	}
}

func TestRow1ReadsCompleteLinesOnly(t *testing.T) {
	r := JudgeRow1(ScanLive(fixture(t, "torn-live.tsv")))
	if r.Outcome != Pass || !strings.Contains(r.Text, "199 acked writes, 199 with an exact LSN") {
		t.Fatalf("the half-written tail line must not be read: %s", r)
	}
}

func TestRow1Failures(t *testing.T) {
	b := string(fixture(t, "complete-final.tsv"))
	for _, c := range []struct {
		name string
		edit func(string) string
		want string
	}{
		{"non-exact", func(s string) string { return setLastA(s, 5, func(v string) string { return inc(v) }) }, "186 with an exact LSN"},
		{"garbage-h0", func(s string) string { return setLastA(s, 4, func(string) string { return "garbage" }) }, "1 malformed lines"},
		{"chain-break", func(s string) string { return shiftLastA(s, 5) }, "1 chain breaks"},
		{"too-few", func(s string) string { return firstLines(s, 100) }, "50 acked writes"},
	} {
		r := JudgeRow1(ScanLive([]byte(c.edit(b))))
		if r.Outcome != Fail || !strings.Contains(r.Text, c.want) {
			t.Errorf("%s: %s (want FAIL with %q)", c.name, r, c.want)
		}
	}
}

func TestFinalLedger(t *testing.T) {
	full := string(fixture(t, "complete-final.tsv"))
	for _, c := range []struct {
		name string
		b    string
		row1 int
		want Outcome
	}{
		{"complete", full, 150, Pass},
		{"fewer-than-row1", full, 188, Fail},
		{"torn-tail", string(fixture(t, "torn-live.tsv")), 150, Fail},
		{"unterminated-well-formed-last-line", strings.TrimSuffix(full, "\n"), 150, Fail},
		{"non-exact", setLastA(full, 5, inc), 150, Fail},
		{"chain-break", shiftLastA(full, 5), 150, Fail},
		{"malformed", setLastA(full, 4, func(string) string { return "x1" }), 150, Fail},
		{"unresolved-intent", insertAfterFirstA(full, "I\t900000\t900000\t"+strings.Repeat("0", 64)+"\t1\t1\t1"), 150, Fail},
	} {
		if r := JudgeFinal(ScanFinal([]byte(c.b), nil), c.row1, nil); r.Outcome != c.want {
			t.Errorf("%s: %s, want %s", c.name, r, c.want)
		}
	}
}

// The kill raced the writer's H1 probe: at most ONE acked write without an LSN is forgiven, only the LAST A, with H0
// readable, acked within 1 s before the kill. Anything else stays FAIL.
func TestRacedKill(t *testing.T) {
	b := string(fixture(t, "raced-kill.tsv"))
	last := ScanFinal([]byte(b), nil).LastA
	win := RaceBefore(raceKillNs)
	r := JudgeFinal(ScanFinal([]byte(b), win), 150, win)
	if r.Outcome != Pass || !strings.Contains(r.Text, "198 exact, 1 acked, no LSN (kill raced the probe") {
		t.Fatalf("real raced kill: %s", r)
	}
	for _, c := range []struct {
		name string
		b    string
		win  *Window
	}{
		{"no-race-instant", b, nil},
		{"ack-more-than-1s-before-kill", b, RaceBefore(last.Wall + 1_000_000_001)},
		{"ack-after-the-kill", b, RaceBefore(last.Wall - 1)},
		{"na-not-last", setNthA(exactLastA(b), 10, 5, "NA"), RaceBefore(raceKillNs)},
		// an NA on the second-to-last ack, INSIDE the window, the last ack exact: still not the last A
		{"na-second-to-last-in-window", setNthA(exactLastA(b), len(aIndexes(lines(b)))-2, 5, "NA"), RaceBefore(raceKillNs)},
		{"two-na", setNthA(b, 10, 5, "NA"), RaceBefore(raceKillNs)},
		{"h0-unreadable", setLastA(b, 4, func(string) string { return "NA" }), RaceBefore(raceKillNs)},
	} {
		if r := JudgeFinal(ScanFinal([]byte(c.b), c.win), 150, c.win); r.Outcome != Fail {
			t.Errorf("%s: %s, want FAIL", c.name, r)
		}
	}
}

func TestRaceWindows(t *testing.T) {
	if w := RaceBefore(5_000_000_000); w.Lo != 4_000_000_000 || w.Hi != 5_000_000_000 {
		t.Errorf("RaceBefore: %+v", w)
	}
	if w := RaceBefore(10); w.Lo != 0 {
		t.Errorf("RaceBefore clamps at 0: %+v", w)
	}
	if w := RaceAroundSecond(100); w.Lo != 99_000_000_000 || w.Hi != 100_999_999_999 {
		t.Errorf("RaceAroundSecond: %+v", w)
	}
}

func TestParseLedgerRefusesInconsistentEvidence(t *testing.T) {
	sha := strings.Repeat("a", 64)
	i := func(seq, blk string) string { return "I\t" + seq + "\t" + blk + "\t" + sha + "\t1\t1\t1\n" }
	a := func(seq, blk, s string) string { return "A\t" + seq + "\t" + blk + "\t" + s + "\t1\t2\t1\t1\n" }
	for name, b := range map[string]string{
		"second-intent":          i("1", "1") + i("1", "2"),
		"block-written-twice":    i("1", "1") + i("2", "1"),
		"ack-without-intent":     a("3", "3", sha),
		"ack-other-content":      i("1", "1") + a("1", "1", strings.Repeat("b", 64)),
		"malformed":              "A\t1\n",
		"failure-without-intent": "E\t4\t4\t5\t1\t1\n",
	} {
		if _, err := ParseLedger([]byte(b)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if l, err := ParseLedger([]byte(i("1", "1") + a("1", "1", sha))); err != nil || !l.BySeq[1].Exact() {
		t.Errorf("consistent ledger: %v", err)
	}
}

// ---- ledger editing helpers (tab-separated fields; field 0 = kind)

func lines(s string) []string { return strings.Split(strings.TrimSuffix(s, "\n"), "\n") }
func join(l []string) string  { return strings.Join(l, "\n") + "\n" }

func inc(v string) string      { return formatInt(atoi(v) + 1) }
func formatInt(v int64) string { return strconv.FormatInt(v, 10) }

func aIndexes(l []string) []int {
	var out []int
	for i, x := range l {
		if strings.HasPrefix(x, "A\t") {
			out = append(out, i)
		}
	}
	return out
}

func setField(line string, f int, fn func(string) string) string {
	p := strings.Split(line, "\t")
	p[f] = fn(p[f])
	return strings.Join(p, "\t")
}

func setLastA(s string, f int, fn func(string) string) string {
	l := lines(s)
	ix := aIndexes(l)
	k := ix[len(ix)-1]
	l[k] = setField(l[k], f, fn)
	return join(l)
}

func setNthA(s string, n, f int, v string) string {
	l := lines(s)
	k := aIndexes(l)[n]
	l[k] = setField(l[k], f, func(string) string { return v })
	return join(l)
}

// shiftLastA keeps the last A exact (H1 = H0 + 1) but moves its H0 by d: a chain break, not an inexact write.
func shiftLastA(s string, d int64) string {
	l := lines(s)
	ix := aIndexes(l)
	k := ix[len(ix)-1]
	p := strings.Split(l[k], "\t")
	h0 := atoi(p[4]) + d
	p[4], p[5] = formatInt(h0), formatInt(h0+1)
	l[k] = strings.Join(p, "\t")
	return join(l)
}

// exactLastA gives the last A its exact H1 = H0 + 1 (the raced ledger without its raced write).
func exactLastA(s string) string {
	l := lines(s)
	ix := aIndexes(l)
	k := ix[len(ix)-1]
	p := strings.Split(l[k], "\t")
	p[5] = formatInt(atoi(p[4]) + 1)
	l[k] = strings.Join(p, "\t")
	return join(l)
}

func firstLines(s string, n int) string { return join(lines(s)[:n]) }

func insertAfterFirstA(s, line string) string {
	l := lines(s)
	k := aIndexes(l)[0]
	out := append(append(append([]string{}, l[:k+1]...), line), l[k+1:]...)
	return join(out)
}
