package hostloss

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

// class of one write at boundary L, by LSN only (never by time).
type class int

const (
	clsEither   class = iota // failed (E) or in flight (I): zero or its exact content
	clsMust                  // acked, every LSN it may hold <= L: present byte-exact
	clsLost                  // acked, every LSN it may hold > L: absent (zero)
	clsLSNLess               // acked without an exact LSN: zero or its exact content, the rep is NOT COUNTED
	clsStraddle              // acked, H0 < L < H1 (unreachable once H1 = H0 + 1 is required; kept fail-closed)
)

func classify(r *Record, L int64) class {
	switch {
	case r.Kind != 'A':
		return clsEither
	case !r.Exact():
		return clsLSNLess
	case r.H1 <= L:
		return clsMust
	case r.H0 >= L:
		return clsLost
	default:
		return clsStraddle
	}
}

// RacedSeq returns the raced write of a ledger: the LAST acknowledged write by seq when its H0 is readable and its
// H1 is NA. Its LSN is H0 + 1 (one sequential writer: every exact write takes exactly one LSN and the chain holds
// into it), so it is judged by CONTENT like an exact write.
func RacedSeq(l *Ledger) (int64, bool) {
	var last *Record
	for _, r := range l.BySeq {
		if r.Kind == 'A' && (last == nil || r.Seq > last.Seq) {
			last = r
		}
	}
	if last == nil || !last.H0OK || last.H1OK {
		return 0, false
	}
	return last.Seq, true
}

// JudgeImage is F5 (and each restore check) by LSN against an image of blocks*bs bytes at boundary L:
//
//	acked, H1 <= L          present byte-exact (sha256 = ledger)
//	acked, H0 >= L          absent (all zero): the image is exact at L
//	acked, not exact        zero or its exact content; the rep is NOT COUNTED
//	the raced write         judged at its inferred LSN H0 + 1 like an exact write; its result is printed
//	failed or in flight     zero or its exact content
//	never written           zero
//
// FAIL on any violation; NOT_COUNTED (no failure) on an LSN-less ack, a straddle, or no acked write at all.
func JudgeImage(name string, l *Ledger, img io.Reader, L int64, blocks, bs int) Row {
	cls := map[int64]class{}
	for s, r := range l.BySeq {
		cls[s] = classify(r, L)
	}
	rs, raced := RacedSeq(l)
	if raced {
		if l.BySeq[rs].H0+1 <= L {
			cls[rs] = clsMust
		} else {
			cls[rs] = clsLost
		}
	}
	var must, lost, lsnless, straddle, either int
	for _, c := range cls {
		switch c {
		case clsMust:
			must++
		case clsLost:
			lost++
		case clsLSNLess:
			lsnless++
		case clsStraddle:
			straddle++
		default:
			either++
		}
	}
	zero := make([]byte, bs)
	buf := make([]byte, bs)
	var bad []string
	racedText := ""
	for b := 0; b < blocks && len(bad) <= 20; b++ {
		if _, err := io.ReadFull(img, buf); err != nil {
			bad = append(bad, fmt.Sprintf("short read at block %d", b))
			break
		}
		r := l.ByBlock[int64(b)]
		isZero := bytes.Equal(buf, zero)
		if r == nil {
			if !isZero {
				bad = append(bad, fmt.Sprintf("block %d: never written but not zero", b))
			}
			continue
		}
		sum := sha256.Sum256(buf)
		same := hex.EncodeToString(sum[:]) == r.SHA
		c := cls[r.Seq]
		switch {
		case c == clsMust && !same:
			what := "different"
			if isZero {
				what = "absent"
			}
			bad = append(bad, fmt.Sprintf("block %d seq %d: acked with LSN <= %d but %s", b, r.Seq, L, what))
		case c == clsLost && !isZero:
			bad = append(bad, fmt.Sprintf("block %d seq %d: LSN > %d but present (image not exact at L)", b, r.Seq, L))
		case (c == clsLSNLess || c == clsStraddle || c == clsEither) && !isZero && !same:
			bad = append(bad, fmt.Sprintf("block %d seq %d: neither zero nor its content (torn)", b, r.Seq))
		}
		if raced && r.Seq == rs {
			racedText = racedResult(r, b, L, c == clsMust, same, isZero)
		}
	}
	info := fmt.Sprintf("L=%d must=%d lost=%d straddle=%d lsnless=%d either=%d", L, must, lost, straddle, lsnless, either)
	if raced {
		if racedText == "" {
			racedText = fmt.Sprintf("raced seq %d: block not read", rs)
		}
		info += " " + racedText
	}
	switch {
	case len(bad) > 0:
		n := len(bad)
		if n > 5 {
			bad = bad[:5]
		}
		return Row{name, Fail, fmt.Sprintf("%s bad=%d first: %s", info, n, strings.Join(bad, " | "))}
	case straddle > 0 || lsnless > 0 || must+lost == 0:
		return Row{name, NotCounted, info + " (a rep without exact LSNs does not count)"}
	}
	return Row{name, Pass, info}
}

func racedResult(r *Record, b int, L int64, must, same, isZero bool) string {
	cmp, res := ">", "absent (zero)"
	if must {
		cmp, res = "<=", "present byte-exact"
		if !same {
			res = "DIFFERENT"
			if isZero {
				res = "ABSENT"
			}
		}
	} else if !isZero {
		res = "PRESENT"
	}
	return fmt.Sprintf("raced seq %d block %d (LSN inferred H0+1=%d %s L=%d): %s", r.Seq, b, r.H0+1, cmp, L, res)
}
