package randomrun

// The nightly: N short random runs in parallel plus one long run, each an
// INDEPENDENT run of a product's random-run scenario (its own seed, cluster,
// ports, work dir and name pool; sequential inside). The conditions:
//   - every run's seed comes from one recorded nightly seed (NightlySeeds),
//     and each run's header names both, so any one run replays alone;
//   - coverage is reported per run AND as a union;
//   - the night passes only if EVERY run passes: one failing bundle fails the
//     night, never an average;
//   - the long run is never dropped for time (drop a parallel run instead).

import (
	"bufio"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	tr "github.com/pingqiu/sw-test-runner"
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
	Checks   string `json:"checks"`   // the run's checks.json (artifacts/), if found
	History  string `json:"history"`  // the run's history.jsonl, if found
	Verdict  string `json:"verdict"`  // PASS only on exit 0 with a coverage file
	Error    string `json:"error,omitempty"`
	// Filled by Judge: a fault run (its coverage has fault cells), and the n
	// of its C6 finding "unknowns resolved: <n>" (0 when absent; ChecksNote
	// says why).
	Faults           bool `json:"faults"`
	UnknownsResolved int  `json:"unknowns_resolved"`
	// InOpHits are the run's in-operation fault hits by cell
	// ("<kind>|<action>|hit", its fault cells): reported BESIDE the unknowns
	// rule, never toward it (a kill that landed inside an operation may still
	// have been answered ok or refused; only C6's count says a request ended
	// unknown and was placed).
	InOpHits   map[string]int `json:"in_op_hits,omitempty"`
	ChecksNote string         `json:"checks_note,omitempty"`
	// The run's required checker set, from its header's "checkers", and the
	// ones in it that its checks.json does not hold (not run: the run FAILs).
	Checkers []string `json:"checkers"`
	NotRun   []string `json:"not_run,omitempty"`
	// Judged is, per required checker, the count of what it judged
	// (checks.json "judged"; -1 when absent); Failed lists the required
	// checkers that did not PASS with judged > 0, and why.
	Judged map[string]int `json:"judged"`
	Failed []string       `json:"failed,omitempty"`
}

// verdict is one checker's entry in checks.json.
type verdict struct {
	Verdict string `json:"verdict"`
	Judged  *int   `json:"judged"`
}

// requiredCheckers reads the checker set from a history's gen_header.
func requiredCheckers(path string) ([]string, error) {
	if path == "" {
		return nil, fmt.Errorf("no history.jsonl in the bundle")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<20), 16<<20)
	for s.Scan() {
		var r tr.HistoryRecord
		if json.Unmarshal(s.Bytes(), &r) != nil || r.Kind != tr.HistoryObserve || r.Verb != "gen_header" {
			continue
		}
		cs, _ := r.Result["checkers"].(string)
		if !checkerSet.MatchString(cs) {
			return nil, fmt.Errorf("the run's header names no checker set (param checkers)")
		}
		return strings.Split(cs, ","), nil
	}
	return nil, fmt.Errorf("the history has no gen_header")
}

// checkersRun lists the checkers a checks.json holds a verdict for.
func checkersRun(path string) (map[string]verdict, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var checks map[string]json.RawMessage
	if err := json.Unmarshal(b, &checks); err != nil {
		return nil, fmt.Errorf("checks.json does not parse")
	}
	out := map[string]verdict{}
	for k, raw := range checks {
		var v verdict
		_ = json.Unmarshal(raw, &v) // an entry of another shape has no verdict and no count
		out[k] = v
	}
	return out, nil
}

// NightReport is the night's verdict and the union coverage.
type NightReport struct {
	NightlySeed       uint64         `json:"nightly_seed"`
	Runs              []NightRun     `json:"runs"`
	UnionCells        map[string]int `json:"union_cells"`
	// UnionFaultCells sums the runs' fault cells (kill|p, while_down|a, the
	// in-operation hit cells): a run's cells_never_reached lists fault cells
	// too, and a cell is reached in the union when any run reached it, an
	// ordinary cell or a fault cell.
	UnionFaultCells   map[string]int `json:"union_fault_cells,omitempty"`
	UnionNeverReached []string       `json:"union_cells_never_reached"`
	Verdict           string         `json:"verdict"`
	// Findings are the night's reasons for FAIL: each failing run's (its
	// index, seed and error) and the night-level rules'.
	Findings []string `json:"findings,omitempty"`
	// InOp are the night's in-operation fault hits, one line per cell, beside
	// the unknowns rule: they neither satisfy nor replace it.
	InOp []string `json:"in_op,omitempty"`
}

// resolvedPrefix is the C6 finding that counts the unknowns resolved by
// observation at a healthy point.
const resolvedPrefix = "unknowns resolved: "

// unknownsResolved reads n from a checks.json's C6 finding
// "unknowns resolved: <n>".
func unknownsResolved(path string) (int, string) {
	if path == "" {
		return 0, "no checks.json in the bundle"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err.Error()
	}
	var checks map[string]struct {
		Findings []string `json:"findings"`
	}
	if err := json.Unmarshal(b, &checks); err != nil {
		return 0, "checks.json does not parse"
	}
	for _, f := range checks["C6"].Findings {
		if s, ok := strings.CutPrefix(f, resolvedPrefix); ok {
			n, err := strconv.Atoi(s)
			if err != nil || n < 0 {
				return 0, fmt.Sprintf("C6 finding %q", f)
			}
			return n, ""
		}
	}
	return 0, "C6 has no finding \"" + resolvedPrefix + "<n>\""
}

// Judge fills each run's verdict and the night's: a run PASSes only with
// exit 0, a readable coverage file, and a checks.json in which every
// checker of the run's required set (its header's checkers) PASSes with
// its own count above zero ("judged" > 0; one missing is "not run", a
// count of 0 or none is "<C> judged nothing"; a checker present but not in
// the set is ignored). Each run reports its table checker -> judged. The
// night PASSes only if every run does. Union coverage sums the runs' cells; a cell is never reached in the
// union only if no run reached it. A night with fault runs also needs one run
// whose C6 resolved at least one unknown ("unknowns resolved: <n>", n > 0):
// otherwise the faults exercised nothing C6 can judge, and the night FAILs.
func Judge(nightly uint64, runs []NightRun) NightReport {
	rep := NightReport{NightlySeed: nightly, UnionCells: map[string]int{}, Verdict: "PASS"}
	never := map[string]int{} // cell -> number of runs that never reached it
	for _, r := range runs {
		r.Verdict = "FAIL"
		var cov struct {
			Cells  map[string]int `json:"cells"`
			Never  []string       `json:"cells_never_reached"`
			Faults map[string]int `json:"fault_cells"`
		}
		r.UnknownsResolved, r.ChecksNote = unknownsResolved(r.Checks)
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
			for k, v := range cov.Faults {
				if rep.UnionFaultCells == nil {
					rep.UnionFaultCells = map[string]int{}
				}
				rep.UnionFaultCells[k] += v
			}
			for _, c := range cov.Never {
				never[c]++
			}
			r.Faults = cov.Faults != nil
			for c, n := range cov.Faults {
				if strings.HasSuffix(c, "|hit") && n > 0 {
					if r.InOpHits == nil {
						r.InOpHits = map[string]int{}
					}
					r.InOpHits[c] = n
				}
			}
			req, rerr := requiredCheckers(r.History)
			r.Checkers = req
			var ran map[string]verdict
			if rerr == nil {
				if r.Checks == "" {
					rerr = fmt.Errorf("no checks.json in the bundle")
				} else {
					ran, rerr = checkersRun(r.Checks)
				}
			}
			r.Judged = map[string]int{}
			for _, c := range req {
				if rerr != nil {
					break
				}
				v, present := ran[c]
				switch {
				case !present:
					r.NotRun = append(r.NotRun, c)
					continue
				case v.Judged == nil:
					r.Judged[c] = -1
				default:
					r.Judged[c] = *v.Judged
				}
				// Each required checker passes with its own count above zero.
				switch {
				case v.Verdict != "PASS":
					r.Failed = append(r.Failed, fmt.Sprintf("%s verdict %s", c, v.Verdict))
				case v.Judged == nil || *v.Judged <= 0:
					r.Failed = append(r.Failed, c+" judged nothing")
				}
			}
			why := ""
			switch {
			case rerr != nil:
				why = "checker set: " + rerr.Error()
			case len(r.NotRun) > 0:
				why = "not run: " + strings.Join(r.NotRun, ", ")
			case len(r.Failed) > 0:
				why = strings.Join(r.Failed, "; ")
			case r.Exit != 0:
				why = fmt.Sprintf("run exited %d", r.Exit)
			default:
				r.Verdict = "PASS"
			}
			if why != "" && r.Error == "" {
				r.Error = why
			} else if why != "" && why != r.Error {
				r.Error += "; " + why
			}
		}
		if r.Verdict != "PASS" {
			rep.Verdict = "FAIL"
			rep.Findings = append(rep.Findings, fmt.Sprintf("run %d (seed %d) FAILs: %s", r.Index, r.Seed, r.Error))
		}
		rep.Runs = append(rep.Runs, r)
	}
	// A cell is never reached in the union when some run reports it unreached
	// and no run reached it, as an ordinary cell or as a fault cell.
	for c := range never {
		if rep.UnionCells[c] == 0 && rep.UnionFaultCells[c] == 0 {
			rep.UnionNeverReached = append(rep.UnionNeverReached, c)
		}
	}
	slices.Sort(rep.UnionNeverReached)
	faults, resolved := 0, 0
	for _, r := range rep.Runs {
		if r.Faults {
			faults++
		}
		if r.UnknownsResolved > 0 {
			resolved++
		}
	}
	// The unknowns rule, on C6's count alone: an in-operation hit is no
	// unknown resolved (a kill inside an operation may be answered ok).
	if faults > 0 && resolved == 0 {
		rep.Verdict = "FAIL"
		rep.Findings = append(rep.Findings, fmt.Sprintf("%d fault runs, and no run's C6 resolved an unknown (%s<n> with n > 0)", faults, resolvedPrefix))
	}
	// Beside it, never toward it: the in-operation hits.
	hits, inRuns := map[string]int{}, map[string]int{}
	for _, r := range rep.Runs {
		for c, n := range r.InOpHits {
			hits[c] += n
			inRuns[c]++
		}
	}
	for _, c := range slices.Sorted(maps.Keys(hits)) {
		rep.InOp = append(rep.InOp, fmt.Sprintf("in-operation hit %s: %d in %d of %d runs (beside the unknowns rule, never toward it)", c, hits[c], inRuns[c], len(rep.Runs)))
	}
	if len(runs) == 0 {
		rep.Verdict = "FAIL"
	}
	return rep
}
