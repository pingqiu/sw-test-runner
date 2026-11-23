package hostloss

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

// Synthetic F5 cases on 32 blocks of 4 KiB: seqs 0..9 on blocks 0..9 with exact LSNs (H0 = 100 + seq,
// H1 = 101 + seq); the boundary L = 105 keeps seqs 0..4 and loses 5..9 (they must then be absent).
const (
	tBS     = 4096
	tBlocks = 32
)

func content(seq int) []byte { return bytes.Repeat([]byte{byte(seq + 1), 0xA5}, tBS/2) }

func shaOf(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

type w struct {
	seq    int
	h0, h1 string // "" = exact defaults
	kind   byte   // 'A' (default), 'E' or 'I'
	blk    int    // -1 = seq
}

func ledgerOf(ws []w) []byte {
	var b strings.Builder
	for _, x := range ws {
		blk := x.blk
		if blk < 0 {
			blk = x.seq
		}
		h0, h1 := x.h0, x.h1
		if h0 == "" {
			h0 = fmt.Sprint(100 + x.seq)
		}
		if h1 == "" {
			h1 = fmt.Sprint(101 + x.seq)
		}
		sha := shaOf(content(x.seq))
		fmt.Fprintf(&b, "I\t%d\t%d\t%s\t%s\t%d\t%d\n", x.seq, blk, sha, h0, 1000+x.seq, 1_800_000_000_000_000_000+x.seq)
		switch x.kind {
		case 'E':
			fmt.Fprintf(&b, "E\t%d\t%d\t5\t%d\t%d\n", x.seq, blk, 2000+x.seq, 1_800_000_000_000_000_001+x.seq)
		case 'I':
		default:
			fmt.Fprintf(&b, "A\t%d\t%d\t%s\t%s\t%s\t%d\t%d\n", x.seq, blk, sha, h0, h1, 2000+x.seq, 1_800_000_000_000_000_001+x.seq)
		}
	}
	return []byte(b.String())
}

func base(edit map[int]w) []w {
	var out []w
	for s := 0; s < 10; s++ {
		x := w{seq: s, blk: -1}
		if e, ok := edit[s]; ok {
			x.h0, x.h1, x.kind = e.h0, e.h1, e.kind
		}
		out = append(out, x)
	}
	return out
}

func imageOf(present []int, extra map[int][]byte) []byte {
	img := make([]byte, tBS*tBlocks)
	for _, s := range present {
		copy(img[s*tBS:], content(s))
	}
	for blk, d := range extra {
		copy(img[blk*tBS:], d)
	}
	return img
}

func seqs(from, to int, except ...int) []int {
	var out []int
outer:
	for s := from; s <= to; s++ {
		for _, e := range except {
			if s == e {
				continue outer
			}
		}
		out = append(out, s)
	}
	return out
}

func judge(t *testing.T, led []byte, img []byte, L int64) Row {
	t.Helper()
	l, err := ParseLedger(led)
	if err != nil {
		t.Fatal(err)
	}
	return JudgeImage("d7-f5", l, bytes.NewReader(img), L, tBlocks, tBS)
}

func TestImageByLSN(t *testing.T) {
	keep := seqs(0, 4)
	all := seqs(0, 9)
	for _, c := range []struct {
		name string
		led  []w
		img  []byte
		L    int64
		want Outcome
		text string
	}{
		{"exact-at-L", base(nil), imageOf(keep, nil), 105, Pass, "must=5 lost=5"},
		{"kept-write-missing", base(nil), imageOf(seqs(0, 4, 2), nil), 105, Fail, "seq 2: acked with LSN <= 105 but absent"},
		{"lost-write-present", base(nil), imageOf(append(keep, 7), nil), 105, Fail, "seq 7: LSN > 105 but present"},
		{"stray-block", base(nil), imageOf(keep, map[int][]byte{20: []byte("XXXXXXXXXXXXXXXX")}), 105, Fail, "block 20: never written but not zero"},
		{"torn-unacked", append(base(nil), w{seq: 10, kind: 'E', blk: -1}), imageOf(keep, map[int][]byte{10: []byte("garbage!")}), 105, Fail, "neither zero nor its content (torn)"},
		{"unacked-either-state", append(append(base(nil), w{seq: 10, kind: 'E', blk: -1}), w{seq: 11, kind: 'I', blk: -1}), imageOf(append(keep, 10), nil), 105, Pass, ""},
		// a (104, 106] write straddles L but is not exact, so it is LSN-less first (straddle is unreachable)
		{"straddle-is-lsnless-not-counted", base(map[int]w{5: {h0: "104", h1: "106"}}), imageOf(keep, nil), 105, NotCounted, "straddle=0 lsnless=1"},
		{"lsnless-not-counted", base(map[int]w{3: {h1: "NA"}}), imageOf(keep, nil), 105, NotCounted, "lsnless=1"},
		{"inexact-not-counted", base(map[int]w{2: {h0: "100", h1: "103"}}), imageOf(keep, nil), 105, NotCounted, "lsnless=1"},
		{"restore-at-own-cut", base(nil), imageOf(all, nil), 110, Pass, "must=10 lost=0"},
		// the raced write: the LAST ack, H0 readable, H1 NA, judged at its inferred LSN H0 + 1 = 110 by content
		{"raced-lost-absent", base(map[int]w{9: {h1: "NA"}}), imageOf(keep, nil), 105, Pass, "raced seq 9 block 9 (LSN inferred H0+1=110 > L=105): absent (zero)"},
		{"raced-lost-present", base(map[int]w{9: {h1: "NA"}}), imageOf(append(keep, 9), nil), 105, Fail, "raced seq 9 block 9 (LSN inferred H0+1=110 > L=105): PRESENT"},
		{"raced-kept-present", base(map[int]w{9: {h1: "NA"}}), imageOf(all, nil), 110, Pass, "raced seq 9 block 9 (LSN inferred H0+1=110 <= L=110): present byte-exact"},
		{"raced-kept-missing", base(map[int]w{9: {h1: "NA"}}), imageOf(seqs(0, 8), nil), 110, Fail, "(LSN inferred H0+1=110 <= L=110): ABSENT"},
		{"raced-kept-wrong", base(map[int]w{9: {h1: "NA"}}), imageOf(all, map[int][]byte{9: bytes.Repeat([]byte("W"), 64)}), 110, Fail, "(LSN inferred H0+1=110 <= L=110): DIFFERENT"},
		{"raced-plus-other-na-not-counted", base(map[int]w{3: {h1: "NA"}, 9: {h1: "NA"}}), imageOf(all, nil), 110, NotCounted, "lsnless=1"},
	} {
		r := judge(t, ledgerOf(c.led), c.img, c.L)
		if r.Outcome != c.want || !strings.Contains(r.Text, c.text) {
			t.Errorf("%s: %s (want %s containing %q)", c.name, r, c.want, c.text)
		}
	}
}

func TestImageShortRead(t *testing.T) {
	r := judge(t, ledgerOf(base(nil)), imageOf(seqs(0, 4), nil)[:tBS*3], 105)
	if r.Outcome != Fail || !strings.Contains(r.Text, "short read at block 3") {
		t.Fatalf("%s", r)
	}
}

func TestRacedSeqOnlyTheLastAck(t *testing.T) {
	for name, c := range map[string]struct {
		edit map[int]w
		want bool
	}{
		"last-ack-h1-na":    {map[int]w{9: {h1: "NA"}}, true},
		"earlier-ack-h1-na": {map[int]w{3: {h1: "NA"}}, false},
		"last-ack-h0-na":    {map[int]w{9: {h0: "NA", h1: "NA"}}, false},
		"last-ack-exact":    {nil, false},
	} {
		l, err := ParseLedger(ledgerOf(base(c.edit)))
		if err != nil {
			t.Fatal(err)
		}
		if _, got := RacedSeq(l); got != c.want {
			t.Errorf("%s: raced=%t, want %t", name, got, c.want)
		}
	}
}
