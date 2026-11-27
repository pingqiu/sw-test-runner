package v3block

// The guest connect's effective values. After each connect the runner reads
// the controller's values back from the kernel (/sys/class/nvme/<ctrl>/<file>,
// the description's guest.connect.readback) and records them in the attach;
// at the run's first connect it also writes one guest_connect record (the
// connect declared, and what the kernel shows). A declared option the kernel
// does not hold, or cannot show, fails the run before any other operation:
// what follows could not count as evidence of the declared configuration.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	tr "github.com/pingqiu/sw-test-runner"
)

// checkConnect reads the controller's effective connect values back and
// records them in res; it returns the mismatch, "" when there is none.
func (e opEnv) checkConnect(ctx context.Context, node runner, v *opVolume, res map[string]any) string {
	ctrl, eff, why := readConnect(ctx, node, v.NQN)
	read := map[string]any{"ctrl": ctrl, "values": eff}
	if why != "" {
		read["error"] = why
	}
	res["guest_connect_effective"] = read
	if e.lastReadback != nil {
		*e.lastReadback = read
	}
	var bad []string
	for _, o := range guest.Connect.Options {
		want, declared := e.connect.Declared[o]
		if !declared {
			continue
		}
		got, shown := eff[o]
		n, err := strconv.Atoi(got)
		if got == "off" {
			n, err = -1, nil
		}
		if !shown || err != nil || n != want {
			bad = append(bad, fmt.Sprintf("%s declared %d, the kernel shows %q", o, want, got))
		}
	}
	if len(bad) == 0 {
		return ""
	}
	msg := "guest connect " + e.connect.Name + ": " + strings.Join(bad, "; ")
	if why != "" {
		msg += " (" + why + ")"
	}
	if e.connectMismatch != nil {
		*e.connectMismatch = msg
	}
	return msg
}

// readConnect reads the one controller of nqn and the readback file of
// every option ("-" when a file is absent, then left out).
func readConnect(ctx context.Context, node runner, nqn string) (string, map[string]string, string) {
	var files []string
	for _, o := range guest.Connect.Options {
		files = append(files, guest.Connect.Readback[o])
	}
	cmd := fmt.Sprintf(`for d in /sys/class/nvme/nvme*; do [ -e "$d" ] || continue; q=$(cat "$d/subsysnqn") || exit 3; [ "$q" = %s ] || continue; echo "ctrl $(basename "$d")"; for f in %s; do if [ -e "$d/$f" ]; then echo "$f $(cat "$d/$f")"; else echo "$f -"; fi; done; done; echo end`,
		shellQuote(nqn), strings.Join(files, " "))
	stdout, stderr, code, err := node.Run(ctx, cmd)
	eff := map[string]string{}
	if err != nil || code != 0 {
		return "", eff, fmt.Sprintf("readback exit %d: %s", code, firstLine(stderr))
	}
	byFile := map[string]string{}
	ctrl, ctrls := "", 0
	for _, l := range strings.Split(strings.TrimSpace(stdout), "\n") {
		f := strings.Fields(l)
		switch {
		case len(f) == 2 && f[0] == "ctrl":
			ctrl, ctrls = f[1], ctrls+1
		case len(f) == 2:
			byFile[f[0]] = f[1]
		}
	}
	for _, o := range guest.Connect.Options {
		if val, ok := byFile[guest.Connect.Readback[o]]; ok && val != "-" {
			eff[o] = val
		}
	}
	if ctrls != 1 {
		return ctrl, eff, fmt.Sprintf("%d controllers of %s", ctrls, nqn)
	}
	return ctrl, eff, ""
}

var firstConnects sync.Map // *tr.History -> true once written

// firstConnect writes, once per run, the guest_connect record: the connect
// declared and the values the kernel showed at the run's first connect.
func firstConnect(actx *tr.ActionContext, e opEnv) {
	h := actx.History
	if h == nil || e.lastReadback == nil || *e.lastReadback == nil {
		return
	}
	if _, done := firstConnects.LoadOrStore(h, true); done {
		return
	}
	res := map[string]any{"name": e.connect.Name, "evidence": e.connect.Evidence, "label": e.connect.Label,
		"declared": e.connect.Declared, "effective": *e.lastReadback,
		"match": e.connectMismatch == nil || *e.connectMismatch == ""}
	_ = h.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "runner", Verb: "guest_connect", Result: res})
}
