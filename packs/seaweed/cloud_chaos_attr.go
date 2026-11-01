package seaweed

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

// chaosOp is one chaos_load.py outcome, on the client's own CLOCK_MONOTONIC.
type chaosOp struct {
	Op      int     `json:"op"`
	Kind    string  `json:"kind"`
	Key     string  `json:"key"`
	Started float64 `json:"started"`
	Ended   float64 `json:"ended"`
	Exact   bool    `json:"exact"`
	Error   string  `json:"error"`
}

// chaosWindows are one client's markers for one case: a P before every fault
// action (kill, restart, partition, heal, ...), and for C5 the keys expected
// unavailable with the client's stamp taken after the victims were ready.
type chaosWindows struct {
	Marks       []float64
	Unavailable map[string]bool
	ReadyMono   float64
}

// checkChaosOps is the stage 2 per-op excusal (Claude01 6ae6e92a, Q1/Q2/Q6).
// A failed op is excused only if, for the latest marker P <= its start, it
// ends by P+60 and a same-kind exact op completes after it, also by P+60.
// A C5 key whose both copies were on the killed pair may instead fail from
// the first marker until ReadyMono+60. Everything else fails the case.
func checkChaosOps(ops []chaosOp, w chaosWindows) error {
	if len(ops) == 0 || len(w.Marks) == 0 {
		return errors.New("chaos ops or markers missing")
	}
	marks := append([]float64{}, w.Marks...)
	sort.Float64s(marks)
	for _, m := range marks {
		if m <= 0 || math.IsInf(m, 0) || math.IsNaN(m) {
			return errors.New("invalid chaos marker")
		}
	}
	for i, o := range ops {
		if o.Op != i || o.Ended < o.Started || (i > 0 && o.Started < ops[i-1].Ended) {
			return fmt.Errorf("op %d order/binding differs", i)
		}
	}
	// The load must span the fault on this client's own clock: running before
	// the first action and still issuing ops after the last (codex01 #49 R2).
	if ops[0].Started >= marks[0] || ops[len(ops)-1].Started <= marks[len(marks)-1] {
		return errors.New("NOT_COVERED: load does not span the fault actions")
	}
	// Every client must exercise both protocols: zero KV or zero S3 ACKs is
	// NOT_COVERED, never PASS (Claude01 6ae74dfb).
	acked := map[string]bool{}
	for _, o := range ops {
		acked[o.Kind] = acked[o.Kind] || o.Exact
	}
	if !acked["kv_set"] || !acked["s3_put"] {
		return fmt.Errorf("NOT_COVERED: KV ACKed %t, S3 ACKed %t", acked["kv_set"], acked["s3_put"])
	}
	for _, f := range ops {
		if f.Exact {
			continue
		}
		if len(w.Unavailable) > 0 && w.Unavailable[f.Key] && (f.Kind == "kv_get" || f.Kind == "s3_get") && f.Started >= marks[0] && f.Ended <= w.ReadyMono+60 {
			continue
		}
		p := 0.
		for _, m := range marks {
			if m <= f.Started {
				p = m
			}
		}
		if p == 0 {
			return fmt.Errorf("op %d %s %s failed before any fault marker", f.Op, f.Kind, f.Key)
		}
		if f.Ended > p+60 {
			return fmt.Errorf("op %d %s %s failed past its marker+60s", f.Op, f.Kind, f.Key)
		}
		recovered := false
		for _, g := range ops {
			recovered = recovered || g.Kind == f.Kind && g.Exact && g.Started >= f.Ended && g.Ended <= p+60
		}
		if !recovered {
			return fmt.Errorf("op %d %s %s: no exact %s by its marker+60s", f.Op, f.Kind, f.Key, f.Kind)
		}
	}
	return nil
}
