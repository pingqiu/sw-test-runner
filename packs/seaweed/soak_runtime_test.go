package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

// fakeSoak drives runSoak with a virtual clock: After(d) advances it at once.
// The load "exits" when the clock reaches exitAt; WaitTask then returns.
type fakeSoak struct {
	t        *testing.T
	mu       sync.Mutex
	mono     float64
	wall     time.Time
	deadline time.Time
	start    float64
	noWit    bool
	exitAt   float64 // load exits when mono >= exitAt
	exitCode int
	exited   chan struct{}
	once     sync.Once
	started  int
	restarts []string
	readies  []string
	readyDL  []time.Time
	hooks    map[string]func(context.Context) // keyed "restart:<episode>" or "after:<n>"
	afters   int
	notReady string // ready job label that answers non-READY
	noProbe  bool
	badPID   bool
	cancel   context.CancelFunc
}

const fakeReplacementPID = 4242

func newFakeSoak(t *testing.T) *fakeSoak {
	f := &fakeSoak{t: t, mono: 1000, wall: time.Unix(1_800_000_000, 0), start: 1000.5, exitAt: 1000.5 + 1800, exited: make(chan struct{}), hooks: map[string]func(context.Context){}}
	f.deadline = f.wall.Add(45 * time.Minute)
	return f
}

func (f *fakeSoak) advance(d time.Duration) {
	f.mu.Lock()
	f.mono += d.Seconds()
	f.wall = f.wall.Add(d)
	crossed := f.mono >= f.exitAt
	f.mu.Unlock()
	if crossed {
		f.once.Do(func() { close(f.exited) })
		// Let the observer goroutine publish the completion first: this models
		// "completion observed", which must then win over the due timer.
		time.Sleep(20 * time.Millisecond)
	}
}

func (f *fakeSoak) StartTask(ctx context.Context, label string) (tr.JobHandle, error) {
	f.started++
	return tr.JobHandle{}, nil
}
func (f *fakeSoak) WaitTask(ctx context.Context, h tr.JobHandle) (tr.ExitInfo, error) {
	select {
	case <-f.exited:
		if f.exitCode != 0 {
			return tr.ExitInfo{Code: f.exitCode}, errors.New("load exited non-zero")
		}
		return tr.ExitInfo{Code: 0, Settled: true}, nil
	case <-ctx.Done():
		return tr.ExitInfo{}, ctx.Err()
	}
}
func (f *fakeSoak) Restart(ctx context.Context, episode string, victim tr.JobHandle) (tr.JobHandle, tr.GenerationRestartReceipt, error) {
	f.restarts = append(f.restarts, episode)
	if hook := f.hooks["restart:"+episode]; hook != nil {
		hook(ctx)
	}
	r := tr.GenerationRestartReceipt{State: "OLD_SETTLED", Kill: tr.KillWitness{PreKillMono: f.mono}}
	if err := ctx.Err(); err != nil { // cancelled after Kill: partial receipt, no replacement
		r.Error = err.Error()
		return tr.JobHandle{}, r, err
	}
	r.State = "REPLACED"
	r.Replacement = tr.ProcessIdentity{PID: fakeReplacementPID, StartTime: 7, BootID: "m02-boot", ExecutableSHA256: strings.Repeat("d", 64)}
	f.advance(10 * time.Second)
	return tr.JobHandle{}, r, nil
}
func (f *fakeSoak) RunJob(ctx context.Context, label string) (tr.ExitInfo, error) {
	f.readies = append(f.readies, label)
	if dl, ok := ctx.Deadline(); ok {
		f.readyDL = append(f.readyDL, dl)
	}
	if err := ctx.Err(); err != nil {
		return tr.ExitInfo{}, err
	}
	pid := fakeReplacementPID
	if f.badPID {
		pid = 1
	}
	row := map[string]any{"Status": "READY", "client_mono": f.mono}
	if label == f.notReady {
		row["Status"] = "NOT_READY"
	}
	ident := map[string]any{"scope_identity": tr.ProcessIdentity{PID: pid, StartTime: 7, BootID: "m02-boot", ExecutableSHA256: strings.Repeat("d", 64)}}
	row["Processes"] = map[string]any{"volume": ident, "master": ident}
	if !f.noProbe {
		row["assign_probe"] = map[string]any{"fid": "5,01637037d6", "grew": false}
	}
	raw, _ := json.Marshal(row)
	return captured(raw), nil
}
func (f *fakeSoak) LoadStart(ctx context.Context) (float64, error) {
	if f.noWit {
		<-ctx.Done()
		return 0, errors.New("load-start witness absent")
	}
	return f.start, nil
}
func (f *fakeSoak) Mono() (float64, error)  { f.mu.Lock(); defer f.mu.Unlock(); return f.mono, nil }
func (f *fakeSoak) Now() time.Time          { f.mu.Lock(); defer f.mu.Unlock(); return f.wall }
func (f *fakeSoak) WorkDeadline() time.Time { return f.deadline }
func (f *fakeSoak) After(d time.Duration) <-chan time.Time {
	f.afters++
	if hook := f.hooks["after:"+string(rune('0'+f.afters))]; hook != nil {
		hook(nil)
		return make(chan time.Time) // never fires: only ctx or completion can wake
	}
	f.advance(d)
	c := make(chan time.Time, 1)
	c <- f.wall
	return c
}

func soakTestPlan() (Plan, tr.PackInputs) {
	p := Plan{Smoke: SmokeSettings{HelperDir: "/opt/profile/worker"}, Workload: []Step{{Host: "m01", Role: "client", Job: "load"}}}
	p.Soak = &SoakPlan{DurationS: 1800, PacingS: 2,
		ReadyAssign: &AssignLayout{Collection: "lmcache", Replication: "000", TTL: "2h"},
		FaultReady:  []Step{{Host: "m01", Job: "fault-ready-m01"}, {Host: "m02", Job: "fault-ready-m02"}},
		Faults:      []SoakFault{{AtS: 480, Role: "volume", Episode: "E1"}, {AtS: 900, Role: "master", Episode: "E2"}}}
	in := tr.PackInputs{
		Restarts: map[string]tr.GenerationRestartPlan{"E1": {Victim: "deploy-volume", Replacement: "replacement-volume"}, "E2": {Victim: "deploy-master", Replacement: "replacement-master"}},
		Files:    map[string]map[string]string{"m02": {"/opt/profile/bin/weed": strings.Repeat("d", 64), "/opt/profile/bin/weed-volume": strings.Repeat("d", 64)}},
	}
	return p, in
}

func runFake(t *testing.T, f *fakeSoak) ([]FaultRecord, tr.ExitInfo, error) {
	p, in := soakTestPlan()
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	defer cancel()
	return runSoak(ctx, f, p, in, map[string]tr.JobHandle{})
}

// Positive: a genuinely active load gets exactly the scheduled faults, each
// with an identity-checked READY, and the joined observer returns its exit.
func TestSoakActiveLoadGetsExactlyTheScheduledFaults(t *testing.T) {
	f := newFakeSoak(t)
	records, exit, err := runFake(t, f)
	if err != nil {
		t.Fatalf("soak: %v", err)
	}
	if strings.Join(f.restarts, ",") != "E1,E2" || len(records) != 2 || exit.Code != 0 {
		t.Fatalf("restarts=%v records=%d exit=%+v", f.restarts, len(records), exit)
	}
	if records[0].ReadyMono <= 0 || records[1].AssignProbe == nil || records[0].AssignProbe != nil {
		t.Fatalf("records=%+v", records)
	}
	if records[0].DispatchMono < records[0].DueMono || records[0].DispatchMono > records[0].DueMono+OverdueTolerance.Seconds() {
		t.Fatalf("fault 1 dispatched at %v, due %v", records[0].DispatchMono, records[0].DueMono)
	}
}

func expectNoFurtherFaults(t *testing.T, f *fakeSoak, err error, want string, restarts int) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err=%v, want %q", err, want)
	}
	if len(f.restarts) != restarts {
		t.Fatalf("restarts=%v, want %d", f.restarts, restarts)
	}
}

// The load exits 0 before the first fault, and the loop has not waited on it:
// the observer retained the result, and no Kill is sent. Exit 0 is no PASS.
func TestSoakEarlyExitZeroBeforeFaultSendsNoKill(t *testing.T) {
	f := newFakeSoak(t)
	f.exitAt = f.start + 100
	_, _, err := runFake(t, f)
	expectNoFurtherFaults(t, f, err, "load completed", 0)
}

func TestSoakNonZeroExitBeforeSecondFaultStopsTheSchedule(t *testing.T) {
	f := newFakeSoak(t)
	f.exitAt, f.exitCode = f.start+700, 3
	_, _, err := runFake(t, f)
	expectNoFurtherFaults(t, f, err, "load completed", 1)
}

// Completion at the very instant the second fault falls due: completion wins.
func TestSoakCompletionSimultaneousWithDueFaultWins(t *testing.T) {
	f := newFakeSoak(t)
	f.exitAt = f.start + 900
	_, _, err := runFake(t, f)
	expectNoFurtherFaults(t, f, err, "load completed", 1)
}

// Completion during the recovery margin after the last fault is still early.
func TestSoakCompletionDuringRecoveryIsEarly(t *testing.T) {
	f := newFakeSoak(t)
	f.exitAt = f.start + 960
	_, _, err := runFake(t, f)
	expectNoFurtherFaults(t, f, err, "load completed", 2)
}

// After the last fault the wait for the load is bounded: an overrun fails.
func TestSoakLoadOverrunFails(t *testing.T) {
	f := newFakeSoak(t)
	f.exitAt = f.start + 5000
	_, _, err := runFake(t, f)
	expectNoFurtherFaults(t, f, err, "overran", 2)
}

func TestSoakCancelWhileWaitingSendsNoKill(t *testing.T) {
	f := newFakeSoak(t)
	f.hooks["after:3"] = func(context.Context) { f.cancel() } // waiting for fault 2
	_, _, err := runFake(t, f)
	expectNoFurtherFaults(t, f, err, "context canceled", 1)
}

// Cancel after Kill: the core records the partial receipt and enrolls no
// replacement; the loop keeps that receipt, runs no READY and stops.
func TestSoakCancelAfterKillKeepsPartialReceiptAndStops(t *testing.T) {
	f := newFakeSoak(t)
	f.hooks["restart:E1"] = func(context.Context) { f.cancel() }
	records, _, err := runFake(t, f)
	expectNoFurtherFaults(t, f, err, "context canceled", 1)
	if len(records) != 1 || records[0].Receipt.State != "OLD_SETTLED" || records[0].Receipt.Error == "" || records[0].Error == "" || len(f.readies) != 0 {
		t.Fatalf("records=%+v readies=%v", records, f.readies)
	}
}

func TestSoakMissingLoadStartWitnessSendsNoKill(t *testing.T) {
	f := newFakeSoak(t)
	f.noWit = true
	_, _, err := runFake(t, f)
	expectNoFurtherFaults(t, f, err, "witness", 0)
}

func TestSoakInsufficientWorkTimeRefusesBeforeTheLoad(t *testing.T) {
	f := newFakeSoak(t)
	f.deadline = f.wall.Add(30 * time.Minute)
	_, _, err := runFake(t, f)
	expectNoFurtherFaults(t, f, err, "not enough work time left to admit", 0)
	if f.started != 0 {
		t.Fatal("load started without the work budget")
	}
}

func TestSoakInsufficientWorkTimeBeforeKillSendsNoKill(t *testing.T) {
	f := newFakeSoak(t)
	// Admits the load exactly; fault 1 fits, but fault 2's budget plus the rest
	// of the load and collect does not, so its Kill is never sent.
	f.deadline = f.wall.Add(1800*time.Second + QuiescenceReserve + CollectReserve)
	_, _, err := runFake(t, f)
	expectNoFurtherFaults(t, f, err, "not enough work time left for this fault", 1)
}

// A slow first fault leaves the second overdue: it fails, no catch-up.
func TestSoakOverdueFaultIsNotCaughtUp(t *testing.T) {
	f := newFakeSoak(t)
	f.hooks["restart:E1"] = func(context.Context) { f.advance(500 * time.Second) }
	_, _, err := runFake(t, f)
	expectNoFurtherFaults(t, f, err, "overdue", 1)
}

func TestSoakLateOrForeignReadyStopsTheSchedule(t *testing.T) {
	for name, set := range map[string]func(*fakeSoak){
		"not READY":     func(f *fakeSoak) { f.notReady = "fault-ready-m02" },
		"foreign PID":   func(f *fakeSoak) { f.badPID = true },
		"master no fid": func(f *fakeSoak) { f.noProbe = true },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeSoak(t)
			set(f)
			records, _, err := runFake(t, f)
			// The exact READY receipt refs survive the failure (codex01 R4).
			if last := records[len(records)-1]; len(last.Ready) == 0 || last.Ready[len(last.Ready)-1].StdoutSHA256 == "" {
				t.Fatalf("partial record lost its READY refs: %+v", last)
			}
			restarts := 1
			if name == "master no fid" {
				restarts = 2 // the volume fault needs no Assign probe
			}
			if err == nil || len(f.restarts) != restarts {
				t.Fatalf("err=%v restarts=%v", err, f.restarts)
			}
		})
	}
}

// codex01 R1: an at_s near MaxInt64 must be refused before any arithmetic or
// effect, also when runSoak is called directly (not through bind).
func TestSoakHugeAtSRefusedBeforeAnyEffect(t *testing.T) {
	for _, at := range []int{math.MaxInt64, math.MaxInt64 / 1000, 1801, 59, -1} {
		f := newFakeSoak(t)
		p, in := soakTestPlan()
		p.Soak.Faults[0].AtS = at
		_, _, err := runSoak(context.Background(), f, p, in, map[string]tr.JobHandle{})
		if err == nil || f.started != 0 || len(f.restarts) != 0 {
			t.Fatalf("at_s=%d: err=%v started=%d restarts=%v", at, err, f.started, f.restarts)
		}
	}
}

// codex01 R4: all READY probes of one fault share one 45 s stage deadline.
func TestSoakReadyProbesShareOneReadyBound(t *testing.T) {
	f := newFakeSoak(t)
	before := time.Now()
	if _, _, err := runFake(t, f); err != nil {
		t.Fatal(err)
	}
	if len(f.readyDL) != 4 || !f.readyDL[0].Equal(f.readyDL[1]) || !f.readyDL[2].Equal(f.readyDL[3]) {
		t.Fatalf("READY deadlines not shared per fault: %v", f.readyDL)
	}
	if f.readyDL[0].Sub(before) > ReadyBound+time.Second {
		t.Fatalf("READY stage bound %v, want <= %v", f.readyDL[0].Sub(before), ReadyBound)
	}
}
