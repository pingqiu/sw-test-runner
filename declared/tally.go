package declared

// Per-run answer counts of the declared actions: per verb, how many
// attempts were made and how many answered each error code. The run's end
// writes them (an observe record "answer_counts"), so a run reports, say,
// its snapshot head_conflicts per snapshot attempted.

import (
	"strings"
	"sync"

	tr "github.com/pingqiu/sw-test-runner"
)

type verbCount struct {
	Attempted int            `json:"attempted"`
	Codes     map[string]int `json:"codes"`
}

type tallyList struct {
	mu    sync.Mutex
	verbs map[string]*verbCount
}

var tallies sync.Map // *tr.History -> *tallyList

func tally(h *tr.History, verb string, res map[string]any) {
	if h == nil {
		return
	}
	v, _ := tallies.LoadOrStore(h, &tallyList{verbs: map[string]*verbCount{}})
	t := v.(*tallyList)
	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.verbs[verb]
	if c == nil {
		c = &verbCount{Codes: map[string]int{}}
		t.verbs[verb] = c
	}
	c.Attempted++
	if d, _ := res["detail"].(string); d != "" {
		if code, _, ok := strings.Cut(d, ": "); ok && code != "" && !strings.ContainsAny(code, " ") {
			c.Codes[code]++
		}
	}
}

// Counts returns h's answer counts, {verb: {attempted, codes}}, and stops
// counting for it.
func Counts(h *tr.History) map[string]any {
	out := map[string]any{}
	v, ok := tallies.LoadAndDelete(h)
	if !ok {
		return out
	}
	t := v.(*tallyList)
	t.mu.Lock()
	defer t.mu.Unlock()
	for verb, c := range t.verbs {
		codes := map[string]any{}
		for k, n := range c.Codes {
			codes[k] = n
		}
		out[verb] = map[string]any{"attempted": c.Attempted, "codes": codes}
	}
	return out
}
