package v3block

// The block model of the random run (runner spec S5). The generic machine
// (package randomrun: seed, time-weighted choice, negative share, name pool,
// coverage, fault schedule, run and replay loops) calls this model through
// randomrun.Model: a small lifecycle shadow of the volumes, the precondition
// of each operation on it, the negatives possible in each state, the effect
// of each outcome, and the coverage cells. The model never judges: the
// checkers judge the history alone. Where the shadow and the product
// disagree, C0's "refusal not expected" (or "declared refusal answered ok")
// shows it.
//
// One operation at a time, across several volumes on one host (two holder
// hosts for a move). A NEGATIVE operation is one the operator pages say must
// be refused, issued with expect=refused. Names come from the pool; each is
// used once (a deleted, delete-reserved or restore-bound name is burnt).
//
// The weights, costs and refusal limits (each with its page line) are data:
// product/operations.json.

import (
	_ "embed"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"

	"github.com/pingqiu/sw-test-runner/randomrun"
)

// GenVersion is recorded in the history's first record; a replay of another
// version is refused. m1-gen/2: the shadow follows the documented refusals
// (reserved and bound names, no attach on an earlier local root or after a
// stop, no ordinary delete of a serving volume), proposes the negatives the
// typed actions can answer, and names every read's lbas. m1-gen/3: a restore
// from a target's snapshot and a release of a target are negatives only while
// the target still names a source chunk.
const GenVersion = "m1-gen/3"

// GenVersionFaults is the generator version recorded in the header when a
// fault schedule is active; a run without faults keeps GenVersion, so an M1
// history replays as before. m2-gen/2: as m1-gen/3.
const GenVersionFaults = "m2-gen/2"

//go:embed product/operations.json
var operationsJSON []byte

// genTable is the block's operations table.
var genTable = mustOperations(operationsJSON)

func mustOperations(b []byte) randomrun.Operations {
	t, err := randomrun.ParseOperations(b)
	if err != nil {
		panic("v3block: product/operations.json: " + err.Error())
	}
	return t
}

// genLimit is the name of a documented limit in the table (by its id).
func genLimit(id string) randomrun.Limit {
	l, ok := genTable.Limit(id)
	if !ok {
		panic("v3block: product/operations.json has no limit " + id)
	}
	return l
}

// DefaultWeights are shares of run time per operation (operations.json).
var DefaultWeights = genTable.DefaultWeights()

// OpCost is an operation's rough duration in seconds on one host
// (operations.json).
var OpCost = genTable.Costs()

// MaxSnapshots is the per-volume snapshot limit (the "eleventh snapshot"
// limit's value). No operator page states it: it is a design invariant. An
// eleventh is still a negative operation; what the product answers to it
// (not a typed refusal, and HEAD advances) is a product result, not the
// generator's to settle.
var MaxSnapshots = genLimit("eleventh_snapshot").Value

// Volume lifecycle states of the generator's shadow.
const (
	stUnborn   = "unborn"   // a pool name not yet created
	stCreated  = "created"  // created (or a restore target), no holder serving
	stServing  = "serving"  // attached: a holder serves it
	stStopped  = "stopped"  // its holder was stopped: HEAD is sealed for its epoch
	stMoved    = "moved"    // moved to the other host, not yet attached there
	stDeleted  = "deleted"  // deleted: the name is burnt
	stReserved = "reserved" // a delete was admitted (the name reserved) and then refused: never serves again, the name is burnt
	stBound    = "bound"    // a pool name bound by a refused restore: never created, the name is burnt
	stUnknown  = "unknown"  // an outcome the runner could not decide: no further op on it
)

// The limits a negative operation probes: each is a refusal an operator page
// documents; the names and page lines are in product/operations.json (VL
// volume-lifecycle.md, SD standalone-deployment.md, OG operator-guide.md, all
// under docs/, in the comments below).
//
// Documented refusals NOT proposed, because the typed action cannot answer
// them "refused" and an unknown outcome takes the volume out of the run:
// every attach refusal (blockvolume exits 78 and the runner does not read the
// reason: attach on the old host after a move VL:544, attach a deleted volume
// VL:545, attach on a stale local root VL:155-160, attach with the wrong
// geometry SD:682), attach after a stop (no grant, the attach times out:
// VL:336-337), and a restore into a deleted name (VL:90-91: the restore's
// not_admitted is not a refusal the runner can tell from a persisted one).
// The shadow still keeps ordinary() from proposing those attaches.
var (
	limCreateDeleted   = genLimit("create_deleted").Name            // VL:201-204
	limDeleteAttached  = genLimit("delete_attached").Name           // VL:409-411, VL:527
	limDeleteSource    = genLimit("delete_source").Name             // VL:413-415, SD:653; side effect OG:193-196
	limDeleteSnapDep   = genLimit("delete_snapshot_dependent").Name // SD:692
	limEleventhSnap    = genLimit("eleventh_snapshot").Name         // design page only: docs/design/invariants.md:1092
	limRestoreExisting = genLimit("restore_existing").Name          // VL:88-90, SD:672
	limRestoreTarget   = genLimit("restore_from_target").Name       // SD:688, SD:693 (side effect: the target name stays bound)
	limReleaseLive     = genLimit("release_live").Name              // SD:696
)

// placementHost is the holder host every volume of the run's cluster spec is
// placed on: its first host (the spec's placements name v.Hosts[0]; ops.go,
// volset.go). A move takes a volume to the other one.
const placementHost = 0

// burnt says a name can never be created or restored into again.
func burnt(state string) bool {
	return state == stDeleted || state == stReserved || state == stBound
}

// GenOp is one proposed operation: the typed action (v3_op_*), the volume it
// acts on, its params (volume included) and the limit a negative probes.
type GenOp = randomrun.Op

type genVol struct {
	name     string
	state    string
	host     int               // the holder host it serves (or moved) on
	snaps    []string          // live snapshot names
	targets  map[string]string // snapshot name -> restore target volume (live)
	source   string            // for a restore target (or a bound name): its source volume
	fromSnap string            // for a restore target (or a bound name): the snapshot it was restored from
	lbas     map[int]bool      // lbas with acknowledged content (written here, or inherited by a restore target)
	snapLBAs map[string][]int  // live snapshot name -> the lbas with content when it was taken
	snapSeq  int
	// A restore target's root names the source's chunk until the target
	// rewrites that chunk and the write is committed (a read fill does not
	// re-own it); every lba here lies in one chunk. inherited: the source
	// snapshot had content, so the target's root names a source chunk.
	// rewrote: the target has had a write answered ok (or, in a fault run,
	// unknown) since its restore; snapRewrote: the same, as it stood when
	// each live snapshot was taken. While a root names a source chunk the
	// product refuses a restore from that snapshot and a release of the
	// target (layout/snapshots.go:102-107, layout/delete.go:250-257).
	inherited   bool
	rewrote     bool
	snapRewrote map[string]bool
	// held[h]: a holder on host h served this volume once, so h has a local
	// root for it. The local root is per host and the runner never moves it
	// aside, so an attach on a host that held the volume earlier is against
	// the operator rule (VL:190-192) and, once HEAD has advanced past that
	// root, refused (VL:155-160): it is never proposed.
	held [2]bool
	// unresolved counts writes with an unknown outcome not yet resolved by a
	// read at a healthy point (fault runs only): the volume stays serving and
	// writable, but is not stopped or moved until they are resolved, so the
	// resolving read finds it where the write left it.
	unresolved int
}

// blockModel is the block product's randomrun.Model. Its zero value is a
// Driver (replay needs no state).
type blockModel struct {
	blocks int // lbas 0..blocks-1 are used for writes and reads
	faulty bool
	vols   map[string]*genVol
	order  []string // pool order
}

var _ randomrun.Model = (*blockModel)(nil)

func (m *blockModel) Operations() randomrun.Operations { return genTable }

func (m *blockModel) Begin(pool []string, faults bool) error {
	if m.blocks <= 0 {
		return fmt.Errorf("blocks are required")
	}
	m.faulty, m.vols, m.order = faults, map[string]*genVol{}, nil
	for _, n := range pool {
		m.vols[n] = &genVol{name: n, state: stUnborn, targets: map[string]string{}, lbas: map[int]bool{}, snapLBAs: map[string][]int{},
			snapRewrote: map[string]bool{}}
		m.order = append(m.order, n)
	}
	return nil
}

func (m *blockModel) Header(h map[string]any) { h["blocks"] = m.blocks }

func (m *blockModel) State(vol string) string { return m.vols[vol].state }

// Resolved: the volume's unknown writes were resolved by a read at a healthy point.
func (m *blockModel) Resolved(vol string) {
	if v := m.vols[vol]; v != nil {
		v.unresolved = 0
	}
}

func op(action, vol string, kv ...string) GenOp {
	p := map[string]string{"volume": vol}
	for i := 0; i+1 < len(kv); i += 2 {
		p[kv[i]] = kv[i+1]
	}
	return GenOp{Action: action, Volume: vol, Params: p}
}

// lbas picks 1..4 distinct lbas, space separated (parseLBAs' form).
func (m *blockModel) lbas(r *rand.Rand) string {
	n := 1 + r.IntN(min(4, m.blocks))
	set := map[int]bool{}
	var out []string
	for len(out) < n {
		b := r.IntN(m.blocks)
		if !set[b] {
			set[b] = true
			out = append(out, strconv.Itoa(b))
		}
	}
	return strings.Join(out, " ")
}

// readLBAs picks the lbas a read names, always explicitly (never "written":
// the handler's own written set does not know a restore target's inherited
// content): 1..4 distinct lbas with acknowledged content, or, when there are
// none, one random lba (a block never written reads zero).
func (m *blockModel) readLBAs(r *rand.Rand, v *genVol) string {
	if len(v.lbas) == 0 {
		return strconv.Itoa(r.IntN(m.blocks))
	}
	have := make([]int, 0, len(v.lbas))
	for b := range v.lbas {
		have = append(have, b)
	}
	slices.Sort(have)
	pick := r.Perm(len(have))[:1+r.IntN(min(4, len(have)))]
	slices.Sort(pick)
	out := make([]string, len(pick))
	for i, j := range pick {
		out[i] = strconv.Itoa(have[j])
	}
	return strings.Join(out, " ")
}

// sortedLBAs lists a set of lbas in order.
func sortedLBAs(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for b := range set {
		out = append(out, b)
	}
	slices.Sort(out)
	return out
}

// firstName is the first pool name (in pool order) that ok accepts, or "".
func (m *blockModel) firstName(ok func(v *genVol) bool) string {
	for _, n := range m.order {
		if ok(m.vols[n]) {
			return n
		}
	}
	return ""
}

// Ordinary lists every operation whose precondition holds on the shadow and
// that the operator pages do not say is refused.
func (m *blockModel) Ordinary(r *rand.Rand, next int) []GenOp {
	var cs []GenOp
	add := func(o GenOp) { cs = append(cs, o) }
	unborn := m.firstName(func(v *genVol) bool { return v.state == stUnborn })
	if unborn != "" {
		add(op("v3_op_create", unborn))
	}
	for _, n := range m.order {
		v := m.vols[n]
		switch v.state {
		case stCreated:
			if !v.held[v.host] {
				add(op("v3_op_attach", n, "host", strconv.Itoa(v.host)))
			}
			if len(v.targets) == 0 {
				add(op("v3_op_delete", n))
			}
		case stStopped, stReserved:
			// No attach after a stop: a sealed HEAD stays sealed for its own
			// epoch and is not re-granted (VL:336-337, VL:381); the way back is
			// a clean move to the same host, which v3_op_move cannot issue. A
			// reserved volume never serves again (OG:193-195); its delete is
			// rerun with the same request id once its dependents are gone.
			if len(v.targets) == 0 {
				add(op("v3_op_delete", n))
			}
		case stMoved:
			// Attach only where no earlier local root lies (VL:190-192). A
			// clean move to a host that never started is deleted without a
			// seal (VL:486-494).
			if !v.held[v.host] {
				add(op("v3_op_attach", n, "host", strconv.Itoa(v.host)))
			}
			add(op("v3_op_delete", n))
		case stServing:
			add(op("v3_op_write", n, "lbas", m.lbas(r), "round", fmt.Sprintf("g%d", next)))
			add(op("v3_op_read", n, "lbas", m.readLBAs(r, v)))
			if len(v.snaps) < MaxSnapshots {
				add(op("v3_op_snapshot", n, "name", fmt.Sprintf("%s-s%d", n, v.snapSeq+1)))
			}
			for _, s := range v.snaps {
				if v.targets[s] == "" {
					add(op("v3_op_snapshot_delete", n, "name", s))
					// A snapshot of a restore target names its source's chunks
					// and cannot be restored (SD:688, SD:693).
					if unborn != "" && v.source == "" {
						add(op("v3_op_restore", n, "snapshot", s, "target_volume", unborn))
					}
					break
				}
			}
			if v.unresolved > 0 {
				break // stays where its unknown writes left it until they are resolved
			}
			add(op("v3_op_stop", n))
			if v.source == "" && len(v.targets) == 0 {
				add(op("v3_op_move", n))
			}
		}
	}
	return cs
}

// Negatives lists operations that must be refused now, each tagged with the
// limit it probes; they carry expect=refused. Each is proposed only where no
// other documented refusal would answer first, so the limit names the cause.
func (m *blockModel) Negatives() []GenOp {
	var cs []GenOp
	add := func(o GenOp, limit string) {
		o.Params["expect"] = "refused"
		o.Negative = limit
		cs = append(cs, o)
	}
	unborn := m.firstName(func(v *genVol) bool { return v.state == stUnborn })
	for _, n := range m.order {
		v := m.vols[n]
		if v.state == stUnknown {
			continue
		}
		switch v.state {
		case stDeleted:
			add(op("v3_op_create", n), limCreateDeleted)
		case stServing:
			add(op("v3_op_delete", n), limDeleteAttached)
			if len(v.snaps) >= MaxSnapshots {
				add(op("v3_op_snapshot", n, "name", fmt.Sprintf("%s-s%d", n, v.snapSeq+1)), limEleventhSnap)
			}
			if len(v.snaps) > 0 {
				s := v.snaps[0]
				if v.source != "" {
					// Only a snapshot taken before the target's first write still
					// names a source chunk, and only if it inherited one.
					s = ""
					for _, x := range v.snaps {
						if v.inherited && !v.snapRewrote[x] {
							s = x
							break
						}
					}
				}
				if v.source == "" {
					// Into a name that exists, not this restore's own replay
					// (target_exists).
					if t := m.firstName(func(t *genVol) bool {
						return t.name != n && (t.state == stCreated || t.state == stServing || t.state == stStopped ||
							t.state == stMoved || t.state == stBound) && !(t.source == n && t.fromSnap == s)
					}); t != "" {
						add(op("v3_op_restore", n, "snapshot", s, "target_volume", t), limRestoreExisting)
					}
				} else if unborn != "" && s != "" {
					add(op("v3_op_restore", n, "snapshot", s, "target_volume", unborn), limRestoreTarget)
				}
			}
		case stStopped:
			if len(v.targets) > 0 {
				add(op("v3_op_delete", n), limDeleteSource)
			}
		}
		// Snapshots with a live restore target: their delete is refused, through
		// the holder while it serves, store direct otherwise; a live target is
		// not released while it names a source chunk (it inherited one and has
		// not written since its restore).
		if v.state == stServing || v.state == stStopped || v.state == stReserved {
			for s, t := range v.targets {
				add(op("v3_op_snapshot_delete", n, "name", s), limDeleteSnapDep)
				tv := m.vols[t]
				if ts := tv.state; (ts == stCreated || ts == stServing || ts == stStopped) && tv.inherited && !tv.rewrote {
					add(op("v3_op_release", n, "target_volume", t), limReleaseLive)
				}
			}
		}
	}
	// Candidates came partly from map iteration: order them on a key that
	// identifies each one, so the same seed always picks the same operation.
	key := func(o GenOp) string {
		p := o.Params
		return strings.Join([]string{o.Action, o.Volume, o.Negative, p["name"], p["snapshot"], p["target_volume"]}, "|")
	}
	slices.SortFunc(cs, func(a, b GenOp) int { return strings.Compare(key(a), key(b)) })
	return cs
}

// Learn applies an operation's outcome (ok, refused, unknown) to the shadow.
// A refused ordinary operation changes nothing (C0 reports it as unexpected).
// A refused negative changes nothing either. Two negatives have a documented
// side effect, and the typed action answers them unknown, since its code
// cannot prove nothing persisted; for these two the shadow applies the
// documented effect instead of taking the volume out:
//   - a source delete refused for a snapshot dependent (snapshot_has_dependents,
//     answered unknown) has committed its reservation first: the volume is
//     reserved (OG:193-195). Answered refused (not_admitted before
//     persistence: the lease horizon has not passed) it reserved nothing.
//   - a restore from a snapshot of a restore target (not_admitted, answered
//     unknown or refused) leaves the target name bound (SD:693); the source
//     is unchanged.
//
// Any other unknown outcome, and a negative answered ok, take the volumes out
// of the run: the shadow cannot say what changed.
//
// In a fault run an unknown write is different: a write never changes the
// lifecycle state, so the volume stays serving, its lbas count as having
// content (a read may name them), and the write is counted unresolved until a
// read at a healthy point resolves it.
func (m *blockModel) Learn(o GenOp, outcome string) {
	v := m.vols[o.Volume]
	if outcome == "unknown" && o.Action == "v3_op_read" {
		return // a read changes nothing, whatever its outcome
	}
	if outcome == "unknown" && o.Action == "v3_op_write" && m.faulty && o.Negative == "" {
		for _, f := range strings.Fields(o.Params["lbas"]) {
			if n, err := strconv.Atoi(f); err == nil {
				v.lbas[n] = true
			}
		}
		v.unresolved++
		v.rewrote = true // it may have persisted: the target may no longer name the source
		return
	}
	if outcome == "refused" || outcome == "unknown" {
		switch o.Negative {
		case limDeleteSource:
			if outcome == "unknown" {
				v.state = stReserved
			}
			return
		case limRestoreTarget:
			t := m.vols[o.Params["target_volume"]]
			t.state, t.source, t.fromSnap = stBound, o.Volume, o.Params["snapshot"]
			return
		}
	}
	if outcome == "unknown" || (outcome == "ok" && o.Negative != "") {
		v.state = stUnknown
		if t := o.Params["target_volume"]; t != "" {
			m.vols[t].state = stUnknown
		}
		return
	}
	if o.Negative != "" {
		return
	}
	if outcome != "ok" {
		return
	}
	switch o.Action {
	case "v3_op_create":
		v.state = stCreated
	case "v3_op_attach":
		v.state = stServing
		v.held[v.host] = true
	case "v3_op_write":
		for _, f := range strings.Fields(o.Params["lbas"]) {
			if b, err := strconv.Atoi(f); err == nil {
				v.lbas[b] = true
			}
		}
		v.rewrote = true
	case "v3_op_snapshot":
		v.snapSeq++
		v.snaps = append(v.snaps, o.Params["name"])
		v.snapLBAs[o.Params["name"]] = sortedLBAs(v.lbas)
		v.snapRewrote[o.Params["name"]] = v.rewrote
	case "v3_op_snapshot_delete":
		v.snaps = slices.DeleteFunc(v.snaps, func(s string) bool { return s == o.Params["name"] })
		delete(v.snapLBAs, o.Params["name"])
		delete(v.snapRewrote, o.Params["name"])
	case "v3_op_restore":
		// The target's content is the source's at the snapshot.
		t := m.vols[o.Params["target_volume"]]
		// A restore target serves where its own placement says, as any volume
		// (SD:664-672), not on its source's current host.
		t.state, t.source, t.fromSnap, t.host = stCreated, o.Volume, o.Params["snapshot"], placementHost
		for _, b := range v.snapLBAs[o.Params["snapshot"]] {
			t.lbas[b] = true
		}
		t.inherited, t.rewrote = len(v.snapLBAs[o.Params["snapshot"]]) > 0, false
		v.targets[o.Params["snapshot"]] = t.name
	case "v3_op_stop":
		v.state = stStopped
	case "v3_op_move":
		v.state, v.host = stMoved, 1-v.host
	case "v3_op_delete":
		v.state = stDeleted
		if v.source != "" {
			src := m.vols[v.source]
			for s, t := range src.targets {
				if t == v.name {
					delete(src.targets, s)
				}
			}
		}
	}
}

// Cells are the coverage cells, ordinary and negative alike, by the state of
// the operation's volume (delete|serving is only the negative "delete while
// attached"; delete is ordinary from created, stopped, moved and reserved).
func (m *blockModel) Cells() map[string][]string {
	return map[string][]string{
		"v3_op_create": {stUnborn, stDeleted}, "v3_op_attach": {stCreated, stMoved},
		"v3_op_write": {stServing}, "v3_op_read": {stServing}, "v3_op_snapshot": {stServing},
		"v3_op_snapshot_delete": {stServing, stStopped}, "v3_op_restore": {stServing}, "v3_op_stop": {stServing},
		"v3_op_move": {stServing}, "v3_op_delete": {stCreated, stStopped, stMoved, stReserved, stServing},
		"v3_op_release": {stServing, stStopped},
	}
}

// Driver side: how a block proposal is executed.

func (m *blockModel) Version(faults bool) string {
	if faults {
		return GenVersionFaults
	}
	return GenVersion
}

// Calls issues a write over several lbas one lba per call (the handler
// records one operation per lba either way), so that every lba's outcome
// counts.
func (m *blockModel) Calls(o GenOp) []map[string]string {
	if o.Action != "v3_op_write" {
		return []map[string]string{o.Params}
	}
	var calls []map[string]string
	for _, lba := range strings.Fields(o.Params["lbas"]) {
		p := map[string]string{}
		for k, v := range o.Params {
			p[k] = v
		}
		p["lbas"] = lba
		calls = append(calls, p)
	}
	return calls
}

// Observe is v3_op_observe of one volume (it answers "observed", no outcome).
func (m *blockModel) Observe(vol string) GenOp {
	return GenOp{Action: "v3_op_observe", Volume: vol, Params: map[string]string{"volume": vol}}
}

// Resolution (kb C6): an unknown write is resolved by reading its lbas, any
// other unknown by observing its volume and a restore's target. A read
// changes nothing, so an unknown read has nothing to resolve.
func (m *blockModel) Resolution(o GenOp) (*GenOp, []string) {
	switch o.Action {
	case "v3_op_read":
		return nil, nil
	case "v3_op_write":
		return &GenOp{Action: "v3_op_read", Volume: o.Volume, Params: map[string]string{"volume": o.Volume, "lbas": o.Params["lbas"]}}, nil
	}
	return nil, []string{o.Volume, o.Params["target_volume"]}
}

// Fault is v3_op_kill or v3_op_restart of the process.
func (m *blockModel) Fault(e randomrun.FaultEvent) GenOp {
	return GenOp{Action: "v3_op_" + e.Action, Params: map[string]string{"process": e.Process}}
}

// Healthy is v3_op_healthy (wait: its wait_seconds).
func (m *blockModel) Healthy(wait string) GenOp {
	p := map[string]string{}
	if wait != "" {
		p["wait_seconds"] = wait
	}
	return GenOp{Action: "v3_op_healthy", Params: p}
}

func (m *blockModel) ValidProcess(p string) bool { return ValidFaultProcess(p) }

// GenConfig sets a block run: the generic config and the lbas used.
type GenConfig struct {
	Seed     uint64
	Ops      int            // the most operations to propose
	Pool     []string       // volume names, each used once
	Blocks   int            // lbas 0..Blocks-1 are used for writes and reads
	Negative float64        // share of operations that must be refused (default 0.1)
	Weights  map[string]int // relative share of TIME per operation; default DefaultWeights

	// M2 process faults (gen_faults.go); off when Faults is 0.
	Faults         float64  // chance of a kill after each operation while nothing is down
	FaultProcesses []string // the processes the schedule may kill
	RestartWithin  int      // a kill's restart comes within this many operations (default DefaultRestartWithin)
}

// Generator is the generic generator over the block model; the model's
// shadow stays reachable for the tests.
type Generator struct {
	*randomrun.Generator
	m    *blockModel
	vols map[string]*genVol
}

// NewGenerator validates the config and starts a run.
func NewGenerator(cfg GenConfig) (*Generator, error) {
	if cfg.Blocks <= 0 {
		return nil, fmt.Errorf("generator: ops, a name pool and blocks are required")
	}
	m := &blockModel{blocks: cfg.Blocks}
	g, err := randomrun.New(randomrun.Config{Seed: cfg.Seed, Ops: cfg.Ops, Pool: cfg.Pool, Negative: cfg.Negative, Weights: cfg.Weights,
		Faults: cfg.Faults, FaultProcesses: cfg.FaultProcesses, RestartWithin: cfg.RestartWithin}, m)
	if err != nil {
		return nil, err
	}
	return &Generator{Generator: g, m: m, vols: m.vols}, nil
}

// candidate is one possible next operation with its weight.
type candidate struct {
	op     GenOp
	weight float64
}

func candidates(cs []randomrun.Candidate) []candidate {
	out := make([]candidate, len(cs))
	for i, c := range cs {
		out[i] = candidate{c.Op, c.Weight}
	}
	return out
}

// ordinary and negatives are the candidates the generic generator would
// choose from now (ordinary ones draw from the run's random stream).
func (g *Generator) ordinary() []candidate  { return candidates(g.Candidates(false)) }
func (g *Generator) negatives() []candidate { return candidates(g.Candidates(true)) }
