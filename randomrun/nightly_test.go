package randomrun

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
)

func TestNightlySeedsAreStableDistinctAndDerivedFromTheNightlySeed(t *testing.T) {
	a := NightlySeeds(20261127, 5)
	b := NightlySeeds(20261127, 5)
	for i := range a {
		if a[i] != b[i] {
			t.Fatal("not stable")
		}
		for j := range a {
			if i != j && a[i] == a[j] {
				t.Fatal("two runs share a seed")
			}
		}
	}
	if c := NightlySeeds(20261128, 5); c[0] == a[0] {
		t.Fatal("another night gave the same first seed")
	}
	// Pinned values: a run replays from (nightly, index) alone, so the function
	// may never change silently.
	if got := NightlySeeds(0, 1)[0]; got != 0xe220a8397b1dcdaf {
		t.Fatalf("splitmix64(0) = %#x", got)
	}
}

func coverageFile(t *testing.T, cells map[string]int, never []string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gen-coverage.json")
	b, _ := json.Marshal(map[string]any{"cells": cells, "cells_never_reached": never})
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNightPassesOnlyIfEveryRunPasses(t *testing.T) {
	ok1 := coverageFile(t, map[string]int{"write|up": 5}, []string{"move|up"})
	ok2 := coverageFile(t, map[string]int{"move|up": 1}, []string{"delete|new"})
	h, ck := historyFile(t, "C0,C6"), checksFile(t)
	all := []NightRun{{Index: 0, Exit: 0, Coverage: ok1, History: h, Checks: ck}, {Index: 1, Exit: 0, Coverage: ok2, History: h, Checks: ck}}
	rep := Judge(7, all)
	if rep.Verdict != "PASS" || rep.Runs[0].Verdict != "PASS" || rep.UnionCells["write|up"] != 5 {
		t.Fatalf("%+v", rep)
	}
	// A cell one run never reached but the other did is reached in the union.
	if len(rep.UnionNeverReached) != 1 || rep.UnionNeverReached[0] != "delete|new" {
		t.Fatalf("union never reached %v", rep.UnionNeverReached)
	}
	for name, runs := range map[string][]NightRun{
		"one run exits nonzero":   {{Exit: 0, Coverage: ok1, History: h, Checks: ck}, {Exit: 1, Coverage: ok2, History: h, Checks: ck}},
		"one run has no coverage": {{Exit: 0, Coverage: ok1, History: h, Checks: ck}, {Exit: 0, History: h, Checks: ck}},
		"a coverage file is bad":  {{Exit: 0, Coverage: ok1, History: h, Checks: ck}, {Exit: 0, Coverage: filepath.Join(t.TempDir(), "missing.json"), History: h, Checks: ck}},
		"no runs at all":          {},
	} {
		if r := Judge(7, runs); r.Verdict != "FAIL" {
			t.Errorf("%s: night %s", name, r.Verdict)
		}
	}
}

func writeJSON(t *testing.T, name string, v any) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	b, _ := json.Marshal(v)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// historyFile is a history whose gen_header names the checker set cs ("":
// none).
func historyFile(t *testing.T, cs string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "history.jsonl")
	h, err := tr.OpenHistory(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	res := map[string]any{"generator": "x/1"}
	if cs != "" {
		res["checkers"] = cs
	}
	_ = h.Observe(tr.HistoryObserve, tr.HistoryRecord{Client: "gen", Verb: "gen_header", Result: res})
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func checksFile(t *testing.T, c6 ...string) string {
	return writeJSON(t, "checks.json", map[string]any{"C0": map[string]any{"findings": []string{}, "verdict": "PASS"},
		"C6": map[string]any{"findings": c6, "verdict": "PASS"}})
}

// A night with fault runs needs one run whose C6 resolved an unknown.
func TestNightWithFaultRunsNeedsAnUnknownResolved(t *testing.T) {
	plain := coverageFile(t, map[string]int{"write|up": 5}, nil)
	fault := writeJSON(t, "gen-coverage.json", map[string]any{"cells": map[string]int{"write|up": 5}, "fault_cells": map[string]int{"kill|p0": 1}})
	none, two := checksFile(t, "unknowns resolved: 0"), checksFile(t, "unknown v1 seq 9 resolved", "unknowns resolved: 2")
	h := historyFile(t, "C0")

	rep := Judge(7, []NightRun{{Exit: 0, Coverage: fault, Checks: none, History: h}, {Index: 1, Exit: 0, Coverage: fault, Checks: two, History: h}})
	if rep.Verdict != "PASS" || !rep.Runs[0].Faults || rep.Runs[0].UnknownsResolved != 0 || rep.Runs[1].UnknownsResolved != 2 {
		t.Fatalf("one fault run resolved two: %+v", rep)
	}
	// Red controls: fault runs, and no run resolved anything.
	for name, runs := range map[string][]NightRun{
		"every fault run resolved 0":    {{Exit: 0, Coverage: fault, Checks: none, History: h}, {Exit: 0, Coverage: plain, Checks: none, History: h}},
		"no checks.json":                {{Exit: 0, Coverage: fault, History: h}},
		"C6 without the count":          {{Exit: 0, Coverage: fault, Checks: checksFile(t, "unknown v1 seq 9 resolved"), History: h}},
		"a count that is not a number":  {{Exit: 0, Coverage: fault, Checks: checksFile(t, "unknowns resolved: two"), History: h}},
		"a checks.json that is no JSON": {{Exit: 0, Coverage: fault, Checks: writeJSON(t, "checks.json", "x"), History: h}},
	} {
		r := Judge(7, runs)
		if r.Verdict != "FAIL" || len(r.Findings) != 1 {
			t.Errorf("%s: night %s %v", name, r.Verdict, r.Findings)
		}
	}
	// A night without fault runs does not need one.
	if r := Judge(7, []NightRun{{Exit: 0, Coverage: plain, Checks: none, History: h}}); r.Verdict != "PASS" || r.Runs[0].Faults {
		t.Fatalf("a night without faults: %+v", r)
	}
	// The count is read whatever the run's kind; a plain run's count also satisfies a night with fault runs.
	if r := Judge(7, []NightRun{{Exit: 0, Coverage: fault, Checks: none, History: h}, {Exit: 0, Coverage: plain, Checks: two, History: h}}); r.Verdict != "PASS" {
		t.Fatalf("%+v", r)
	}
}

// A run's required checker set (its header's checkers) must all be in its
// checks.json: one missing is "not run" and fails the run and the night; one
// present but not required is ignored.
func TestNightRequiresTheRunsCheckerSet(t *testing.T) {
	cov := coverageFile(t, map[string]int{"write|up": 1}, nil)
	ck := checksFile(t) // holds C0 and C6
	rep := Judge(7, []NightRun{{Exit: 0, Coverage: cov, History: historyFile(t, "C0"), Checks: ck}})
	if rep.Verdict != "PASS" || strings.Join(rep.Runs[0].Checkers, ",") != "C0" || len(rep.Runs[0].NotRun) != 0 {
		t.Fatalf("C6 present but not required must be ignored: %+v", rep)
	}
	rep = Judge(7, []NightRun{{Exit: 0, Coverage: cov, History: historyFile(t, "C0,C5,C6,MO"), Checks: ck}})
	r := rep.Runs[0]
	if rep.Verdict != "FAIL" || r.Verdict != "FAIL" || strings.Join(r.NotRun, ",") != "C5,MO" || r.Error != "not run: C5, MO" {
		t.Fatalf("a required checker missing: %+v", rep)
	}
	for name, run := range map[string]NightRun{
		"no checker set in the header": {Exit: 0, Coverage: cov, History: historyFile(t, ""), Checks: ck},
		"no history":                   {Exit: 0, Coverage: cov, Checks: ck},
		"no checks.json":               {Exit: 0, Coverage: cov, History: historyFile(t, "C0")},
		"a checks.json that is no JSON": {Exit: 0, Coverage: cov, History: historyFile(t, "C0"),
			Checks: writeJSON(t, "checks.json", "x")},
	} {
		if r := Judge(7, []NightRun{run}); r.Verdict != "FAIL" {
			t.Errorf("%s: night %s %+v", name, r.Verdict, r.Runs)
		}
	}
}
