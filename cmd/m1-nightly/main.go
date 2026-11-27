// Command m1-nightly runs one night of the block product's M1 random runs:
// -parallel short runs at the same time plus one long run, each an
// independent run of the M1 scenario (its own seed, ports, work dir and
// cluster), then judges the night (randomrun.Judge) and writes
// nightly.json. It exits 0 only if every run passed.
//
//	m1-nightly -swblock ./swblock -scenario scenarios/v3block/m1-random.yaml \
//	  -results /var/tmp/m1-night -nightly-seed 20261127 \
//	  -env product_src=... -env runner_src=... [-parallel 4 -short-ops 2500 -long-ops 10000]
//
// Run i (0..parallel-1 short, parallel = long) gets seed NightlySeeds(nightly)[i],
// port_base = -port-base + i*-port-step and run_tag = n<nightly>r<i> (lowercase letters and digits: the scenario's rule).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pingqiu/sw-test-runner/randomrun"
)

type envList []string

func (e *envList) String() string     { return strings.Join(*e, ",") }
func (e *envList) Set(v string) error { *e = append(*e, v); return nil }

// plan lays out the night's runs without running anything.
func plan(nightly uint64, parallel, shortOps, longOps, portBase, portStep int) []randomrun.NightRun {
	seeds := randomrun.NightlySeeds(nightly, parallel+1)
	var runs []randomrun.NightRun
	for i := 0; i <= parallel; i++ {
		r := randomrun.NightRun{Index: i, Kind: "short", Seed: seeds[i], Ops: shortOps,
			PortBase: portBase + i*portStep, Tag: fmt.Sprintf("n%dr%d", nightly, i)}
		if i == parallel {
			r.Kind, r.Ops = "long", longOps
		}
		runs = append(runs, r)
	}
	return runs
}

// argv is one run's swblock command line.
func argv(swblock, scenario, results string, nightly uint64, r randomrun.NightRun, env []string) []string {
	a := []string{swblock, "run", "-results-dir", filepath.Join(results, fmt.Sprintf("run-%d", r.Index))}
	for _, kv := range env {
		a = append(a, "-env", kv)
	}
	for _, kv := range []string{
		"gen_seed=" + strconv.FormatUint(r.Seed, 10), "gen_ops=" + strconv.Itoa(r.Ops),
		"nightly_seed=" + strconv.FormatUint(nightly, 10), "port_base=" + strconv.Itoa(r.PortBase), "run_tag=" + r.Tag,
	} {
		a = append(a, "-env", kv)
	}
	return append(a, scenario)
}

// findFile returns the first file called name under dir, or "": the run's
// gen-coverage.json and its artifacts/checks.json.
func findFile(dir, name string) string {
	found := ""
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == name && found == "" {
			found = p
			return fs.SkipAll
		}
		return nil
	})
	return found
}

func main() {
	var env envList
	swblock := flag.String("swblock", "", "the swblock binary")
	scenario := flag.String("scenario", "scenarios/v3block/m1-random.yaml", "the M1 scenario")
	results := flag.String("results", "", "the night's results directory (one run-<i> per run)")
	nightlyFlag := flag.Uint64("nightly-seed", 0, "the night's seed (default: today's date as yyyymmdd)")
	parallel := flag.Int("parallel", 4, "short runs at the same time (the long run is always added)")
	shortOps := flag.Int("short-ops", 2500, "operations per short run")
	longOps := flag.Int("long-ops", 10000, "operations of the long run")
	portBase := flag.Int("port-base", 20000, "first run's port base")
	portStep := flag.Int("port-step", 1000, "port distance between runs (a run spans port_base..+10*names+7 and +10000..+10002)")
	dry := flag.Bool("dry-run", false, "print the plan and the command lines, run nothing")
	flag.Var(&env, "env", "key=value passed to every run (repeatable)")
	flag.Parse()

	nightly := *nightlyFlag
	if nightly == 0 {
		nightly, _ = strconv.ParseUint(time.Now().UTC().Format("20060102"), 10, 64)
	}
	if *swblock == "" || *results == "" || *parallel < 0 || *shortOps <= 0 || *longOps <= 0 || *portStep < 1000 {
		fmt.Fprintln(os.Stderr, "m1-nightly: -swblock and -results are required; ops positive; -port-step at least 1000")
		os.Exit(2)
	}
	runs := plan(nightly, *parallel, *shortOps, *longOps, *portBase, *portStep)
	if *dry {
		for _, r := range runs {
			fmt.Println(strings.Join(argv(*swblock, *scenario, *results, nightly, r, env), " "))
		}
		return
	}
	if err := os.MkdirAll(*results, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "m1-nightly:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var wg sync.WaitGroup
	for i := range runs {
		wg.Add(1)
		go func(r *randomrun.NightRun) {
			defer wg.Done()
			a := argv(*swblock, *scenario, *results, nightly, *r, env)
			dir := filepath.Join(*results, fmt.Sprintf("run-%d", r.Index))
			_ = os.MkdirAll(dir, 0o755)
			log, err := os.Create(filepath.Join(dir, "driver.log"))
			if err != nil {
				r.Exit, r.Error = -1, err.Error()
				return
			}
			defer log.Close()
			cmd := exec.CommandContext(ctx, a[0], a[1:]...)
			cmd.Stdout, cmd.Stderr = log, log
			cmd.WaitDelay = 30 * time.Second
			if err := cmd.Run(); err != nil {
				r.Exit = -1
				if ee, ok := err.(*exec.ExitError); ok {
					r.Exit = ee.ExitCode()
				}
				r.Error = err.Error()
			}
			r.Bundle = dir
			r.Coverage = findFile(dir, "gen-coverage.json")
			r.Checks = findFile(dir, "checks.json")
		}(&runs[i])
	}
	wg.Wait()
	rep := randomrun.Judge(nightly, runs)
	b, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(filepath.Join(*results, "nightly.json"), b, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "m1-nightly:", err)
		os.Exit(2)
	}
	fmt.Printf("m1-nightly: nightly seed %d, %d runs, verdict %s\n", nightly, len(runs), rep.Verdict)
	if rep.Verdict != "PASS" {
		os.Exit(1)
	}
}
