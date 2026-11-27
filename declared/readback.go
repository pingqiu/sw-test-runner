package declared

// The "nvme_readback" probe: after a connect, the controller's EFFECTIVE
// values read back from the kernel (/sys/class/nvme/<ctrl>/<file>, the
// probe's Files: option -> sysfs file), recorded in the result field Into,
// and compared with the options the connect was given (Expect, a template
// of ",key=value..." as the connect's options). A declared option the
// kernel does not hold, or cannot show, ends the step unknown; with FailRun
// the action also fails the run (a runner error after its record), so no
// operation after it counts. The run's first read-back also writes one
// observe record RecordVerb: RecordFields (templates), the declared and the
// effective values, and whether they match.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	tr "github.com/pingqiu/sw-test-runner"
)

type readbackKey struct {
	h    *tr.History
	verb string
}

var readbackRecorded sync.Map // readbackKey -> true

// parseOptions reads ",key=value..." into its values.
func parseOptions(s string) (map[string]int, error) {
	out := map[string]int{}
	for _, kv := range strings.Split(strings.TrimPrefix(s, ","), ",") {
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		n, err := strconv.Atoi(v)
		if !ok || err != nil {
			return nil, fmt.Errorf("option %q", kv)
		}
		out[k] = n
	}
	return out, nil
}

func (x *Executor) readback(s *scope, ctx context.Context, pr *Probe, res map[string]any) (string, string, bool) {
	nqn, err := s.expand(pr.Subsystem)
	if err != nil {
		return tr.OutcomeUnknown, err.Error(), false
	}
	expectS, err := s.expand(pr.Expect)
	if err != nil {
		return tr.OutcomeUnknown, err.Error(), false
	}
	declared, err := parseOptions(expectS)
	if err != nil {
		return tr.OutcomeUnknown, "readback: " + err.Error(), false
	}
	opts := make([]string, 0, len(pr.Files))
	for o := range pr.Files {
		opts = append(opts, o)
	}
	sort.Strings(opts)
	var files []string
	for _, o := range opts {
		files = append(files, pr.Files[o])
	}
	cmd := fmt.Sprintf(`for d in /sys/class/nvme/nvme*; do [ -e "$d" ] || continue; q=$(cat "$d/subsysnqn") || exit 3; [ "$q" = %s ] || continue; echo "ctrl $(basename "$d")"; for f in %s; do if [ -e "$d/$f" ]; then echo "$f $(cat "$d/$f")"; else echo "$f -"; fi; done; done; echo end`,
		ShellQuote(nqn), strings.Join(files, " "))
	stdout, stderr, code, rerr := s.node.Run(ctx, cmd)
	eff := map[string]string{}
	why := ""
	ctrl := ""
	if rerr != nil || code != 0 {
		why = fmt.Sprintf("readback exit %d: %s", code, FirstLine(stderr))
	} else {
		byFile := map[string]string{}
		ctrls := 0
		for _, l := range strings.Split(strings.TrimSpace(stdout), "\n") {
			f := strings.Fields(l)
			switch {
			case len(f) == 2 && f[0] == "ctrl":
				ctrl, ctrls = f[1], ctrls+1
			case len(f) == 2:
				byFile[f[0]] = f[1]
			}
		}
		for _, o := range opts {
			if v, ok := byFile[pr.Files[o]]; ok && v != "-" {
				eff[o] = v
			}
		}
		if ctrls != 1 {
			why = fmt.Sprintf("%d controllers of %s", ctrls, nqn)
		}
	}
	read := map[string]any{"ctrl": ctrl, "values": eff}
	if why != "" {
		read["error"] = why
	}
	if res != nil && pr.Into != "" {
		res[pr.Into] = read
	}
	var bad []string
	for _, o := range sortedKeysInt(declared) {
		got, shown := eff[o]
		n, err := strconv.Atoi(got)
		if got == "off" {
			n, err = -1, nil
		}
		if !shown || err != nil || n != declared[o] {
			bad = append(bad, fmt.Sprintf("%s declared %d, the kernel shows %q", o, declared[o], got))
		}
	}
	match := len(bad) == 0
	if pr.RecordVerb != "" {
		if ref, ok := OpOf(ctx); ok {
			if _, done := readbackRecorded.LoadOrStore(readbackKey{ref.History, pr.RecordVerb}, true); !done {
				rec := map[string]any{"declared": declared, "effective": read, "match": match}
				for _, k := range sortedKeys(pr.RecordFields) {
					v, _ := s.expand(pr.RecordFields[k])
					rec[k] = v
				}
				_ = ref.History.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "runner", Verb: pr.RecordVerb, Volume: ref.Volume, Result: rec})
			}
		}
	}
	if match {
		return "", "", true
	}
	msg := "guest connect: " + strings.Join(bad, "; ")
	if why != "" {
		msg += " (" + why + ")"
	}
	if pr.FailRun {
		s.fatal = msg
	}
	return tr.OutcomeUnknown, msg, false
}

func sortedKeysInt(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
