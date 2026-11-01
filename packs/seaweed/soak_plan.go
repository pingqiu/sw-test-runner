package seaweed

import (
	"errors"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
)

// SoakPlan is the KV rotation soak (step 2 contract r3). It is exclusive with
// Plan.Restart. Client kill is not a runner fault; it lives in the step-3 worker.
type SoakPlan struct {
	DurationS   int           `json:"duration_s"`
	PacingS     float64       `json:"pacing_s"`
	Rotation    *Rotation     `json:"rotation"`
	ReadyAssign *AssignLayout `json:"ready_assign,omitempty"`
	// FaultReady are the ready probes run after each fault: the deploy-phase
	// ready steps carry no Assign probe (the layout does not exist yet and an
	// Assign would grow it), these carry ready_assign when a master fault exists.
	FaultReady []Step      `json:"fault_ready"`
	Faults     []SoakFault `json:"faults"`
	// Step 3: the cache layout setup grows, the client kill inside the load
	// worker, and the deploy-phase M02 setup job.
	Layout        AssignLayout `json:"layout"`
	GrowCount     int          `json:"grow_count"`
	ClientKillAtS int          `json:"client_kill_at_s"`
	Setup         Step         `json:"setup"`
}

type SoakFault struct {
	AtS     int    `json:"at_s"`
	Role    string `json:"role"`
	Episode string `json:"episode"`
}

// Rotation mirrors lab_profile.py master_toml(): the product's own rules plus caps.
type Rotation struct {
	Collections          string  `json:"collections"`
	HighWatermark        float64 `json:"high_watermark"`
	LowWatermark         float64 `json:"low_watermark"`
	MinAgeMinutes        int     `json:"min_age_minutes"`
	MaxVolumesPerCycle   int     `json:"max_volumes_per_cycle"`
	MinRemainingVolumes  int     `json:"min_remaining_volumes"`
	CheckIntervalSeconds int     `json:"check_interval_seconds"`
}

type AssignLayout struct {
	Collection  string `json:"collection"`
	Replication string `json:"replication"`
	TTL         string `json:"ttl"`
}

// Frozen soak budgets (contract r3, R3/R4). SoakHorizon is claude01's ruling
// (6ae7b460): a ceiling on the plan's hard deadline, never an extension.
const (
	SoakHorizon       = 60 * time.Minute
	KillSettleBound   = 30 * time.Second
	ReplacementEnroll = 15 * time.Second
	ReadyBound        = 45 * time.Second
	FaultBudget       = KillSettleBound + ReplacementEnroll + ReadyBound
	RecoveryMargin    = 60 * time.Second
	QuiescenceReserve = 60 * time.Second
	CollectReserve    = 120 * time.Second
	CleanupReserve    = 120 * time.Second
	DeployReserve     = 300 * time.Second
	OverdueTolerance  = 5 * time.Second
	WitnessWait       = 10 * time.Second
)

var (
	soakName        = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	soakReplication = regexp.MustCompile(`^[0-9]{3}$`)
	soakTTL         = regexp.MustCompile(`^([1-9][0-9]{0,4}[mhdwMy])?$`)
	// ErrSoakWorkerUnadmitted keeps every soak plan inert until step 3 lands
	// exact worker admission: no phase of a soak plan binds, so nothing runs.
	ErrSoakWorkerUnadmitted = errors.New("SOAK_WORKER_UNADMITTED: the soak worker has no exact admission yet")
)

func (r *Rotation) validate() error {
	if r == nil {
		return errors.New("soak rotation required")
	}
	for _, c := range strings.Split(r.Collections, ",") {
		if !soakName.MatchString(c) {
			return errors.New("soak rotation collections")
		}
	}
	if !(0 < r.LowWatermark && r.LowWatermark < r.HighWatermark && r.HighWatermark < 1) {
		return errors.New("soak rotation watermarks need 0 < low < high < 1")
	}
	for _, b := range []struct{ v, lo, hi int }{{r.MinAgeMinutes, 0, 1e6}, {r.MaxVolumesPerCycle, 1, 1e6}, {r.MinRemainingVolumes, 1, 1e6}, {r.CheckIntervalSeconds, 1, 86400}} {
		if b.v < b.lo || b.v > b.hi {
			return errors.New("soak rotation bounds")
		}
	}
	return nil
}

func (l *AssignLayout) validate() error {
	if l == nil || !soakName.MatchString(l.Collection) || !soakReplication.MatchString(l.Replication) || !soakTTL.MatchString(l.TTL) {
		return errors.New("soak ready_assign layout")
	}
	return nil
}

// profileSettings carries the soak-only profile fields into ProfileArgs.
// They are unexported on SmokeSettings, so plan JSON can never set them there.
func (p Plan) profileSettings(faultReady bool) SmokeSettings {
	s := p.Smoke
	if p.Soak != nil {
		s.rotation = p.Soak.Rotation
		if faultReady {
			s.readyAssign = p.Soak.ReadyAssign
		}
	}
	return s
}

func secs(n int) time.Duration { return time.Duration(n) * time.Second }

// checkSchedule bounds every number before any arithmetic on it (codex01 R1:
// an at_s near MaxInt64 overflowed the seconds math). bindSoak and runSoak
// both call it, so a direct runSoak call can't dispatch an unchecked plan.
func (x *SoakPlan) checkSchedule() error {
	if x == nil || x.DurationS <= 0 || x.DurationS > int(SoakHorizon/time.Second) {
		return errors.New("soak duration out of range")
	}
	if len(x.Faults) == 0 || len(x.Faults) > 2 {
		return errors.New("soak needs one or two faults")
	}
	roles, prev := map[string]bool{}, 0
	for i, f := range x.Faults {
		if f.AtS < 60 || f.AtS > x.DurationS {
			return errors.New("soak fault at_s must lie within the load, from 60 s")
		}
		if (f.Role != "volume" && f.Role != "master") || roles[f.Role] {
			return errors.New("soak allows one volume and one master fault")
		}
		roles[f.Role] = true
		if i > 0 && secs(f.AtS-prev) < FaultBudget+RecoveryMargin {
			return errors.New("soak faults must be ordered, from 60 s, one fault budget plus recovery apart")
		}
		prev = f.AtS
	}
	if secs(prev)+FaultBudget+RecoveryMargin+QuiescenceReserve > secs(x.DurationS) {
		return errors.New("soak last fault leaves no recovery and quiescence before load end")
	}
	// The worker's client kill keeps a fault budget plus recovery from every
	// runner fault and from the load end.
	k := x.ClientKillAtS
	if k < 60 || k > x.DurationS || secs(x.DurationS-k) < FaultBudget+RecoveryMargin+QuiescenceReserve {
		return errors.New("soak client kill must lie within the load with recovery before its end")
	}
	for _, f := range x.Faults {
		if d := f.AtS - k; secs(max(d, -d)) < FaultBudget+RecoveryMargin {
			return errors.New("soak client kill too close to a runner fault")
		}
	}
	return nil
}

// bindSoak is the soak's static admission (contract r3 R2, R3, R4 shape).
// used holds every job label bind() already admitted.
func bindSoak(p Plan, d *inventory.Document, in tr.PackInputs, used map[string]bool, now time.Time) error {
	x := p.Soak
	if p.Restart != nil {
		return errors.New("soak and restart are exclusive")
	}
	if p.Profile != "m01-m02-v1" || d.Transport != "dc" || len(p.Workload) != 1 || p.Workload[0].Host != "m01" {
		return errors.New("soak needs the exact local M01 DC profile with one M01 load")
	}
	if in.Pause != nil {
		return errors.New("soak carries no pause authority")
	}
	if x.DurationS <= 0 || x.DurationS > int(SoakHorizon/time.Second) || math.IsNaN(x.PacingS) || math.IsInf(x.PacingS, 0) || x.PacingS < 0.25 || x.PacingS > 10 {
		return errors.New("soak duration/pacing out of range")
	}
	if err := x.checkSchedule(); err != nil { // every number bounded before any arithmetic
		return err
	}
	if err := x.Rotation.validate(); err != nil {
		return err
	}
	// R3 static checks. Work holds deploy, load, quiescence and collect; only
	// cleanup lies between WorkDeadline and HardDeadline.
	b := in.Binding
	if b.HardDeadline.Sub(b.WorkDeadline) < CleanupReserve {
		return errors.New("soak needs the cleanup reserve between work and hard deadlines")
	}
	if DeployReserve+secs(x.DurationS)+QuiescenceReserve+CollectReserve > b.WorkDeadline.Sub(now) {
		return errors.New("soak does not fit the admitted work deadline")
	}
	if b.HardDeadline.Sub(now) > SoakHorizon {
		return errors.New("soak hard deadline beyond the 60 min soak horizon")
	}
	// R2: one fault per role, in order; an injective job map; exact episodes.
	if len(in.Restarts) != len(x.Faults) {
		return errors.New("soak restart episodes must be exactly the scheduled faults")
	}
	labels, master := map[string]bool{}, false
	for _, f := range x.Faults {
		master = master || f.Role == "master"
		ep, ok := in.Restarts[f.Episode]
		if !ok || ep.Victim == ep.Replacement {
			return errors.New("soak fault episode not admitted")
		}
		for _, l := range []string{ep.Victim, ep.Replacement} {
			if labels[l] {
				return errors.New("soak fault labels must be injective")
			}
			labels[l] = true
		}
		victim := false
		for _, s := range p.Deploy {
			victim = victim || (s.Job == ep.Victim && s.Role == f.Role && s.Host == "m02")
		}
		j := in.Jobs[ep.Replacement]
		want, err := ProfileArgs(d, "m02", "deploy", f.Role, p.profileSettings(false), in.Files["m02"])
		if !victim || used[ep.Replacement] || err != nil || !slices.Equal(j.Argv, append(want, "--replacement-"+f.Role)) || j.Node != "m02" || j.Executable != in.Jobs[ep.Victim].Executable || j.SHA256 != in.Jobs[ep.Victim].SHA256 {
			return errors.New("soak replacement is not the exact private helper of its victim")
		}
	}
	if master {
		if err := x.ReadyAssign.validate(); err != nil {
			return err
		}
	} else if x.ReadyAssign != nil {
		return errors.New("soak ready_assign only with a master fault")
	}
	// Step 3: one collection across setup, rotation and ready_assign, and the
	// M02 setup job, exact and injective.
	if err := x.Layout.validate(); err != nil || x.GrowCount < 1 || x.GrowCount > 16 || x.Rotation.Collections != x.Layout.Collection || (x.ReadyAssign != nil && *x.ReadyAssign != x.Layout) {
		return errors.New("soak layout, grow count or collection coherence")
	}
	if s := x.Setup; s.Host != "m02" || s.Role != "" || used[s.Job] || labels[s.Job] {
		return errors.New("soak setup step not exact")
	}
	labels[x.Setup.Job] = true
	if err := checkProfile(in.Jobs[x.Setup.Job], in, d, "m02", "soak_setup", "", p.setupSettings()); err != nil {
		return err
	}
	// Fault ready probes: one per host, distinct from every other job label,
	// pinned to the profile with the soak fields (Assign probe included).
	hosts := map[string]bool{}
	for _, s := range x.FaultReady {
		j, ok := in.Jobs[s.Job]
		if !ok || j.Node != s.Host || s.Role != "" || used[s.Job] || labels[s.Job] || hosts[s.Host] {
			return errors.New("soak fault ready step not exact")
		}
		hosts[s.Host] = true
		labels[s.Job] = true
		if err := checkProfile(j, in, d, s.Host, "ready", "", p.profileSettings(true)); err != nil {
			return err
		}
	}
	if !hosts["m01"] || !hosts["m02"] || len(hosts) != 2 {
		return errors.New("soak fault ready must cover M01 and M02")
	}
	return nil
}
