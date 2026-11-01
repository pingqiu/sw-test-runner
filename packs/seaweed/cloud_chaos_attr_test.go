package seaweed

import (
	"strings"
	"testing"
)

// One op per second from 0.5 s; fail() picks the failed ones; kinds cycle.
func attrOps(n int, fail func(i int, start float64) bool) []chaosOp {
	kinds := []string{"kv_set", "kv_get", "s3_put", "s3_get"}
	ops := make([]chaosOp, n)
	for i := range ops {
		s := 0.5 + float64(i)
		ops[i] = chaosOp{Op: i, Kind: kinds[i%4], Key: "k" + string(rune('a'+i%26)), Started: s, Ended: s + .5, Exact: !fail(i, s)}
	}
	return ops
}

func TestChaosPerOpExcusal(t *testing.T) {
	two := []float64{30, 100} // e.g. a kill at 30 and its restart at 100
	for _, tc := range []struct {
		name string
		ops  []chaosOp
		w    chaosWindows
		ok   bool
	}{
		{"no failures", attrOps(160, func(int, float64) bool { return false }), chaosWindows{Marks: two}, true},
		{"failures inside one window", attrOps(160, func(_ int, s float64) bool { return s > 30 && s < 40 }), chaosWindows{Marks: two}, true},
		{"failures judged against the restart marker", attrOps(160, func(_ int, s float64) bool { return s > 100 && s < 110 }), chaosWindows{Marks: two}, true},
		{"failure before any marker", attrOps(160, func(_ int, s float64) bool { return s > 20 && s < 22 }), chaosWindows{Marks: two}, false},
		{"failure straddling the marker", attrOps(160, func(_ int, s float64) bool { return s == 29.5 }), chaosWindows{Marks: two}, false},
		{"failure past marker+60 while still down", attrOps(160, func(_ int, s float64) bool { return s > 89 && s < 92 }), chaosWindows{Marks: two}, false},
		{"no same-kind recovery by marker+60", attrOps(100, func(i int, s float64) bool { return s > 30 && i%4 == 1 }), chaosWindows{Marks: []float64{30}}, false},
		{"C5 key unavailable until ready+60", attrOps(160, func(i int, s float64) bool { return i%4 == 1 && s > 30 && s < 150 && i%26 == 1 }), chaosWindows{Marks: two, Unavailable: map[string]bool{"kb": true}, ReadyMono: 95}, true},
		{"C5 key unavailable past ready+60", attrOps(170, func(i int, s float64) bool { return i%4 == 1 && s > 30 && s < 165 && i%26 == 1 }), chaosWindows{Marks: two, Unavailable: map[string]bool{"kb": true}, ReadyMono: 95}, false},
		// op 92 is a kv_set at 92.5 s on key "ko": past P=30+60, so only a
		// (wrong) write excusal for unavailable keys could accept it.
		{"C5 excusal is for reads only", attrOps(160, func(i int, s float64) bool { return i == 92 }), chaosWindows{Marks: two, Unavailable: map[string]bool{"ko": true}, ReadyMono: 95}, false},
		{"no markers", attrOps(10, func(int, float64) bool { return false }), chaosWindows{Marks: nil}, false},
		{"load ended before the first marker", attrOps(25, func(int, float64) bool { return false }), chaosWindows{Marks: two}, false},
		{"load started after the first marker", attrOps(160, func(int, float64) bool { return false })[35:], chaosWindows{Marks: two}, false},
		{"load ended before the last marker", attrOps(90, func(int, float64) bool { return false }), chaosWindows{Marks: two}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkChaosOps(tc.ops, tc.w); (err == nil) != tc.ok {
				t.Fatal(err)
			}
		})
	}
	// A load that never ACKs one protocol is NOT_COVERED (Claude01 6ae74dfb).
	for _, kind := range []string{"kv_set", "s3_put"} {
		ops := attrOps(160, func(int, float64) bool { return false })
		for i := range ops {
			if ops[i].Kind == kind {
				ops[i].Kind = "kv_get"
			}
		}
		if err := checkChaosOps(ops, chaosWindows{Marks: two}); err == nil || !strings.HasPrefix(err.Error(), "NOT_COVERED") {
			t.Fatalf("no %s ACK: %v", kind, err)
		}
	}
	ops := attrOps(10, func(int, float64) bool { return false })
	ops[3].Op = 7
	if checkChaosOps(ops, chaosWindows{Marks: two}) == nil {
		t.Fatal("op binding not checked")
	}
}
