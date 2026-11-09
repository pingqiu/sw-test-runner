package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Both snapshots are from the task host and bind the SAME live task/copy.
// The second snapshot begins only after the existing kill reported GONE.
func maintenanceReplicaFlight(task string, want maintenanceSubject, result maintenanceTaskResult, f *maintenanceFaultSample) (maintenanceFlight, maintenanceFault, bool) {
	a, b := f.Before, f.After
	fault := maintenanceFault{Subject: f.Subject, Confirmed: f.Confirmed, Missing: f.Missing}
	witness := strings.Fields(f.GoneWitness)
	if !f.BeforePrecedesDispatch || !f.AfterFollowsGone || !f.Confirmed || len(witness) != 4 || witness[0] != "OLD" || witness[3] != "GONE" || f.TargetUnit == "" {
		return maintenanceFlight{}, fault, false
	}
	unit := "sw-cloud-" + result.Identity.RunID + "-" + f.Victim + "-" + f.Role
	if f.TargetUnit != unit && f.TargetUnit != unit+"-g2" && f.TargetUnit != unit+"-g3" && f.TargetUnit != unit+"-g4" {
		return maintenanceFlight{}, fault, false
	}
	if pid, e := strconv.ParseUint(witness[1], 10, 64); e != nil || pid == 0 {
		return maintenanceFlight{}, fault, false
	}
	if start, e := strconv.ParseUint(witness[2], 10, 64); e != nil || start == 0 {
		return maintenanceFlight{}, fault, false
	}
	if a.Identity == nil || b.Identity == nil || !a.Alive || !b.Alive || *a.Identity != *b.Identity || *a.Identity != result.Identity || a.Boot != b.Boot || a.Boot != result.Identity.Boot || a.Progress == nil || b.Progress == nil {
		return maintenanceFlight{}, fault, false
	}
	p, q := a.Progress, b.Progress
	if p.Begin < result.Started || a.After > result.Ended || b.Before > result.Ended {
		return maintenanceFlight{}, fault, false
	}
	if p.Source == "" || p.Target == "" || p.Source != q.Source || p.Target != q.Target {
		return maintenanceFlight{}, fault, false
	}
	if f.Role == "volume" {
		if f.TargetEndpoint != p.Source && f.TargetEndpoint != p.Target {
			return maintenanceFlight{}, fault, false
		}
	} else if f.Role != "master" && f.Role != "filer" {
		return maintenanceFlight{}, fault, false
	}
	if p.State != "in_flight" || q.State != "in_flight" || p.VID != want.VID || q.VID != want.VID || p.Operation == "" || p.Operation != q.Operation || p.Bytes <= 0 || q.Bytes <= p.Bytes || p.Begin != q.Begin || p.Begin > a.Before || a.After > b.Before {
		return maintenanceFlight{}, fault, false
	}
	fault.Owner, fault.Boot = result.Identity.owner(), a.Boot
	fault.Before = maintenanceBound{a.Before, a.After}
	fault.Gone = maintenanceBound{a.After, b.Before}
	flight := maintenanceFlight{Subject: want, Job: result.Job, Attempt: result.Attempt, Owner: fault.Owner, Boot: a.Boot, Operation: p.Operation, Begin: maintenanceBound{p.Begin, p.Begin}, End: maintenanceBound{b.Before, b.After}, Bytes: p.Bytes, State: "in_flight"}
	return flight, fault, true
}

func maintenanceReplicaPost(before, after map[string]maintenanceScope, task string) (known, owners, moved bool, pending int) {
	old, newOwners := []string{}, []string{}
	known = true
	for id, s := range before {
		if s.Ordinary && s.Mounted {
			old = append(old, id)
		}
	}
	for id, s := range after {
		pending += s.Pending
		if s.Ordinary {
			if !s.Mounted || s.ReadOnly {
				known = false
			}
			newOwners = append(newOwners, id)
		} else if s.Mounted {
			known = false
		}
	}
	// A partial/missing host response must not look like an empty directory.
	if len(before) != 6 || len(after) != 6 {
		known = false
	}
	sort.Strings(old)
	sort.Strings(newOwners)
	owners = len(newOwners) == 2
	moved = !reflect.DeepEqual(old, newOwners)
	return
}

func (m *maintenanceRuntime) collect(ctx context.Context, checkLock bool) (maintenanceVerdict, error) {
	e := maintenanceEvidence{Task: m.c.MaintenanceTask, Expected: m.subject}
	for _, o := range m.manifest.Objects {
		e.ExpectedData = append(e.ExpectedData, maintenanceData{Key: o.Key, ExpectedSHA256: o.SHA, ExpectedLength: o.Length, Deleted: o.Deleted})
	}
	readErr := m.leaf(ctx, "meta", "readback", 90, nil, &e.Data)
	var waitErr error
	if m.done != nil {
		select {
		case <-m.done:
			_, waitErr = m.taskSnapshot()
		case <-ctx.Done():
			waitErr = ctx.Err()
		}
	} else {
		waitErr = errors.New("maintenance task was not dispatched")
	}
	result, _ := m.taskSnapshot()
	e.Job, e.Attempt, e.Owner, e.Boot = result.Job, result.Attempt, result.Identity.owner(), result.Identity.Boot
	e.Terminal, e.Refusal = result.Terminal, result.Refusal
	if waitErr != nil {
		e.Terminal = "unknown"
		e.Refusal = ""
	}
	if result.Terminal != "refused" {
		e.Refusal = ""
	}
	after, postErr := m.scopes(ctx, true)
	unchangedRefusal := e.Terminal == "refused" && reflect.DeepEqual(m.beforeTask, after)
	if postErr == nil {
		postErr = m.verifyCollection(ctx, after, unchangedRefusal)
	}
	var lockErr error
	dataFailed := assessMaintenance(e).Data == "FAIL"
	if dataFailed { // qualification stops; only preservation/cleanup follows
	} else if checkLock && readErr == nil && postErr == nil && waitErr == nil && (e.Terminal == "succeeded" || e.Terminal == "refused") {
		var lock struct {
			Usable     bool   `json:"lock_usable"`
			VID        string `json:"vid"`
			Collection string `json:"collection"`
			Waited     bool   `json:"waited"`
		}
		lockErr = m.leaf(ctx, "meta", "lock_check", 20, nil, &lock)
		if lockErr == nil && (!lock.Usable || lock.VID != m.manifest.VID || lock.Collection != m.manifest.Collection) {
			lockErr = errors.New("post-task lock proof is not bound to the fixture")
		}
	} else {
		lockErr = errors.New("task did not settle for a bounded post-lock check")
	}
	moved := true
	if postErr == nil {
		known, owners, changed, pending := maintenanceReplicaPost(m.beforeTask, after, m.c.MaintenanceTask)
		moved = changed
		e.EffectsKnown, e.PostOwners, e.GenerationConverged, e.Pending = known, owners, true, pending
		if unchangedRefusal && known && pending == 0 {
			e.PostOwners = true
		}
	}
	if lockErr != nil {
		e.EffectsKnown = false
	}
	for _, f := range m.faults {
		flight, fault, valid := maintenanceReplicaFlight(m.c.MaintenanceTask, m.subject, result, f)
		e.Faults = append(e.Faults, fault)
		if valid {
			e.Flights = append(e.Flights, flight)
		}
	}
	if m.c.MaintenanceTask == "volume.balance" && !moved {
		e.Flights = nil
	}
	verdict := assessMaintenance(e)
	m.retain = verdict.Task == "UNKNOWN" || !e.EffectsKnown || e.Pending != 0
	saveErr := errors.Join(m.r.save("maintenance-evidence.json", e), m.r.save("maintenance-post-owners.json", after), m.r.save("maintenance-verdict.json", verdict), m.r.save("maintenance-faults.json", m.faults))
	if verdict.Data == "FAIL" {
		return verdict, errors.Join(fmt.Errorf("maintenance exact-data oracle FAIL"), readErr, waitErr, postErr, saveErr)
	}
	if readErr != nil || waitErr != nil || postErr != nil || saveErr != nil || lockErr != nil {
		return verdict, errors.Join(readErr, waitErr, postErr, saveErr, lockErr)
	}
	if verdict.Data != "PASS" || verdict.Task == "UNKNOWN" || verdict.Task == "FAIL" {
		return verdict, fmt.Errorf("maintenance unresolved result: %+v", verdict)
	}
	return verdict, nil
}

// This bounds the task and the unchanged case together. Cancellation does not
// bypass cleanup; its independent reserve is registered before any mutation.
func (m *maintenanceRuntime) concurrent(ctx context.Context, run func(context.Context) error) error {
	phase, cancel, err := m.phase(ctx, 600*time.Second)
	if err != nil {
		return err
	}
	defer cancel()
	m.mu.Lock()
	m.failureCancel = cancel
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.failureCancel = nil; m.mu.Unlock() }()
	first, changed, done, stop, observeErr := m.startObserver(phase)
	if observeErr != nil {
		return observeErr
	}
	defer func() { stop(); <-done }()
	if err = m.startTask(phase); err != nil {
		return err
	}
	m.retain = true // uncertainty is settled only by the independent post-readback
	// One observer-start receipt, not a wait for work or successful coverage.
	select {
	case <-first:
	case <-phase.Done():
		return phase.Err()
	}
	m.injecting = true
	err = run(phase)
	m.injecting = false
	if err != nil {
		cancel()
	}
	select {
	case <-m.done:
		_, taskErr := m.taskSnapshot()
		err = errors.Join(err, taskErr)
	case <-phase.Done():
		err = errors.Join(err, phase.Err())
	}
	err = errors.Join(err, m.finishObserver(phase, changed, done, stop))
	return err
}

func (m *maintenanceRuntime) observeMixed(raw string) {
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	if len(lines) == 0 {
		return
	}
	body := []byte(lines[len(lines)-1])
	if oneStrictValue(body) != nil {
		return
	}
	var row map[string]json.RawMessage
	if json.Unmarshal(body, &row) != nil {
		return
	}
	var status, code, message string
	json.Unmarshal(row["status"], &status)
	json.Unmarshal(row["code"], &code)
	json.Unmarshal(row["error"], &message)
	// This fatal is raised only after the existing worker observed a successful
	// GET with wrong bytes. Generic read failures remain UNKNOWN.
	if status == "FAIL" && code == "CHAOS_LOAD_FAILED" && strings.HasPrefix(message, "AssertionError: WRONG bytes for ") {
		m.mu.Lock()
		m.mixedWrong = true
		m.mu.Unlock()
	}
}
func (m *maintenanceRuntime) loadFailed() {
	m.mu.Lock()
	cancel := m.failureCancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
func (m *maintenanceRuntime) mixedDataFailed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mixedWrong
}
