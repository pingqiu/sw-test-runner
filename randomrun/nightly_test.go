package randomrun

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
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
	all := []NightRun{{Index: 0, Exit: 0, Coverage: ok1}, {Index: 1, Exit: 0, Coverage: ok2}}
	rep := Judge(7, all)
	if rep.Verdict != "PASS" || rep.Runs[0].Verdict != "PASS" || rep.UnionCells["write|up"] != 5 {
		t.Fatalf("%+v", rep)
	}
	// A cell one run never reached but the other did is reached in the union.
	if len(rep.UnionNeverReached) != 1 || rep.UnionNeverReached[0] != "delete|new" {
		t.Fatalf("union never reached %v", rep.UnionNeverReached)
	}
	for name, runs := range map[string][]NightRun{
		"one run exits nonzero":   {{Exit: 0, Coverage: ok1}, {Exit: 1, Coverage: ok2}},
		"one run has no coverage": {{Exit: 0, Coverage: ok1}, {Exit: 0}},
		"a coverage file is bad":  {{Exit: 0, Coverage: ok1}, {Exit: 0, Coverage: filepath.Join(t.TempDir(), "missing.json")}},
		"no runs at all":          {},
	} {
		if r := Judge(7, runs); r.Verdict != "FAIL" {
			t.Errorf("%s: night %s", name, r.Verdict)
		}
	}
}
