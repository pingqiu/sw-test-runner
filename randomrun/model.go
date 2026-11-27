// Package randomrun is the generic random-run machine: a seeded generator
// that proposes one typed operation at a time and learns each outcome, the
// run loop that executes the proposals through a product's typed action
// handlers and records them, the fault schedule, replay of a recorded run,
// and the nightly seeds and verdict.
//
// It knows no product. A product describes itself in two parts: its
// operations table (weights, costs and refusal limits, read from the
// product's description files; see Operations), and a Model in its own
// package: the state the generator keeps, the preconditions (which ordinary
// and which negative operations are possible in a state), the effect of each
// outcome on that state, and the coverage cells it defines. The generator
// never judges: the checkers judge the history alone.
//
// The interface is what the first product needs and nothing more.
package randomrun

import (
	"math/rand/v2"

	tr "github.com/pingqiu/sw-test-runner"
)

// Op is one proposed operation: the typed action and its params.
type Op struct {
	No       int               // operation number in the run, from 1
	Action   string            // the typed action
	Volume   string            // the subject it acts on (the history record's volume)
	Params   map[string]string // the action's params
	Negative string            // the limit a negative operation probes; "" for an ordinary one
	Forced   string            // why the model forced it (Before, Due, the tail); "" for a drawn one
}

// Driver is the product's side of executing a proposal. Replay needs only
// this part.
type Driver interface {
	// Version is the generator version recorded in a run's header; faults
	// says a fault schedule is active. A replay accepts exactly these two.
	Version(faults bool) string
	// Calls splits a proposal into the params of each handler call: one call,
	// unless the product records the operation in parts (each part's outcome
	// counts, and the worst is the operation's).
	Calls(o Op) []map[string]string
	// Observe is the observation of one subject. Its handler records what it
	// saw and answers no outcome.
	Observe(subject string) Op
	// Resolution says how an operation answered unknown is resolved at a
	// healthy point: by running check (resolved only when check answers ok;
	// otherwise it stays pending for the next healthy point), or by observing
	// the named subjects ("" names none). Neither means nothing to resolve.
	Resolution(o Op) (check *Op, observe []string)
	// Fault is the operation that runs one fault event.
	Fault(e FaultEvent) Op
	// Healthy is the operation that asks for a healthy point (wait: the
	// seconds it may poll, "" for its default). Its handler records an
	// observation with verb "healthy" and answers healthy="true" at one (the
	// handler verb contract: doc.go).
	Healthy(wait string) Op
	// ValidProcess says whether a fault schedule may name process p.
	ValidProcess(p string) bool
	// MasterOutage names the product's master process and its short-outage
	// bound in seconds (an outage this short interrupts nothing); ok false
	// when the product states none.
	MasterOutage() (master string, maxSeconds int, ok bool)
	// Look is a single-look health record: one look, no wait; its record
	// says so (result.look "single").
	Look() Op
	// OutageRestart is the master's restart in a short outage, bounded by
	// the outage bound.
	OutageRestart() Op
}

// SubjectNamer is an optional part of a Driver: the subjects an operation
// names, observed around a negative or a refusal (default: its Volume).
// An operation on a pair (a restore into a target) names both, so the
// refusal is shown to have changed neither.
type SubjectNamer interface {
	ObserveSubjects(o Op) []string
}

// Model is the product's side of proposing: its state, preconditions and
// effects, and its coverage cells.
type Model interface {
	Driver
	// Operations is the product's operations table: default weights, costs.
	Operations() Operations
	// Begin starts the state of a run over the name pool (already checked:
	// non-empty, each name a distinct token); faults says a fault schedule is
	// active. It refuses the model's own bad configuration.
	Begin(pool []string, faults bool) error
	// Header adds the model's own configuration to the run's header.
	Header(h map[string]any)
	// State is a subject's state now: a coverage cell is action|state, taken
	// before the operation.
	State(subject string) string
	// Ordinary lists every operation whose precondition holds now and that the
	// product does not refuse, in a stable order. r is the run's random stream
	// (for choices inside an operation, such as which blocks); next is the
	// number the chosen operation will get.
	Ordinary(r *rand.Rand, next int) []Op
	// Negatives lists operations the product must refuse now, each with its
	// Negative limit set and its expectation in its params, in a stable order.
	Negatives() []Op
	// Before lists the operations to run before a drawn operation o (for
	// example, reading back what a stop would make unreadable), each with
	// Forced set; none: o runs at once.
	Before(o Op) []Op
	// Due lists operations the state makes due now, before the next draw
	// (each with Forced set). A due operation, once learnt, whatever its
	// outcome, is no longer due.
	Due() []Op
	// Learn applies an operation's outcome (ok, refused, unknown) to the state.
	Learn(o Op, outcome string)
	// Resolved says the unknown operations of a subject resolved by a check
	// were resolved at a healthy point.
	Resolved(subject string)
	// Cells lists, per action, the states in which it can be proposed: the
	// cells a run reports as never reached.
	Cells() map[string][]string
	// Observed hands the model each observe record the run's handlers wrote
	// (healthy records, the faults' settled records, observations), in
	// order, after the operation or fault event that wrote it: a model learns
	// what was observed from the record, not from the invoke.
	Observed(r tr.HistoryRecord)
	// KillTarget resolves a scheduled kill at kill time: the process that
	// holds for the scheduled name now, deterministically from the state;
	// ok false when none holds (the kill is skipped).
	KillTarget(scheduled string) (process string, ok bool)
	// OutageWrites lists the writes of a short master outage after operation
	// after: one on every serving subject. Learnt like any outcome.
	OutageWrites(after int) []Op
	// Tail is the next operation of the coverage tail, run after the last
	// operation (and a fault run's final healthy point) and before the sweep:
	// deterministic from the state, each learnt before the next is asked
	// for; false when the tail is done.
	Tail() (Op, bool)
	// Sweep lists the operations of the end-of-run sweep, in order: reads of
	// every block with acknowledged content of each live subject, and one
	// observation of each subject the checkers need to see last (deleted ones
	// included). They are recorded as ordinary operations, not proposals.
	Sweep() []Op
}
