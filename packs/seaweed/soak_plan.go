package seaweed

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
	// Step 3b: the M02 evidence job and the M01 verdict job, run at collect.
	Evidence Step `json:"evidence"`
	Assess   Step `json:"assess"`
	// Pattern makes the load kv_load's configurable client pattern; nil keeps
	// the soak's own fixed mix, byte for byte.
	Pattern *LoadPattern `json:"pattern,omitempty"`
	// ReadTransport is the soak's RDMA read route for clients 0-2 (client 3
	// stays HTTP): absent is "dc", today's soak byte for byte; "rc" reads over
	// RC push (#999). kv_load keeps its pattern's own transport.
	ReadTransport SoakTransport `json:"read_transport,omitempty"`
}

// SoakTransport decodes only "dc" or "rc": null, "" and anything else refuse,
// so a typo or a null can never fall back to DC silently.
type SoakTransport string

func (t *SoakTransport) UnmarshalJSON(raw []byte) error {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || (s != "dc" && s != "rc") {
		return errors.New(`soak read_transport must be "dc" or "rc"`)
	}
	*t = SoakTransport(s)
	return nil
}

// rc reports a soak that reads over RC push.
func (x *SoakPlan) rc() bool { return x != nil && x.ReadTransport == "rc" }

// LoadPattern is kv_load's client pattern: Clients workers on one transport,
// Keys keys drawn uniformly or by zipf(ZipfS), ObjectBytes each, at most
// RatePerClient operations a second per client, through Phases in order.
type LoadPattern struct {
	Clients       int         `json:"clients"`
	Transport     string      `json:"transport"`
	Keys          int         `json:"keys"`
	Distribution  string      `json:"distribution"`
	ZipfS         float64     `json:"zipf_s,omitempty"`
	ObjectBytes   int         `json:"object_bytes"`
	RatePerClient float64     `json:"rate_per_client"`
	Phases        []LoadPhase `json:"phases"`
	// Capacity knobs, absent (0) for today's values. VolumeNofile is the
	// volumes' soft RLIMIT_NOFILE (their RDMA connection budget is soft/8,
	// clamped 8..256; inherited is 1024, so 128). PushPipes is each client's
	// SW_RDMA_KVCACHE_PUSH_PIPES (kv_smoke.configure sets 8).
	VolumeNofile int `json:"volume_nofile,omitempty"`
	PushPipes    int `json:"push_pipes,omitempty"`
}

// UnmarshalJSON refuses a present null capacity knob: plain decoding would
// turn it into 0, which means absent, and the knob would silently vanish. The
// rest decodes as strictly as the plan itself (unknown fields refuse).
func (p *LoadPattern) UnmarshalJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for k, v := range fields {
		if (strings.EqualFold(k, "volume_nofile") || strings.EqualFold(k, "push_pipes")) && string(bytes.TrimSpace(v)) == "null" {
			return errors.New("kv_load pattern " + strings.ToLower(k) + " must be absent or a number, not null")
		}
	}
	type plain LoadPattern
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode((*plain)(p))
}

// datasetBytes is the most the declared pattern can write onto one volume
// disk: write_once's keys plus every operation of every mixed phase at the
// declared rate counted as a write (an overwrite leaves the old needle's bytes
// until vacuum), all on that disk, with as much again for needle overhead and
// slack. ProfileArgs reaches it before the pattern is checked, so it never
// indexes the phases and saturates instead of overflowing; check() refuses
// such a pattern later.
func (p *LoadPattern) datasetBytes() int64 {
	writes := float64(p.Keys)
	for i, ph := range p.Phases {
		if i > 0 && ph.Kind == "mixed" {
			writes += float64(p.Clients) * p.RatePerClient * float64(ph.DurationS)
		}
	}
	bytes := 2 * math.Ceil(writes) * float64(p.ObjectBytes)
	if math.IsNaN(bytes) || bytes < 0 || bytes > math.MaxInt64/2 {
		return math.MaxInt64 / 2
	}
	return int64(bytes)
}

// LoadPhase: write_once writes every key exactly once, then read reads for
// DurationS, and mixed reads with probability ReadRatio and writes otherwise.
type LoadPhase struct {
	Kind      string  `json:"kind"`
	DurationS int     `json:"duration_s,omitempty"`
	ReadRatio float64 `json:"read_ratio,omitempty"`
}

// writeSeconds is write_once's least duration at the declared rate: each
// write, then its verification read.
func (p *LoadPattern) writeSeconds() float64 {
	return 2 * float64(p.Keys) / (float64(p.Clients) * p.RatePerClient)
}

// check bounds every pattern number, and requires the phases, with write_once
// at its declared rate, to fit inside the load's duration.
func (p *LoadPattern) check(durationS int) error {
	finite := func(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }
	// dc only: the worker's mandatory client kill takes a DC worker.
	if p.Clients < 1 || p.Clients > 64 || p.Transport != "dc" || p.Keys < 1 || p.Keys > 1<<20 ||
		p.ObjectBytes < 1 || p.ObjectBytes > 31<<20 || !finite(p.RatePerClient) || p.RatePerClient <= 0 || p.RatePerClient > 100 {
		return errors.New("kv_load pattern clients, transport, keys, object_bytes or rate out of range")
	}
	if !(p.Distribution == "uniform" && p.ZipfS == 0 || p.Distribution == "zipf" && finite(p.ZipfS) && p.ZipfS > 0 && p.ZipfS <= 3) {
		return errors.New("kv_load pattern distribution must be uniform, or zipf with 0 < zipf_s <= 3")
	}
	if p.VolumeNofile != 0 && (p.VolumeNofile < 1024 || p.VolumeNofile > 1048576) || p.PushPipes < 0 || p.PushPipes > 8 {
		return errors.New("kv_load pattern volume_nofile must be absent or 1024..1048576, push_pipes absent or 1..8")
	}
	if len(p.Phases) < 2 || len(p.Phases) > 4 || p.Phases[0].Kind != "write_once" || p.Phases[0].DurationS != 0 || p.Phases[0].ReadRatio != 0 {
		return errors.New("kv_load pattern starts with one write_once phase and has 2 to 4 phases")
	}
	need := p.writeSeconds()
	for _, ph := range p.Phases[1:] {
		if ph.DurationS < 1 || ph.DurationS > durationS || !(ph.Kind == "read" && ph.ReadRatio == 0 ||
			ph.Kind == "mixed" && finite(ph.ReadRatio) && ph.ReadRatio > 0 && ph.ReadRatio < 1) {
			return errors.New("kv_load pattern phase must be read, or mixed with 0 < read_ratio < 1, for 1 s to the load duration")
		}
		need += float64(ph.DurationS)
	}
	if need > float64(durationS) {
		return errors.New("kv_load pattern phases do not fit in the load duration")
	}
	return nil
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
	SoakHorizon       = tr.SoakHorizon // one value: the worker envelope admits a soak scenario up to it
	KillSettleBound   = 30 * time.Second
	ReplacementEnroll = 15 * time.Second
	ReadyBound        = 45 * time.Second
	AckWait           = 5 * time.Second // the worker's fault-window acknowledgement
	FaultBudget       = AckWait + KillSettleBound + ReplacementEnroll + ReadyBound
	RecoveryMargin    = 60 * time.Second
	QuiescenceReserve = 60 * time.Second
	LoadExitGrace     = 30 * time.Second // after quiescence: the load's terminal row, joins and exit
	CollectReserve    = 120 * time.Second
	CleanupReserve    = 120 * time.Second
	DeployReserve     = 300 * time.Second
	OverdueTolerance  = 5 * time.Second
	WitnessWait       = 10 * time.Second
	// DirectSoakHorizon bounds a soak that seaweed-cloud direct runs (#1002: the 2 h and 8 h gates).
	// The approved (S0b) path and its worker lease envelope keep SoakHorizon.
	DirectSoakHorizon = 9 * time.Hour
)

// directSoakBudget refuses, before anything deploys, a direct soak whose work deadline or
// controller timeout (--timeout) can't hold it: deploy, the load, quiescence and the trailing
// collection and assessment, plus the cleanup reserve for the timeout. Unsized, an 8 h run
// would fail hours in.
func directSoakBudget(x *SoakPlan, now, work, timeout time.Time) error {
	if err := x.checkScheduleWithin(DirectSoakHorizon); err != nil {
		return err
	}
	need := DeployReserve + WitnessWait + secs(x.DurationS) + QuiescenceReserve + LoadExitGrace + trailingReserve(x.DurationS, true)
	if work.Sub(now) < need {
		return fmt.Errorf("soak needs %s of work time; the inventory's work deadline leaves %s", need, work.Sub(now).Round(time.Second))
	}
	if timeout.Sub(now) < need+CleanupReserve {
		return fmt.Errorf("soak needs a direct --timeout of at least %s; it leaves %s", need+CleanupReserve, timeout.Sub(now).Round(time.Second))
	}
	return nil
}

// assessTimeout bounds the direct path's assessor step: 120 s per 30 minutes of soak, or 15 s times the
// square of that count when larger. The per-miss join grows about quadratically with the duration (#1002,
// offline on the real evidence: 2 h 163k misses 91 s, 8 h 760k misses 1552 s), so a linear step leaves
// 8 h at 81% of its 1920 s; the square gives 3840 s there and keeps 30 min and 2 h unchanged.
func assessTimeout(durationS int) time.Duration {
	k := time.Duration(soakScale(durationS))
	return max(120*time.Second*k, 15*time.Second*k*k)
}

// trailingReserve is the work time a soak keeps after its load for collection and assessment.
// The approved path and any 30-minute soak keep CollectReserve, as before. A longer direct soak
// also reserves its whole scaled assessor step (#1002), whose evidence grows with the duration.
// The predeploy budget, the load admission and every fault check use this one value.
func trailingReserve(durationS int, direct bool) time.Duration {
	if direct && soakScale(durationS) > 1 {
		return CollectReserve + assessTimeout(durationS)
	}
	return CollectReserve
}

// soakScale is how many 30-minute soaks the plan's duration spans (at least 1): the factor by which the
// soak's duration-bound evidence and the assessor's step grow (#1002).
func soakScale(durationS int) int {
	return max(1, (durationS+1799)/1800)
}

var (
	soakName        = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	soakReplication = regexp.MustCompile(`^[0-9]{3}$`)
	soakTTL         = regexp.MustCompile(`^([1-9][0-9]{0,4}[mhdwMy])?$`)
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
	s.filerVerbose = p.Restart != nil && p.Restart.Writes
	if p.Soak != nil {
		s.rotation, s.kvLoad = p.Soak.Rotation, p.Soak.Pattern != nil
		if x := p.Soak.Pattern; x != nil {
			s.volumeNofile, s.kvLoadBytes = x.VolumeNofile, x.datasetBytes()
		}
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
func (x *SoakPlan) checkSchedule() error { return x.checkScheduleWithin(SoakHorizon) }

// checkScheduleWithin is checkSchedule under the executor's horizon: SoakHorizon on the approved path,
// DirectSoakHorizon under seaweed-cloud direct.
func (x *SoakPlan) checkScheduleWithin(horizon time.Duration) error {
	if x == nil || x.DurationS <= 0 || x.DurationS > int(horizon/time.Second) {
		return errors.New("soak duration out of range")
	}
	if t := x.ReadTransport; t != "" && t != "dc" && t != "rc" || t == "rc" && x.Pattern != nil {
		return errors.New(`soak read_transport must be "dc" or "rc", and "rc" is the soak's only (not kv_load's)`)
	}
	if x.Pattern != nil {
		if err := x.Pattern.check(x.DurationS); err != nil {
			return err
		}
		if float64(x.ClientKillAtS) < x.Pattern.writeSeconds() { // the kill's GET would come before the barrier
			return errors.New("kv_load pattern client kill must come after write_once")
		}
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
	if DeployReserve+WitnessWait+secs(x.DurationS)+QuiescenceReserve+LoadExitGrace+CollectReserve > b.WorkDeadline.Sub(now) {
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
	// The verdict jobs: M02 evidence (lab_profile soak_collect) and the M01 assessor.
	for _, s := range []Step{x.Evidence, x.Assess} {
		if s.Role != "" || used[s.Job] || labels[s.Job] {
			return errors.New("soak evidence/assess step not exact")
		}
		labels[s.Job] = true
	}
	if x.Evidence.Host != "m02" || x.Assess.Host != "m01" {
		return errors.New("soak evidence runs on M02 and the verdict on M01")
	}
	if err := checkProfile(in.Jobs[x.Evidence.Job], in, d, "m02", "soak_collect", "", p.setupSettings()); err != nil {
		return err
	}
	assess, err := SoakAssessArgs(d, p.Smoke, x)
	if err := checkSoakTask(in.Jobs[x.Assess.Job], in, "m01", assess, err, p.Smoke, soakAssessHelpers, false); err != nil {
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
