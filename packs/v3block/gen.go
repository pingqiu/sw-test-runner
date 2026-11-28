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
	"maps"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/randomrun"
)

// GenVersion is recorded in the history's first record; a replay of another
// version is refused. m1-gen/2: the shadow follows the documented refusals
// (reserved and bound names, no attach on an earlier local root or after a
// stop, no ordinary delete of a serving volume), proposes the negatives the
// typed actions can answer, and names every read's lbas. m1-gen/3: a restore
// from a target's snapshot and a release of a target are negatives only while
// the target still names a source chunk. m1-gen/4: an attach or stop
// answered unknown leaves its volume unsettled until a settled record places
// it, and a volume a healthy record lists not serving gets no reads or writes.
//
// m1-gen/5 (host run 4): forced operations (recorded "forced" in gen_step,
// outside the budget): the dirty lbas read back before a stop or move, the
// content read after an attach on a new host, a restore target's inherited
// lbas read first, a pair window before a source or target leaves, the
// lease horizon learnt from observations, and a coverage tail before the
// sweep; a negative is observed on every volume it names.
//
// m1-gen/6: the source-delete negative carries retry_horizon. m1-gen/7
// (host runs 5 and M2-2): the pair window writes across, and a volume whose
// holder the runner killed is offered nothing until it serves again.
// m1-gen/8 (host runs 6): every restore pair gets its window: the target's
// first attach is forced, neither side leaves before the window, a lost
// side is noted in coverage. m1-gen/9: a volume out of the run is observed
// at the end, and read if it still serves.
const GenVersion = "m1-gen/9"

// GenVersionFaults is the generator version recorded in the header when a
// fault schedule is active; a run without faults keeps GenVersion, so an M1
// history replays as before. m2-gen/2: as m1-gen/3. m2-gen/3: no ordinary
// kill of the master; it also learns as m1-gen/4.
// m2-gen/4: as m1-gen/5. m2-gen/5: as m1-gen/6. m2-gen/6: a master outage
// with nothing serving is skipped. m2-gen/7: as m1-gen/7. m2-gen/8: as
// m1-gen/8. m2-gen/9: as m1-gen/9. m2-gen/10: the schedule kills the fault
// processes in turn, the first of each forced (randomrun.NewFaultSchedule),
// so every listed process is reached by construction; with fault_during and
// fault_store, the in-operation faults (randomrun inop.go): a holder kill
// timed into a write and into a snapshot, a store pause around a snapshot,
// each with its target operation forced (TargetOp), and the follow-ups of
// the store pause (AfterStorePause).
const GenVersionFaults = "m2-gen/10"

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

// genLimit is a documented limit the generator probes (by its id); a limit
// the table lists as not proposed is never one.
func genLimit(id string) randomrun.Limit {
	l, ok := genTable.Limit(id)
	if !ok || !l.IsProposed() {
		panic("v3block: product/operations.json has no proposed limit " + id)
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
	// unsettled: an attach or stop answered unknown; offered nothing but
	// observation until a "settled" record places it (held: serving again;
	// not_held: stopped).
	stUnsettled = "unsettled"
)

// The limits a negative operation probes: each is a refusal an operator page
// documents; the names and page lines are in product/operations.json (VL
// volume-lifecycle.md, SD standalone-deployment.md, OG operator-guide.md, all
// under docs/, in the comments below).
//
// The documented refusals NOT proposed (every attach refusal, and a restore
// into a deleted name: the typed action cannot answer them refused) are rows
// of the same table with proposed false and why. The shadow still keeps
// Ordinary from proposing those attaches.
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
	// fenced: a healthy record listed it not serving; it is offered no reads
	// or writes until a later healthy record does not.
	fenced bool
	// dirty: lbas written (ok, or unknown in a fault run) since that lba's
	// last ok read. A stop or move would leave them unread for good (no
	// attach after a stop): they are read back first (Before).
	dirty map[int]bool
	// moved: moved since its last attach; its next attach (on the new host)
	// is followed by a read of its whole content (due).
	moved bool

	// killed: the run killed its holder (kill ok or unknown); offered
	// nothing but observation (no negative either: they assume a live
	// holder) until its restart answers ok (restarted) and a healthy record
	// shows it serving. Guest I/O to a dead holder holds.
	killed, restarted bool
	// inheritedLBAs: a restore target's content at its restore, read first
	// after its first attach (C2); attached: it has been attached once.
	inheritedLBAs []int
	attached      bool
	// pairDone: the restore pairs (source|target) whose pair window ran.
	pairDone map[string]bool
	// leaseLapsed: the volume's latest observation shows its lease expired
	// ("lease state: expired" in the workspace part); leaseObsAt: the drawn
	// count at its last lease observation (Due observes a stopped volume
	// every leaseObserveEvery drawn operations until it lapses).
	leaseLapsed bool
	leaseObsAt  int
	// obsServing: its latest observation shows it still serving (a granted
	// lease, HEAD neither detached nor deleted).
	obsServing bool
}

// leaseObserveEvery is how many drawn operations apart a stopped volume
// whose lease is not seen expired is observed (Due).
const leaseObserveEvery = 10

// blockModel is the block product's randomrun.Model. Its zero value is a
// Driver (replay needs no state).
type blockModel struct {
	blocks int // lbas 0..blocks-1 are used for writes and reads
	faulty bool
	vols   map[string]*genVol
	order  []string // pool order
	drawn  int      // drawn (not forced) operations learnt
	tail   tailState
	due    []GenOp  // forced operations owed, in order (Due)
	notes  []string // coverage notes (Notes)
	// pairReady: the drawn count when both sides of a pending pair first
	// served.
	pairReady map[string]int
}

// pairDelay is how many drawn operations a pair window waits once both
// sides serve.
const pairDelay = 10

// tailState is where the coverage tail stands (Tail).
type tailState struct {
	stage    int
	observe  []string // volumes to observe next
	deleted  []string // volumes the tail deleted
	stoppedC string   // the stopped candidate observed for its lease
	// The volumes out of the run (unknown) with acknowledged content: each
	// observed, and read when that observation shows it still serving.
	outInit  bool
	outVols  []string
	outCheck string
	outReads []GenOp
}

var _ randomrun.Model = (*blockModel)(nil)

func (m *blockModel) Operations() randomrun.Operations { return genTable }

func (m *blockModel) Begin(pool []string, faults bool) error {
	if m.blocks <= 0 {
		return fmt.Errorf("blocks are required")
	}
	m.faulty, m.vols, m.order, m.pairReady = faults, map[string]*genVol{}, nil, map[string]int{}
	for _, n := range pool {
		m.vols[n] = &genVol{name: n, state: stUnborn, targets: map[string]string{}, lbas: map[int]bool{}, snapLBAs: map[string][]int{},
			snapRewrote: map[string]bool{}, dirty: map[int]bool{}, pairDone: map[string]bool{}}
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
	add := func(o GenOp) {
		// A side of a pending restore pair is not stopped, moved or deleted
		// before its pair window (C3).
		if (o.Action == "v3_op_stop" || o.Action == "v3_op_move" || o.Action == "v3_op_delete") && m.pending(m.vols[o.Volume]) {
			return
		}
		cs = append(cs, o)
	}
	unborn := m.firstName(func(v *genVol) bool { return v.state == stUnborn })
	if unborn != "" {
		add(op("v3_op_create", unborn))
	}
	for _, n := range m.order {
		v := m.vols[n]
		if v.killed {
			continue // its holder was killed: nothing but observation until it serves again
		}
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
			// After a stop the lease horizon refuses a delete first
			// (VL:409-411): it is proposed once an observation shows the
			// lease expired (a reserved volume is past it).
			if len(v.targets) == 0 && (v.state == stReserved || v.leaseLapsed) {
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
			if !v.fenced {
				add(op("v3_op_write", n, "lbas", m.lbas(r), "round", fmt.Sprintf("g%d", next)))
				add(op("v3_op_read", n, "lbas", m.readLBAs(r, v)))
			}
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
		if v.state == stUnknown || v.killed {
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
			// Within the lease horizon the horizon refuses first, not this
			// limit: probed only once an observation shows the lease expired.
			// retry_horizon: the declared delete retries while the answer is
			// the lease-horizon refusal before persistence (a plain refusal,
			// nothing changed) and returns the first other answer, the one
			// judged against this limit. Never on "delete while attached",
			// whose expected refusal is the horizon itself.
			if len(v.targets) > 0 && v.leaseLapsed {
				add(op("v3_op_delete", n, "retry_horizon", "true"), limDeleteSource)
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
	if o.Forced != "" {
		// learnt, whatever its outcome: no longer due
		if i := slices.IndexFunc(m.due, func(d GenOp) bool { return sameOp(d, o) }); i >= 0 {
			m.due = slices.Delete(m.due, i, i+1)
		}
	}
	if o.Forced == "" {
		m.drawn++
	}
	if o.Forced == "lease" && v != nil {
		v.leaseObsAt = m.drawn
	}
	if o.Action == "v3_op_read" && outcome == "ok" && v != nil {
		for _, f := range strings.Fields(o.Params["lbas"]) {
			if b, err := strconv.Atoi(f); err == nil {
				delete(v.dirty, b)
			}
		}
	}
	if outcome == "unknown" && o.Action == "v3_op_read" {
		return // a read changes nothing, whatever its outcome
	}
	if outcome == "unknown" && o.Action == "v3_op_write" && m.faulty && o.Negative == "" {
		for _, f := range strings.Fields(o.Params["lbas"]) {
			if n, err := strconv.Atoi(f); err == nil {
				v.lbas[n] = true
				v.dirty[n] = true
			}
		}
		v.unresolved++
		v.rewrote = true // it may have persisted: the target may no longer name the source
		return
	}
	if outcome == tr.OutcomeRefused || outcome == tr.OutcomeRefusedEffect || outcome == tr.OutcomeUnknown {
		switch o.Negative {
		case limDeleteSource:
			// refused_effect: the documented effect, the reservation committed
			// (OG:193-196). While the outcome map has no page-pinned row for it
			// the answer stays unknown, and unknown applies the same effect.
			if outcome == tr.OutcomeRefusedEffect || outcome == tr.OutcomeUnknown {
				v.state = stReserved
			}
			return
		case limRestoreTarget:
			t := m.vols[o.Params["target_volume"]]
			t.state, t.source, t.fromSnap = stBound, o.Volume, o.Params["snapshot"]
			return
		}
	}
	if outcome == tr.OutcomeRefusedEffect {
		// A refusal with an effect the shadow does not model: it cannot say
		// what changed, so the volumes leave the run, as for unknown.
		v.state = stUnknown
		if t := o.Params["target_volume"]; t != "" {
			m.vols[t].state = stUnknown
		}
		return
	}
	// An attach or stop answered unknown: its volume's intended state is
	// unsettled until a settled record places it (Observed).
	if outcome == "unknown" && o.Negative == "" && (o.Action == "v3_op_attach" || o.Action == "v3_op_stop") {
		v.state = stUnsettled
		return
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
		v.state, v.fenced, v.leaseLapsed = stServing, false, false // a new holder and lease
		v.held[v.host] = true
		if v.source != "" && !v.attached && len(v.inheritedLBAs) > 0 {
			// C2: a restore target's first reads are its inherited lbas,
			// before anything writes them.
			m.due = append(m.due, m.readsOf(v, v.inheritedLBAs, "inherited")...)
		}
		v.attached = true
		if v.moved {
			// The move's own data check: every acknowledged lba read once on
			// the new host.
			v.moved = false
			m.due = append(m.due, m.readsOf(v, sortedLBAs(v.lbas), "attach-check")...)
		}
	case "v3_op_write":
		for _, f := range strings.Fields(o.Params["lbas"]) {
			if b, err := strconv.Atoi(f); err == nil {
				v.lbas[b] = true
				v.dirty[b] = true
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
		t.inheritedLBAs = slices.Clone(v.snapLBAs[o.Params["snapshot"]])
		// The target's first attach, forced soon, while the source serves.
		at := op("v3_op_attach", t.name, "host", strconv.Itoa(placementHost))
		at.Forced = "pair-attach"
		m.due = append(m.due, at)
		v.targets[o.Params["snapshot"]] = t.name
	case "v3_op_stop":
		v.state, v.leaseLapsed, v.leaseObsAt = stStopped, false, m.drawn
	case "v3_op_move":
		v.state, v.host, v.moved = stMoved, 1-v.host, true
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

// sweepReadSize is the most lbas one sweep read names (a read's size).
const sweepReadSize = 4

// readsOf is the reads of lbas on v, at most a read's size each, forced for
// reason.
func (m *blockModel) readsOf(v *genVol, lbas []int, reason string) []GenOp {
	var out []GenOp
	for i := 0; i < len(lbas); i += sweepReadSize {
		var f []string
		for _, b := range lbas[i:min(i+sweepReadSize, len(lbas))] {
			f = append(f, strconv.Itoa(b))
		}
		o := op("v3_op_read", v.name, "lbas", strings.Join(f, " "))
		o.Forced = reason
		out = append(out, o)
	}
	return out
}

// pairWrite is the pair window's forced write of one lba on v.
func (m *blockModel) pairWrite(v *genVol, lba int, round string) GenOp {
	o := op("v3_op_write", v.name, "lbas", strconv.Itoa(lba), "round", "p-"+strings.ReplaceAll(round, "|", "-"))
	o.Forced = "pair"
	return o
}

// sameOp says two proposals are the same operation (number aside).
func sameOp(a, b GenOp) bool {
	return a.Action == b.Action && a.Volume == b.Volume && a.Forced == b.Forced && maps.Equal(a.Params, b.Params)
}

// Before an ordinary stop or move (not on a fenced or killed volume: it
// cannot be read), C1: the volume's dirty lbas are read back.
func (m *blockModel) Before(o GenOp) []GenOp {
	v := m.vols[o.Volume]
	if o.Negative != "" || v == nil || v.fenced || v.killed || (o.Action != "v3_op_stop" && o.Action != "v3_op_move") {
		return nil
	}
	return m.readsOf(v, sortedLBAs(v.dirty), "readback")
}

// A restore pair (source, target) is pending until its pair window ran or a
// side was lost. While it is pending neither side is offered an ordinary
// stop, move or delete (negatives are), the target's first attach is forced
// right after the restore (on its placement host, while the source still
// serves), and the window is queued as soon as both serve.

// pairKey names a pair.
func pairKey(src, tgt *genVol) string { return src.name + "|" + tgt.name }

// pairs lists the live restore pairs, in a stable order.
func (m *blockModel) pairs() [][2]*genVol {
	var out [][2]*genVol
	for _, n := range m.order {
		v := m.vols[n]
		for _, s := range slices.Sorted(maps.Keys(v.targets)) {
			out = append(out, [2]*genVol{v, m.vols[v.targets[s]]})
		}
	}
	return out
}

// pairLost says why a pair can no longer get its window ("" while it can):
// a side out of the run, unsettled, whose holder was killed, or otherwise
// not in the state the window needs (the source serving, the target created
// or serving).
func pairLost(src, tgt *genVol) string {
	for _, v := range []*genVol{src, tgt} {
		switch {
		case v.killed:
			return v.name + ": holder killed"
		case v.state == stUnknown:
			return v.name + ": out of the run (an unknown outcome)"
		case v.state == stUnsettled:
			return v.name + ": unsettled"
		}
	}
	if src.state != stServing {
		return src.name + ": " + src.state
	}
	if tgt.state != stCreated && tgt.state != stServing {
		return tgt.name + ": " + tgt.state
	}
	return ""
}

// pending says v is a side of a pair whose window has not run.
func (m *blockModel) pending(v *genVol) bool {
	for _, t := range v.targets {
		if !v.pairDone[pairKey(v, m.vols[t])] {
			return true
		}
	}
	if v.source != "" {
		if src := m.vols[v.source]; src != nil && src.targets[v.fromSnap] == v.name && !src.pairDone[pairKey(src, v)] {
			return true
		}
	}
	return false
}

// pairCheck settles the pending pairs: one with a lost side is dropped and
// noted ("pair not exercised: <reason>"); one whose sides both serve gets
// its window queued.
func (m *blockModel) pairCheck() { m.settlePairs(false) }

// settlePairs is pairCheck; now queues the window of a pair whose sides
// both serve without waiting pairDelay (the run's end).
func (m *blockModel) settlePairs(now bool) {
	for _, p := range m.pairs() {
		src, tgt := p[0], p[1]
		key := pairKey(src, tgt)
		if src.pairDone[key] {
			continue
		}
		if why := pairLost(src, tgt); why != "" {
			src.pairDone[key] = true
			m.notes = append(m.notes, "pair not exercised: "+key+": "+why)
			continue
		}
		if src.state == stServing && tgt.state == stServing && !src.fenced && !tgt.fenced {
			// The window waits pairDelay drawn operations once both serve (the
			// hold keeps both): the unwritten target is what "release a live
			// restore target" and a restore from the target's snapshot probe,
			// and the window writes it.
			ready, seen := m.pairReady[key]
			if !seen {
				m.pairReady[key], ready = m.drawn, m.drawn
			}
			if now || m.drawn-ready >= pairDelay {
				src.pairDone[key] = true
				m.due = append(m.due, m.window(src, tgt)...)
			}
		}
	}
}

// window is a pair's window (C3): in each direction a write on one side
// after the restore to a block the other side also holds, then a read of it
// on the other side (which must still show its own value): an inherited lba
// written on the source and read on the target, another (the same, if it
// inherited one) written on the target and read on the source (a target
// that inherited nothing uses a block both hold, and with none the target
// first writes one so that it holds it); then the same lbas (the union of
// both contents) read on both.
func (m *blockModel) window(src, tgt *genVol) []GenOp {
	var out []GenOp
	key := pairKey(src, tgt)
	in := tgt.inheritedLBAs
	if len(in) == 0 {
		for _, b := range sortedLBAs(src.lbas) {
			if tgt.lbas[b] {
				in = append(in, b)
			}
		}
	}
	u := maps.Clone(src.lbas)
	maps.Copy(u, tgt.lbas)
	if len(in) == 0 {
		// No block in common: the target writes one first, so it holds it.
		x := 0
		if s := sortedLBAs(src.lbas); len(s) > 0 {
			x = s[0]
		}
		out = append(out, m.pairWrite(tgt, x, key+"-0"))
		in = []int{x}
	}
	{
		a, b := in[0], in[len(in)-1]
		out = append(out, m.pairWrite(src, a, key+"-1"))
		out = append(out, m.readsOf(tgt, []int{a}, "pair")...)
		out = append(out, m.pairWrite(tgt, b, key+"-2"))
		out = append(out, m.readsOf(src, []int{b}, "pair")...)
		u[a], u[b] = true, true
	}
	lbas := sortedLBAs(u)
	out = append(out, m.readsOf(src, lbas, "pair")...)
	return append(out, m.readsOf(tgt, lbas, "pair")...)
}

// dueState says whether an owed operation runs now ("go"), waits for its
// volume to come back ("wait": fenced, killed, unsettled), or is dropped
// ("drop": its volume left for good, or a forced attach whose pair no
// longer allows it).
func (m *blockModel) dueState(o GenOp) string {
	v := m.vols[o.Volume]
	if o.Action == "v3_op_attach" {
		src := m.vols[v.source]
		if v.state == stCreated && !v.held[v.host] && src != nil && src.state == stServing && !src.killed {
			return "go"
		}
		return "drop"
	}
	switch {
	case v.state == stUnsettled, v.state == stServing && (v.fenced || v.killed):
		return "wait"
	case v.state == stServing:
		return "go"
	}
	return "drop"
}

// Due is the first owed operation that can run now (the pairs settled
// first): owed operations wait while their volume is fenced, killed or
// unsettled, and are dropped when it left for good. Then the lease
// observations.
func (m *blockModel) Due() []GenOp {
	m.pairCheck()
	for i := 0; i < len(m.due); i++ {
		switch m.dueState(m.due[i]) {
		case "go":
			return []GenOp{m.due[i]}
		case "drop":
			m.due = slices.Delete(m.due, i, i+1)
			i--
		}
	}
	// The lease horizon is learnt from observations, never from the clock: a
	// stopped volume whose lease is not seen expired is observed every
	// leaseObserveEvery drawn operations.
	for _, n := range m.order {
		v := m.vols[n]
		if v.state == stStopped && !v.leaseLapsed && m.drawn-v.leaseObsAt >= leaseObserveEvery {
			o := m.Observe(n)
			o.Forced = "lease"
			return []GenOp{o}
		}
	}
	return nil
}

// Notes are the model's coverage notes: the pairs not exercised, and why.
func (m *blockModel) Notes() []string { return slices.Clone(m.notes) }

// Sweep is the end-of-run sweep (C1, C2/C3, C7 need it): for every serving
// volume, reads of every lba with acknowledged content (written, or inherited
// by a restore target) in batches of a read's size, then one observation; one
// observation of every other volume that exists or was deleted (created,
// stopped, moved, reserved, deleted). An unborn or bound name has nothing to
// see, and a volume whose state is unknown is out of the run.
func (m *blockModel) Sweep() []GenOp {
	var out []GenOp
	for _, n := range m.order {
		v := m.vols[n]
		switch v.state {
		case stServing:
			if v.fenced || v.killed {
				out = append(out, m.Observe(n))
				continue
			}
			have := sortedLBAs(v.lbas)
			for i := 0; i < len(have); i += sweepReadSize {
				var f []string
				for _, b := range have[i:min(i+sweepReadSize, len(have))] {
					f = append(f, strconv.Itoa(b))
				}
				out = append(out, op("v3_op_read", n, "lbas", strings.Join(f, " ")))
			}
			out = append(out, m.Observe(n))
		case stCreated, stStopped, stMoved, stReserved, stDeleted, stUnsettled, stUnknown:
			// A volume out of the run is observed too (always safe): it lets
			// C6 place its unknown.
			out = append(out, m.Observe(n))
		}
	}
	return out
}

// Observed learns from observe records (not from the invokes):
//   - observe (a volume's observation): its workspace part's lease state;
//     "lease state: expired" lets a stopped volume be deleted, and the
//     source negative be probed, past the lease horizon;
//   - settled {volume, state, evidence}: an unsettled volume placed by
//     observation; held means serving again (ordinary operations offered),
//     not_held means stopped. A volume that is not unsettled is not moved;
//   - healthy (any look): a serving volume in result.not_serving is fenced
//     (no reads or writes) until a later healthy record leaves it out;
//     result.third_state [{volume, why}] takes the volume out of the run.
func (m *blockModel) Observed(r tr.HistoryRecord) {
	switch r.Verb {
	case "observe":
		// The workspace part of a volume's observation: its lease state.
		if v := m.vols[r.Volume]; v != nil {
			ws, _ := r.Result["workspace"].(map[string]any)
			text, _ := ws["text"].([]any)
			lapsed, granted := false, false
			for _, l := range text {
				lapsed = lapsed || l == "lease state: expired"
				granted = granted || l == "lease state: granted"
			}
			v.leaseLapsed = lapsed
			// Still serving, as far as an observation tells: a granted lease,
			// and a HEAD neither detached nor deleted.
			head, _ := r.Result["head"].(map[string]any)
			hs, _ := head["state"].(string)
			v.obsServing = granted && hs != "detached" && hs != "deleted"
		}
	case "settled":
		vol, _ := r.Result["volume"].(string)
		v := m.vols[vol]
		if v == nil || v.state != stUnsettled {
			return
		}
		switch r.Result["state"] {
		case "held":
			v.state, v.fenced = stServing, false
			v.held[v.host] = true
			if v.source != "" && !v.attached && len(v.inheritedLBAs) > 0 {
				// C2: an attach settled held is its first attach.
				m.due = append(m.due, m.readsOf(v, v.inheritedLBAs, "inherited")...)
			}
			v.attached = true
		case "not_held":
			v.state = stStopped
		}
	case VerbHealthyRecord:
		ns, ok := r.Result["not_serving"].([]any)
		if ok {
			out := map[string]bool{}
			for _, x := range ns {
				if s, ok := x.(string); ok {
					out[s] = true
				}
			}
			for _, v := range m.vols {
				v.fenced = out[v.name] && v.state == stServing
			}
			// A restarted holder serves again once a healthy record shows it:
			// healthy, or not listed not serving and named among the holders.
			holders, _ := r.Result["holders"].(map[string]any)
			for _, v := range m.vols {
				h, _ := holders[v.name].(map[string]any)
				if v.killed && v.restarted && (r.Result["healthy"] == true || (!out[v.name] && h["pid"] != nil)) {
					v.killed, v.restarted = false, false
				}
			}
		}
		third, _ := r.Result["third_state"].([]any)
		for _, x := range third {
			e, _ := x.(map[string]any)
			vol, _ := e["volume"].(string)
			if v := m.vols[vol]; v != nil {
				v.state = stUnknown
			}
		}
	}
}

// VerbHealthyRecord is the healthy record's verb (randomrun's contract).
const VerbHealthyRecord = randomrun.VerbHealthy

// Tail is the coverage tail (C7), deterministic from the state, one
// operation at a time (first what is still owed, the pending pair windows
// included):
//  1. delete one created volume that was never attached (pool order), if any;
//  2. delete one stopped volume without targets whose observation shows its
//     lease expired; when none does yet, observe the first candidate once
//     and delete it if that observation shows it expired;
//  3. observe each volume the tail deleted;
//  4. create a deleted name (the first in pool order): a negative, refused.
func (m *blockModel) Tail() (GenOp, bool) {
	t := &m.tail
	// First the owed operations, with every pending pair window queued now
	// (the budget is spent: no drawn operation will start the delay's end).
	m.settlePairs(true)
	// Out of the run: each volume taken out (unknown, a third state, or never
	// settled) with acknowledged
	// content is observed; if that observation shows it still serving, its
	// content is read (read only, never written), so C1 is not left with
	// unread blocks.
	if !t.outInit {
		t.outInit = true
		for _, n := range m.order {
			if v := m.vols[n]; (v.state == stUnknown || v.state == stUnsettled) && len(v.lbas) > 0 {
				t.outVols = append(t.outVols, n)
			}
		}
	}
	if t.outCheck != "" {
		v := m.vols[t.outCheck]
		t.outCheck = ""
		if v.obsServing {
			t.outReads = m.readsOf(v, sortedLBAs(v.lbas), "tail")
		}
	}
	if len(t.outReads) > 0 {
		o := t.outReads[0]
		t.outReads = t.outReads[1:]
		return o, true
	}
	if len(t.outVols) > 0 {
		n := t.outVols[0]
		t.outVols, t.outCheck = t.outVols[1:], n
		m.vols[n].obsServing = false
		o := m.Observe(n)
		o.Forced = "tail"
		return o, true
	}
	for i := 0; i < len(m.due); i++ {
		switch m.dueState(m.due[i]) {
		case "go":
			o := m.due[i]
			m.due = slices.Delete(m.due, i, i+1)
			o.Forced = "tail"
			return o, true
		case "drop":
			m.due = slices.Delete(m.due, i, i+1)
			i--
		}
	}
	for {
		switch t.stage {
		case 0:
			t.stage = 1
			if n := m.firstName(func(v *genVol) bool {
				return v.state == stCreated && !v.held[0] && !v.held[1] && len(v.targets) == 0 && v.source == ""
			}); n != "" {
				t.deleted = append(t.deleted, n)
				return op("v3_op_delete", n), true
			}
		case 1:
			stopped := func(v *genVol) bool { return v.state == stStopped && len(v.targets) == 0 }
			if n := m.firstName(func(v *genVol) bool { return stopped(v) && v.leaseLapsed }); n != "" {
				t.stage = 3
				t.deleted = append(t.deleted, n)
				return op("v3_op_delete", n), true
			}
			t.stage = 3
			if n := m.firstName(stopped); n != "" {
				t.stage, t.stoppedC = 2, n
				return m.Observe(n), true
			}
		case 2:
			t.stage = 3
			if v := m.vols[t.stoppedC]; v.state == stStopped && v.leaseLapsed {
				t.deleted = append(t.deleted, v.name)
				return op("v3_op_delete", v.name), true
			}
		case 3:
			t.stage, t.observe = 4, nil
			for _, n := range t.deleted {
				if m.vols[n].state == stDeleted {
					t.observe = append(t.observe, n)
				}
			}
		case 4:
			if len(t.observe) > 0 {
				n := t.observe[0]
				t.observe = t.observe[1:]
				return m.Observe(n), true
			}
			t.stage = 5
			if n := m.firstName(func(v *genVol) bool { return v.state == stDeleted }); n != "" {
				o := op("v3_op_create", n)
				o.Params["expect"], o.Negative = "refused", limCreateDeleted
				return o, true
			}
		default:
			return GenOp{}, false
		}
	}
}

// Driver side: how a block proposal is executed.

func (m *blockModel) Version(faults bool) string {
	if faults {
		return GenVersionFaults
	}
	return GenVersion
}

// ObserveSubjects: a restore or a release names its source and its target
// (the typed restore records the target as its subject); every other
// operation its own volume.
func (m *blockModel) ObserveSubjects(o GenOp) []string {
	if o.Action == "v3_op_restore" || o.Action == "v3_op_release" {
		return []string{o.Volume, o.Params["target_volume"]}
	}
	return []string{o.Volume}
}

var _ randomrun.SubjectNamer = (*blockModel)(nil)

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

// MasterOutage: the master and the short-outage bound, from
// product/checks.json (master_outage, fault_targets).
func (m *blockModel) MasterOutage() (string, int, bool) {
	return genOutage.master, genOutage.maxSeconds, genOutageOK
}

// Look is v3_op_healthy look=single: one look, no wait.
func (m *blockModel) Look() GenOp {
	return GenOp{Action: "v3_op_healthy", Params: map[string]string{"look": "single"}}
}

// OutageRestart is v3_op_restart of the master, ready within the outage bound.
func (m *blockModel) OutageRestart() GenOp {
	return GenOp{Action: "v3_op_restart", Params: map[string]string{"process": genOutage.master, "ready_seconds": strconv.Itoa(genOutage.maxSeconds)}}
}

// FaultRan learns a holder kill or restart that ran ("blockvolume:<k>", the
// k-th spec volume): a kill answered ok or unknown leaves its holder dead;
// its restart answered ok waits for a healthy record showing it serving,
// any other answer takes the volume out (a third state is C6's).
func (m *blockModel) FaultRan(action, process, outcome string) {
	rest, ok := strings.CutPrefix(process, "blockvolume:")
	k, err := strconv.Atoi(rest)
	if !ok || err != nil || k < 0 || k >= len(m.order) {
		return
	}
	v := m.vols[m.order[k]]
	switch {
	case action == FaultKill && (outcome == tr.OutcomeOK || outcome == tr.OutcomeUnknown):
		v.killed, v.restarted = true, false
	case action == FaultRestart && v.killed && outcome == tr.OutcomeOK:
		v.restarted = true
	case action == FaultRestart && v.killed:
		v.killed, v.state = false, stUnknown
	}
}

// KillTarget resolves a scheduled holder kill "blockvolume:<i>" at kill
// time: among the volumes that hold now (serving and not fenced, in pool
// order) the (i mod n)-th, named by its place in the spec ("blockvolume:<k>",
// the spec's volumes being the pool in order). None holding: ok false.
// Another process name is its own target.
func (m *blockModel) KillTarget(scheduled string) (string, bool) {
	rest, isHolder := strings.CutPrefix(scheduled, "blockvolume:")
	i, err := strconv.Atoi(rest)
	if !isHolder || err != nil {
		return scheduled, true
	}
	var holding []int
	for k, n := range m.order {
		if v := m.vols[n]; v.state == stServing && !v.fenced && !v.killed {
			holding = append(holding, k)
		}
	}
	if len(holding) == 0 {
		return "", false
	}
	return "blockvolume:" + strconv.Itoa(holding[i%len(holding)]), true
}

// OutageWrites is one write on every serving volume, of one lba (after mod
// blocks: no draw from the run's random stream), in pool order.
func (m *blockModel) OutageWrites(after int) []GenOp {
	var out []GenOp
	for _, n := range m.order {
		if v := m.vols[n]; v.state == stServing && !v.killed {
			out = append(out, op("v3_op_write", n, "lbas", strconv.Itoa(after%m.blocks), "round", fmt.Sprintf("o%d", after)))
		}
	}
	return out
}

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
	MasterOutage   float64  // chance of a short master outage after each operation outside a kill's interval
	// In-operation faults (randomrun inop.go): kills timed into an operation
	// of each InOp.During action, a store pause around one of InOp.StoreAction.
	InOp       randomrun.InOpFaults
	StoreBound int
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
		Faults: cfg.Faults, FaultProcesses: cfg.FaultProcesses, RestartWithin: cfg.RestartWithin, MasterOutage: cfg.MasterOutage,
		InOp: cfg.InOp, StoreBound: cfg.StoreBound}, m)
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
