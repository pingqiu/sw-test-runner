package v3block

// C3 by construction: every restore pair's two directions are forced steps.
//
// C3 (checkers/history_check.py c3) judges a restore pair (source S, target
// T) in two directions. X -> Y is exercised by the LATEST write on X at a
// block, after the restore, being acknowledged (ok), followed by an ok read
// of the same block on Y with no write by Y to that block after X's write.
// A pair with a direction not exercised fails C3.
//
// The model keeps the same facts from what it learns (c3Learn): a clock over
// the learnt operations, each volume's latest write per lba (ok or not), and
// per pair the restore's clock and which directions an ok read exercised.
// When a restore completes ok, the target's first attach is forced, then its
// inherited reads (C2); as soon as both sides serve, the S -> T window is
// due: a write on S of a block, then a read of it on T. The T -> S window
// (a write on T read back on S, then the union of both contents read on
// both) waits pairDelay drawn operations: until then the target is
// unwritten, which the release and restore-from-target negatives probe.
// Each window's steps run in order (a window waits while its next step's
// volume is killed, fenced or unsettled; nothing of it runs out of order).
// When a window has run and its direction is still not exercised (its write
// answered unknown, its read did not answer ok, or a write in between masked
// it), the direction is forced again with a new round, at most pairTries
// times. The pair is held (neither side stopped, moved or deleted by an
// ordinary operation, nor given the eleventh-snapshot negative) until both
// directions are exercised or given up.
//
// What still ends a pair unjudged, and is noted in coverage ("pair not
// exercised: <source>|<target>: <why>"): a side taken out of the run by an
// outcome the model cannot place (an unknown snapshot or other unknown
// non-write, a refused_effect, a negative answered ok), a side that a
// settled record places stopped after an unknown attach or stop, the run
// ending while a side is still down, and a direction still not exercised
// after pairTries windows.

import (
	"slices"
	"strconv"
	"strings"
)

// pairTries is how many windows one direction gets.
const pairTries = 3

// oldPairs, set only by tests, emulates the pair policy before C3 by
// construction (m1-gen/9): a side killed or unsettled loses the pair, the
// target's first attach is dropped while the source's holder is killed,
// both directions wait pairDelay, each gets one window, and the pair stops
// being held as soon as its windows are queued (an in-operation fault's
// target and the store pause's stop may then land on a side mid-window).
// The red controls run the same cases under it.
var oldPairs = false

// wrote is a volume's latest learnt write of one lba.
type wrote struct {
	clock int
	ok    bool
}

// c3Pair is one restore pair as C3 judges it. Direction 0 is source ->
// target (a write on the source read on the target), 1 the reverse.
type c3Pair struct {
	src, tgt string
	at       int     // the clock of the restore
	ex       [2]bool // exercised
	tries    [2]int  // windows queued
	open     [2]bool // a window of the direction still has steps
	gaveUp   [2]bool // pairTries windows ran without exercising it
}

// pairWin is the forced steps of one direction of a pair, run in order.
type pairWin struct {
	key string
	dir int
	ops []GenOp
}

// c3Learn applies a learnt operation to the C3 facts: a restore answered ok
// starts a pair; a write answered ok or unknown is its volume's latest at
// each of its lbas (unknown: not acknowledged); a read answered ok exercises
// the direction from the other side of each pair at each of its lbas, as C3
// does. A negative changes nothing here (it writes nothing).
func (m *blockModel) c3Learn(o GenOp, outcome string) {
	m.clock++
	if o.Negative != "" {
		return
	}
	lbas := func() []int {
		var out []int
		for _, f := range strings.Fields(o.Params["lbas"]) {
			if b, err := strconv.Atoi(f); err == nil {
				out = append(out, b)
			}
		}
		return out
	}
	switch o.Action {
	case "v3_op_restore":
		if outcome == "ok" {
			t := o.Params["target_volume"]
			m.c3[o.Volume+"|"+t] = &c3Pair{src: o.Volume, tgt: t, at: m.clock}
		}
	case "v3_op_write":
		if outcome != "ok" && outcome != "unknown" {
			return
		}
		if m.lastW[o.Volume] == nil {
			m.lastW[o.Volume] = map[int]wrote{}
		}
		for _, b := range lbas() {
			m.lastW[o.Volume][b] = wrote{m.clock, outcome == "ok"}
		}
	case "v3_op_read":
		if outcome != "ok" {
			return
		}
		for _, c := range m.c3 {
			dir, other := 0, c.src // a read on the target: source -> target
			switch o.Volume {
			case c.tgt:
			case c.src:
				dir, other = 1, c.tgt
			default:
				continue
			}
			for _, b := range lbas() {
				w, own := m.lastW[other][b], m.lastW[o.Volume][b]
				ownAt := 0
				if own.clock > c.at {
					ownAt = own.clock
				}
				if w.ok && w.clock > c.at && w.clock > ownAt {
					c.ex[dir] = true
				}
			}
		}
	}
}

// pairBlocks are the lbas a pair's windows use: a, written on the source
// and read on the target, and b, written on the target and read on the
// source. Each is a block both sides hold when there is one (an inherited
// lba first, so the reading side has its own value there); otherwise the
// source's first content lba (or 0) for both.
func (m *blockModel) pairBlocks(src, tgt *genVol) (int, int) {
	in := tgt.inheritedLBAs
	if len(in) == 0 {
		for _, b := range sortedLBAs(src.lbas) {
			if tgt.lbas[b] {
				in = append(in, b)
			}
		}
	}
	if len(in) == 0 {
		x := 0
		if s := sortedLBAs(src.lbas); len(s) > 0 {
			x = s[0]
		}
		return x, x
	}
	return in[0], in[len(in)-1]
}

// window is one direction's forced steps: a write of the direction's block
// on the writing side (a new round each try), then a read of it on the
// other side; the first T -> S window then reads the union of both
// contents on both sides (the pair's readback before either side may
// leave).
func (m *blockModel) window(c *c3Pair, dir int) *pairWin {
	src, tgt := m.vols[c.src], m.vols[c.tgt]
	key := pairKey(src, tgt)
	a, b := m.pairBlocks(src, tgt)
	w, r, lba := src, tgt, a
	if dir == 1 {
		w, r, lba = tgt, src, b
	}
	round := key + "-" + strconv.Itoa(dir+1)
	if c.tries[dir] > 0 {
		round += string(rune('a' + c.tries[dir]))
	}
	ops := []GenOp{m.pairWrite(w, lba, round)}
	ops = append(ops, m.readsOf(r, []int{lba}, "pair")...)
	if dir == 1 && c.tries[1] == 0 {
		u := map[int]bool{a: true, b: true}
		for x := range src.lbas {
			u[x] = true
		}
		for x := range tgt.lbas {
			u[x] = true
		}
		lbas := sortedLBAs(u)
		ops = append(ops, m.readsOf(src, lbas, "pair")...)
		ops = append(ops, m.readsOf(tgt, lbas, "pair")...)
	}
	return &pairWin{key: key, dir: dir, ops: ops}
}

// pairLost says why a pair can no longer be judged ("" while it can): a
// side out of the run (an unknown outcome the model cannot place), or not in
// a state its windows need (the source serving, the target created or
// serving). A side killed, fenced or unsettled is waited for.
func pairLost(src, tgt *genVol) string {
	for _, v := range []*genVol{src, tgt} {
		switch {
		case v.state == stUnknown:
			return v.name + ": out of the run (an unknown outcome)"
		case oldPairs && v.killed:
			return v.name + ": holder killed"
		case oldPairs && v.state == stUnsettled:
			return v.name + ": unsettled"
		}
	}
	if src.state != stServing && src.state != stUnsettled {
		return src.name + ": " + src.state
	}
	if tgt.state != stCreated && tgt.state != stServing && tgt.state != stUnsettled {
		return tgt.name + ": " + tgt.state
	}
	return ""
}

// pending says v is a side of a pair whose directions are not settled.
func (m *blockModel) pending(v *genVol) bool {
	for _, t := range v.targets {
		if !v.pairDone[pairKey(v, m.vols[t])] {
			return true
		}
	}
	if v.source != "" {
		if src := m.vols[v.source]; src != nil && src.targets[v.fromSnap] == v.name && !src.pairDone[pairKey(src, v)] {
			return true
		}
	}
	return false
}

// pairCheck settles the pending pairs (settlePairs).
func (m *blockModel) pairCheck() { m.settlePairs(false) }

// serves says v serves now (reads and writes can run on it).
func serves(v *genVol) bool { return v.state == stServing && !v.fenced && !v.killed }

// settlePairs drops a lost pair (noted), queues the windows a pair's
// directions are owed, and settles a pair whose directions are exercised or
// given up. now queues the T -> S window without waiting pairDelay (the
// run's end).
func (m *blockModel) settlePairs(now bool) {
	for _, p := range m.pairs() {
		src, tgt := p[0], p[1]
		key := pairKey(src, tgt)
		if src.pairDone[key] {
			continue
		}
		c := m.c3[key]
		if c == nil { // a pair the model did not see restored (a test's shadow)
			c = &c3Pair{src: src.name, tgt: tgt.name, at: m.clock}
			m.c3[key] = c
		}
		if why := pairLost(src, tgt); why != "" {
			m.pairDrop(key, why)
			continue
		}
		if serves(src) && serves(tgt) {
			ready, seen := m.pairReady[key]
			if !seen {
				m.pairReady[key], ready = m.drawn, m.drawn
			}
			for dir := 0; dir < 2; dir++ {
				if c.ex[dir] || c.open[dir] || c.gaveUp[dir] {
					continue
				}
				if (dir == 1 || oldPairs) && !now && m.drawn-ready < pairDelay {
					continue
				}
				if c.tries[dir] >= pairTries || oldPairs && c.tries[dir] >= 1 {
					c.gaveUp[dir] = true
					m.notes = append(m.notes, "pair not exercised: "+key+": "+[]string{"source -> target", "target -> source"}[dir]+" after "+strconv.Itoa(pairTries)+" windows")
					continue
				}
				m.wins = append(m.wins, m.window(c, dir))
				c.tries[dir]++
				c.open[dir] = true
			}
			if oldPairs && c.tries[0] > 0 && c.tries[1] > 0 {
				src.pairDone[key] = true
				continue
			}
		}
		if (c.ex[0] || c.gaveUp[0]) && (c.ex[1] || c.gaveUp[1]) && !c.open[0] && !c.open[1] {
			src.pairDone[key] = true
		}
	}
}

// pairDrop ends a pair that can no longer be judged, with its note.
func (m *blockModel) pairDrop(key, why string) {
	src := m.vols[strings.SplitN(key, "|", 2)[0]]
	src.pairDone[key] = true
	m.notes = append(m.notes, "pair not exercised: "+key+": "+why)
	m.wins = slices.DeleteFunc(m.wins, func(w *pairWin) bool { return w.key == key })
	if c := m.c3[key]; c != nil {
		c.open = [2]bool{}
	}
}

// winNext is the next step of the first window whose next step can run:
// a window whose next step waits (its volume killed, fenced, unsettled) is
// skipped whole, and one whose volume left for good is dropped (its pair is
// then lost: settlePairs notes it).
func (m *blockModel) winNext() (GenOp, bool) {
	for i := 0; i < len(m.wins); i++ {
		w := m.wins[i]
		switch m.dueState(w.ops[0]) {
		case "go":
			return w.ops[0], true
		case "drop":
			if c := m.c3[w.key]; c != nil {
				c.open[w.dir] = false
			}
			m.wins = slices.Delete(m.wins, i, i+1)
			i--
		}
	}
	return GenOp{}, false
}

// winLearnt pops a learnt window step; a window with no step left is done
// (settlePairs then checks its direction).
func (m *blockModel) winLearnt(o GenOp) {
	for i, w := range m.wins {
		if len(w.ops) > 0 && sameOp(w.ops[0], o) {
			w.ops = w.ops[1:]
			if len(w.ops) == 0 {
				if c := m.c3[w.key]; c != nil {
					c.open[w.dir] = false
				}
				m.wins = slices.Delete(m.wins, i, i+1)
			}
			return
		}
	}
}
