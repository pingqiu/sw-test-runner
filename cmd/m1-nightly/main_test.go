package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/pingqiu/sw-test-runner/randomrun"
)

func TestPlanGivesEachRunItsOwnSeedPortsAndTagAndAlwaysALongRun(t *testing.T) {
	runs := plan(20261127, 4, 2500, 10000, 20000, 1000)
	if len(runs) != 5 || runs[4].Kind != "long" || runs[4].Ops != 10000 || runs[0].Ops != 2500 {
		t.Fatalf("%+v", runs)
	}
	seeds := randomrun.NightlySeeds(20261127, 5)
	ports, tags := map[int]bool{}, map[string]bool{}
	for i, r := range runs {
		if r.Seed != seeds[i] {
			t.Fatalf("run %d seed %d is not NightlySeeds[%d]", i, r.Seed, i)
		}
		if ports[r.PortBase] || tags[r.Tag] {
			t.Fatalf("run %d shares a port base or tag", i)
		}
		ports[r.PortBase], tags[r.Tag] = true, true
		if !regexp.MustCompile(`^[a-z0-9]+$`).MatchString(r.Tag) {
			t.Fatalf("run %d tag %q is not lowercase letters and digits", i, r.Tag)
		}
	}
	// No parallel runs at all still keeps the long run.
	if only := plan(1, 0, 10, 100, 20000, 1000); len(only) != 1 || only[0].Kind != "long" {
		t.Fatalf("%+v", only)
	}
}

func TestArgvPassesTheRunsSeedNightlySeedPortsAndTag(t *testing.T) {
	r := plan(7, 1, 25, 100, 20000, 1000)[1]
	a := strings.Join(argv("/bin/swblock", "s.yaml", "/res", 7, r, []string{"product_src=/p"}), " ")
	for _, want := range []string{"-results-dir /res/run-1", "-env product_src=/p", "-env gen_ops=100",
		"-env nightly_seed=7", "-env port_base=21000", "-env run_tag=n7r1", " s.yaml"} {
		if !strings.Contains(strings.ReplaceAll(a, "\\", "/"), want) {
			t.Errorf("argv lacks %q: %s", want, a)
		}
	}
}

func TestFindFileFindsTheRunsCoverageAndChecks(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"r/gen-coverage.json", "r/artifacts/checks.json"} {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.HasSuffix(filepath.ToSlash(findFile(dir, "checks.json")), "r/artifacts/checks.json") || findFile(dir, "gen-coverage.json") == "" {
		t.Fatal("not found")
	}
	if findFile(dir, "nightly.json") != "" {
		t.Fatal("found a file that is not there")
	}
}

// The fault scenario's 5-seed condition: a sequential night of 5 runs, each
// of -long-ops operations, one seed each from the nightly seed, no parallel
// and no long run.
func TestSequentialPlanIsFiveRunsOneSeedEach(t *testing.T) {
	runs := planSequential(20261201, 5, 300, 20000, 1000)
	seeds := randomrun.NightlySeeds(20261201, 5)
	if len(runs) != 5 {
		t.Fatalf("%d runs", len(runs))
	}
	tags := map[string]bool{}
	for i, r := range runs {
		if r.Kind != "sequential" || r.Ops != 300 || r.Seed != seeds[i] || r.Index != i || tags[r.Tag] {
			t.Fatalf("run %d: %+v", i, r)
		}
		tags[r.Tag] = true
		a := strings.Join(argv("/bin/swblock", "scenarios/v3block/m2-faults.yaml", "/res", 20261201, r, nil), " ")
		if !strings.Contains(a, "gen_ops=300") || !strings.Contains(a, "gen_seed="+strconv.FormatUint(seeds[i], 10)) || !strings.HasSuffix(a, "m2-faults.yaml") {
			t.Fatalf("argv %s", a)
		}
	}
}
