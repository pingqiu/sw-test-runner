package v3block

// The M1 nightly: N short random runs in parallel plus one long run, each an
// INDEPENDENT run of scenarios/v3block/m1-random.yaml (its own seed,
// blockmaster, SeaweedFS, ports, work dir and name pool; sequential inside).
// The architect's conditions:
//   - every run's seed comes from one recorded nightly seed (NightlySeeds),
//     and each run's header names both, so any one run replays alone;
//   - coverage is reported per run AND as a union;
//   - the night passes only if EVERY run passes: one failing bundle fails the
//     night, never an average;
//   - the long run is never dropped for time (drop a parallel run instead).

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
)

// splitmix64 is a fixed, well-mixed function: run i's seed is
// splitmix64(nightly + i), stable across versions and platforms.
func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// NightlySeeds returns the seeds of runs 0..n-1 of one night.
func NightlySeeds(nightly uint64, n int) []uint64 {
	out := make([]uint64, n)
	for i := range out {
		out[i] = splitmix64(nightly + uint64(i))
	}
	return out
}

// NightRun is one run of the night as the driver saw it.
type NightRun struct {
	Index    int    `json:"index"`
	Kind     string `json:"kind"` // "short" or "long"
	Seed     uint64 `json:"seed"`
	Ops      int    `json:"ops"`
	PortBase int    `json:"port_base"`
	Tag      string `json:"run_tag"`
	Exit     int    `json:"exit"`
	Bundle   string `json:"bundle"`
	Coverage string `json:"coverage"` // the run's gen-coverage.json, if found
	Verdict  string `json:"verdict"`  // PASS only on exit 0 with a coverage file
	Error    string `json:"error,omitempty"`
}

// NightReport is the night's verdict and the union coverage.
type NightReport struct {
	NightlySeed       uint64         `json:"nightly_seed"`
	Runs              []NightRun     `json:"runs"`
	UnionCells        map[string]int `json:"union_cells"`
	UnionNeverReached []string       `json:"union_cells_never_reached"`
	Verdict           string         `json:"verdict"`
}

// Judge fills each run's verdict and the night's: a run PASSes only with
// exit 0 and a readable coverage file; the night PASSes only if every run
// does. Union coverage sums the runs' cells; a cell is never reached in the
// union only if no run reached it.
func Judge(nightly uint64, runs []NightRun) NightReport {
	rep := NightReport{NightlySeed: nightly, UnionCells: map[string]int{}, Verdict: "PASS"}
	never := map[string]int{} // cell -> number of runs that never reached it
	for _, r := range runs {
		r.Verdict = "FAIL"
		var cov struct {
			Cells map[string]int `json:"cells"`
			Never []string       `json:"cells_never_reached"`
		}
		b, err := os.ReadFile(r.Coverage)
		switch {
		case r.Coverage == "":
			r.Error = "no gen-coverage.json in the bundle"
		case err != nil:
			r.Error = err.Error()
		case json.Unmarshal(b, &cov) != nil:
			r.Error = "gen-coverage.json does not parse"
		default:
			for k, v := range cov.Cells {
				rep.UnionCells[k] += v
			}
			for _, c := range cov.Never {
				never[c]++
			}
			if r.Exit == 0 {
				r.Verdict = "PASS"
			} else if r.Error == "" {
				r.Error = fmt.Sprintf("run exited %d", r.Exit)
			}
		}
		if r.Verdict != "PASS" {
			rep.Verdict = "FAIL"
		}
		rep.Runs = append(rep.Runs, r)
	}
	// A cell is never reached in the union when some run reports it unreached
	// and no run reached it.
	for c := range never {
		if rep.UnionCells[c] == 0 {
			rep.UnionNeverReached = append(rep.UnionNeverReached, c)
		}
	}
	slices.Sort(rep.UnionNeverReached)
	if len(runs) == 0 {
		rep.Verdict = "FAIL"
	}
	return rep
}
