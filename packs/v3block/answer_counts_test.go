package v3block

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/declared"
)

// The release bar's ratio: the summary line, the answer_counts field and
// the bundle's answer-counts.json, for k/n, 0/n and 0/0 (a run without a
// snapshot says so).
func TestSnapshotHeadConflictRatio(t *testing.T) {
	conflict := chunkErr("head_conflict", "storage: capture CAS not applied: storage: chunk HEAD precondition failed")
	for name, c := range map[string]struct {
		answers []eqAnswer
		line    string
		k, n    int
	}{
		// The conflict is rerun (the page's retry): its rerun is an attempt
		// too, so three snapshots make four attempts and the conflict stays
		// counted.
		"one conflict in three snapshots, rerun": {[]eqAnswer{{}, conflict, {}, {}}, "head_conflict per snapshot attempted: 1/4", 1, 4},
		"none in two":                            {[]eqAnswer{{}, {}}, "head_conflict per snapshot attempted: 0/2", 0, 2},
		"no snapshot":                            {nil, "head_conflict per snapshot attempted: 0/0 (no snapshot attempted)", 0, 0},
	} {
		node := &eqNode{rules: []eqRule{{"ops chunk snapshot", c.answers}}, n: make([]int, 1)}
		if c.answers == nil {
			node.rules[0].answers = []eqAnswer{{}}
		}
		actx, p := deadlineCtx(t, &declared.SimHost{Inner: node})
		var logs []string
		actx.Log = func(f string, a ...interface{}) { logs = append(logs, fmt.Sprintf(f, a...)) }
		actx.Bundle = &tr.RunBundle{Dir: t.TempDir()}
		for i := 0; i < snapshots(c.answers); i++ {
			if _, err := blockActions.Handler("v3_op_snapshot")(context.Background(), actx, tr.Action{Params: map[string]string{"volume": "v1", "name": fmt.Sprintf("s%d", i)}}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := v3HistoryEnd(context.Background(), actx, tr.Action{}); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, l := range logs {
			found = found || l == c.line
		}
		if !found {
			t.Errorf("%s: no line %q in %q", name, c.line, logs)
		}
		want := map[string]any{"conflicts": float64(c.k), "attempts": float64(c.n)}
		var rec map[string]any
		for _, r := range historyRecords(t, p) {
			if r.Verb == "answer_counts" {
				rec = r.Result
			}
		}
		if fmt.Sprint(rec["snapshot_head_conflict"]) != fmt.Sprint(want) {
			t.Errorf("%s: answer_counts %v", name, rec)
		}
		b, err := os.ReadFile(filepath.Join(actx.Bundle.Dir, "answer-counts.json"))
		var file map[string]any
		if err != nil || json.Unmarshal(b, &file) != nil || fmt.Sprint(file["snapshot_head_conflict"]) != fmt.Sprint(want) {
			t.Errorf("%s: answer-counts.json %s %v", name, b, err)
		}
	}
}

// snapshots is how many snapshot steps a case runs: one per answer, but a
// head_conflict and its rerun are one step.
func snapshots(answers []eqAnswer) int {
	n := 0
	for _, a := range answers {
		if !strings.Contains(a.out, "head_conflict") {
			n++
		}
	}
	return n
}
