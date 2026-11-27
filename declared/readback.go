package declared

// The "device_readback" probe: after a connect, the unit's EFFECTIVE
// values read back by the step's device resolver (its readback, resolver.go;
// the probe's Files: option -> the resolver's file name), recorded in the
// result field Into (the unit under the resolver's UnitKey), and compared
// with the options the connect was given (Expect, a template of
// ",key=value..." as the connect's options). A declared option the unit
// does not hold, or cannot show, ends the step unknown with the resolver's
// Mismatch text (one MismatchItem per option); with FailRun
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
	subsystem, err := s.expand(pr.Subsystem)
	if err != nil {
		return tr.OutcomeUnknown, err.Error(), false
	}
	r, err := x.resolver(pr.Resolver)
	if err != nil || r.Readback == nil {
		return tr.OutcomeUnknown, fmt.Sprintf("device resolver %q declares no readback", pr.Resolver), false
	}
	rb := r.Readback
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
	unit, byFile, why := x.DeviceReadback(ctx, s.node, pr.Resolver, subsystem, files)
	eff := map[string]string{}
	for _, o := range opts {
		if v, ok := byFile[pr.Files[o]]; ok {
			eff[o] = v
		}
	}
	read := map[string]any{rb.UnitKey: unit, "values": eff}
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
		if t, ok := rb.TextValues[got]; ok {
			n, err = t, nil
		}
		if !shown || err != nil || n != declared[o] {
			is := resolverScope(ctx, s.node, map[string]string{"option": o, "declared": strconv.Itoa(declared[o]), "shown": got})
			item, _ := is.expand(rb.MismatchItem)
			bad = append(bad, item)
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
	ms := resolverScope(ctx, s.node, map[string]string{"mismatches": strings.Join(bad, "; "), "subsystem": subsystem})
	msg, _ := ms.expand(rb.Mismatch)
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
