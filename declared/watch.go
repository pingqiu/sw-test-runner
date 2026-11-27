package declared

// The abandoned-child watcher. A child the runner abandoned at a deadline
// (it survived the kill) may still act: an abandoned device write is queued
// in the guest kernel and can land later. The checkers can close its
// window only on recorded facts, so every abandoned child is watched, and
// its exit is recorded as one observe record "child_exit":
//
//	{op_seq, pid, start, exit, outcome, killed?, alive?}
//
// op_seq is the abandoned operation's invoke seq (null for an observation).
// exit is the child's exit status when the detached runner's own result
// file holds it (the child is the command's process the runner waited on),
// else null. outcome decodes it as the operation would have: exit 0 ok,
// another status failed, an exit the runner caused (SIGKILL, 137) or none
// known unknown. Never from a log.
//
// Check runs at each operation boundary, at each healthy point and at the
// run's end (final: a child still alive is recorded unknown, alive true).
// Abandon is the ONLY way a result names abandoned children: it registers
// them, so an abandoned list without a watcher cannot be written (a test
// keeps "abandoned" out of every other file).

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	tr "github.com/pingqiu/sw-test-runner"
)

// AbandonedKey is the result field that names abandoned children.
const AbandonedKey = "abandoned"

// OpRef names the operation a command runs for: its history, invoke seq (0
// for an observation) and subject.
type OpRef struct {
	History *tr.History
	Seq     uint64
	Volume  string
}

type opKey struct{}

// WithOp returns ctx carrying the operation ref.
func WithOp(ctx context.Context, ref OpRef) context.Context {
	return context.WithValue(ctx, opKey{}, ref)
}

// OpOf returns the operation ref ctx carries.
func OpOf(ctx context.Context) (OpRef, bool) {
	ref, ok := ctx.Value(opKey{}).(OpRef)
	return ref, ok && ref.History != nil
}

type watchedChild struct {
	ref  OpRef
	proc Proc
	dir  string
}

type watchList struct {
	mu    sync.Mutex
	items []watchedChild
}

var watchers sync.Map // *tr.History -> *watchList

func listOf(h *tr.History) *watchList {
	l, _ := watchers.LoadOrStore(h, &watchList{})
	return l.(*watchList)
}

// Abandon is the result of an operation its runner deadline ended: the
// detail, and the processes that survived the kill, which it registers with
// the run's watcher (the operation ctx carries). Without an operation ref
// nothing could record their exit, so the result says so ("unwatched").
func Abandon(ctx context.Context, t *Detached) map[string]any {
	res := map[string]any{"detail": t.Error()}
	if len(t.Abandoned) == 0 {
		return res
	}
	res[AbandonedKey] = t.Abandoned
	ref, ok := OpOf(ctx)
	if !ok {
		res["unwatched"] = true
		return res
	}
	l := listOf(ref.History)
	l.mu.Lock()
	for _, p := range t.Abandoned {
		l.items = append(l.items, watchedChild{ref: ref, proc: p, dir: t.Dir})
	}
	l.mu.Unlock()
	return res
}

// Watched reports how many abandoned children of h are watched.
func Watched(h *tr.History) int {
	l := listOf(h)
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.items)
}

var digitsOnly = regexp.MustCompile(`^[0-9]+$`)

// Check looks once at every abandoned child of h and records each that is
// gone; final also records each still alive (unknown, alive true) and
// stops watching. A run with no abandoned child issues no command. node
// may be nil at the end (no node to look with): every child is then
// recorded unknown with alive null.
func Check(ctx context.Context, node Runner, h *tr.History, final bool) error {
	if h == nil {
		return nil
	}
	l := listOf(h)
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.items) == 0 {
		return nil
	}
	answers := map[string][]string{}
	looked := false
	if node != nil {
		var pids []string
		var b strings.Builder
		for _, c := range l.items {
			pids = append(pids, c.proc.PID)
		}
		fmt.Fprintf(&b, ": runner-io watch %s;", strings.Join(pids, " "))
		for _, c := range l.items {
			if !digitsOnly.MatchString(c.proc.PID) || (c.proc.Start != "" && !digitsOnly.MatchString(c.proc.Start)) {
				continue
			}
			fmt.Fprintf(&b, ` s=$(cat /proc/%[1]s/stat 2>/dev/null); t=${s##*) }; set -- $t; if [ -n "$s" ] && { [ -z "%[2]s" ] || [ "${20}" = "%[2]s" ]; }; then echo "alive %[1]s"; else echo "gone %[1]s $(cat %[3]s 2>/dev/null || echo -) $(cat %[4]s 2>/dev/null || echo -)"; fi;`,
				c.proc.PID, c.proc.Start, ShellQuote(c.dir+"/rc"), ShellQuote(c.dir+"/cpid"))
		}
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ProtocolCallBound)
		out, _, code, err := Call(cctx, node.Run, b.String())
		cancel()
		if err == nil && code == 0 {
			looked = true
			for _, line := range strings.Split(out, "\n") {
				if f := strings.Fields(line); len(f) >= 2 {
					answers[f[1]] = f
				}
			}
		}
	}
	var keep []watchedChild
	for i, c := range l.items {
		f, seen := answers[c.proc.PID]
		var res map[string]any
		switch {
		case seen && f[0] == "gone":
			res = exitResult(c, f)
		case final && (!looked || !seen):
			res = childResult(c, nil, tr.OutcomeUnknown)
			res["alive"] = nil
			res["detail"] = "the child could not be looked at by the run's end"
		case final:
			res = childResult(c, nil, tr.OutcomeUnknown)
			res["alive"] = true
		default:
			keep = append(keep, c)
			continue
		}
		if err := h.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "runner", Verb: "child_exit", Volume: c.ref.Volume, Result: res}); err != nil {
			l.items = append(keep, l.items[i:]...)
			return err
		}
	}
	l.items = keep
	if final {
		watchers.Delete(h)
	}
	return nil
}

func childResult(c watchedChild, exit any, outcome string) map[string]any {
	var seq any
	if c.ref.Seq != 0 {
		seq = c.ref.Seq
	}
	return map[string]any{"op_seq": seq, "pid": c.proc.PID, "start": c.proc.Start, "exit": exit, "outcome": outcome}
}

// exitResult decodes a gone child from "gone <pid> <rc|-> <cpid|->": the
// exit status is known only for the command's own process (cpid), from the
// rc file the detached runner wrote when it was reaped.
func exitResult(c watchedChild, f []string) map[string]any {
	if len(f) < 4 || f[3] != c.proc.PID || f[2] == "-" {
		return childResult(c, nil, tr.OutcomeUnknown)
	}
	rc, err := strconv.Atoi(f[2])
	if err != nil {
		return childResult(c, nil, tr.OutcomeUnknown)
	}
	switch {
	case rc == 0:
		return childResult(c, rc, tr.OutcomeOK)
	case rc == 137:
		res := childResult(c, rc, tr.OutcomeUnknown)
		res["killed"] = true // the runner's SIGKILL: what it did before is not known
		return res
	}
	return childResult(c, rc, "failed")
}
