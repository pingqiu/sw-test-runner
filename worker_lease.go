package testrunner

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// An in-process ledger writer under the worker's EXISTING flock. It is not a
// broker/holder and cannot keep exclusion after controller death. The accepted
// test-only remote-tail limit still applies; UNKNOWN is never a release.
type workerOwnedLease struct {
	policy     TrustedPolicy
	binding    RunBinding
	authority  string
	acquired   []byte
	stop, done chan struct{}
	once       sync.Once
	err        error // only read after done is joined
	finishErr  error
	released   bool
}

// The worker-owned envelope is ten minutes. Only the registered pack's trusted
// judgement over the captured pins (for Seaweed: its full bind of a valid KV
// soak plan) can extend it, and never past SoakHorizon (claude01's ruling). It
// is decided before any acquire is written; an action label alone proves nothing.
const (
	WorkerEnvelope = 10 * time.Minute
	SoakAction     = "seaweed_kv_soak"
	SoakHorizon    = 60 * time.Minute
)

func workerEnvelope(pre *ApprovedRun, register func(*Registry)) time.Duration {
	if register == nil {
		return WorkerEnvelope
	}
	r := NewRegistry()
	register(r)
	if r.leaseEnvelope == nil {
		return WorkerEnvelope
	}
	d, err := r.leaseEnvelope(pre.declaredPackInputs())
	if err != nil || d <= WorkerEnvelope {
		return WorkerEnvelope
	}
	return min(d, SoakHorizon)
}

func beginWorkerLease(ctx context.Context, request string, p TrustedPolicy, register func(*Registry)) (*workerOwnedLease, context.Context, error) {
	if p.bootstrap.WorkerLease == nil {
		return nil, ctx, nil
	}
	pre := &ApprovedRun{policy: p, requestPath: request, readerChanged: make(chan struct{})}
	if err := pre.readBounded(ctx, 5*time.Second, pre.captureInputs); err != nil {
		return nil, ctx, err
	}
	b := pre.request.Binding
	id, err := hex.DecodeString(b.LeaseID)
	if err != nil || len(id) != 16 || b.LeaseID != strings.ToLower(b.LeaseID) || b.LeaseOwner == "" {
		return nil, ctx, errors.New("invalid explicit worker lease identity")
	}
	lock := p.bootstrap.WorkerLease.LockFile
	if p.bootstrap.LedgerDir != lock+".ledger" || b.HardDeadline.Sub(time.Now()) > workerEnvelope(pre, register) {
		return nil, ctx, errors.New("worker lease lock/ledger or envelope mismatch (ten minutes; a pack-bound soak plan SoakHorizon)")
	}
	if err = verifyWorkerFlock(lock); err != nil {
		return nil, ctx, err
	}
	if err = checkWorkerQuarantine(ctx, lock); err != nil {
		return nil, ctx, err
	}
	dir, err := os.Lstat(p.bootstrap.LedgerDir)
	if err != nil || !dir.IsDir() || dir.Mode()&os.ModeSymlink != 0 {
		return nil, ctx, errors.New("worker lease ledger must already exist")
	}
	entries, err := os.ReadDir(p.bootstrap.LedgerDir)
	if err != nil {
		return nil, ctx, err
	}
	if len(entries) > 16384 {
		return nil, ctx, errors.New("worker lease ledger scan bound")
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, b.LeaseID+"-") {
			return nil, ctx, errors.New("worker lease ID already has records; no overwrite/reuse")
		}
		if strings.HasSuffix(name, "-acquire.json") {
			other := strings.TrimSuffix(name, "-acquire.json")
			info, e := os.Lstat(filepath.Join(p.bootstrap.LedgerDir, other+"-release.json"))
			if e != nil || !info.Mode().IsRegular() {
				return nil, ctx, fmt.Errorf("unreleased/unknown existing lease: %s", other)
			}
		}
	}
	if err = ctx.Err(); err != nil {
		return nil, ctx, err
	}
	l := &workerOwnedLease{policy: p, binding: b, authority: pre.approval.SourceID, stop: make(chan struct{}), done: make(chan struct{})}
	l.acquired, err = json.Marshal(l.row("started"))
	if err != nil {
		return nil, ctx, err
	}
	if err = l.writeNew("acquire", l.acquired); err != nil {
		return nil, ctx, err
	}
	live, cancel := context.WithCancel(ctx)
	go func() {
		defer close(l.done)
		defer cancel()
		ticker := time.NewTicker(240 * time.Second)
		defer ticker.Stop()
		timer := time.NewTimer(time.Until(b.HardDeadline))
		defer timer.Stop()
		for {
			select {
			case <-l.stop:
				return
			case <-timer.C:
				l.err = errors.New("worker lease original hard deadline reached")
				return
			case <-ticker.C:
				if err := l.renew(); err != nil {
					l.err = err
					return
				}
			}
		}
	}()
	return l, live, nil
}

// Preserve the existing m01/m02 quarantine gate while canonical FD9 is held.
// Missing, unreadable, changed or non-regular state is UNKNOWN, never clearance.
func checkWorkerQuarantine(ctx context.Context, lock string) error {
	name := strings.TrimSuffix(lock, ".lock") + ".quarantine"
	before, err := os.Lstat(name)
	if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return errors.New("worker quarantine directory UNKNOWN")
	}
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return errors.New("worker quarantine changed")
	}
	entries, err := f.ReadDir(16385)
	if (err != nil && err != io.EOF) || len(entries) > 16384 {
		return errors.New("worker quarantine scan UNKNOWN/bound exceeded")
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := entry.Name()
		if !(strings.HasPrefix(n, "m01-") || strings.HasPrefix(n, "m02-")) || !strings.HasSuffix(n, ".json") || strings.HasSuffix(n, ".cleared.json") {
			continue
		}
		record, e := os.Lstat(filepath.Join(name, n))
		if e != nil || !record.Mode().IsRegular() {
			return fmt.Errorf("worker quarantine record UNKNOWN: %s", n)
		}
		marker, e := os.Lstat(filepath.Join(name, strings.TrimSuffix(n, ".json")+".cleared.json"))
		if e != nil || !marker.Mode().IsRegular() {
			return fmt.Errorf("worker quarantine uncleared/UNKNOWN: %s", n)
		}
	}
	after, err := os.Lstat(name)
	if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		return errors.New("worker quarantine changed during scan")
	}
	return ctx.Err()
}

func (l *workerOwnedLease) row(clock string) map[string]any {
	row := map[string]any{"lease_id": l.binding.LeaseID, "agent": l.binding.LeaseOwner, clock: float64(time.Now().UnixNano()) / 1e9, "pid": os.Getpid(), "host": "m01", "run_id": l.binding.RunID, "authority": l.authority, "hard_deadline": l.binding.HardDeadline}
	label := map[string]string{"started": "start_time", "renewed": "renew_time", "released": "release_time"}[clock]
	row[label] = time.Now().UTC().Format(time.RFC3339Nano)
	if clock == "started" {
		row["purpose"] = "S0b inventory reproduction / " + l.binding.RunID
	}
	if clock == "released" {
		row["outcome"] = "RESOURCES_SETTLED"
	}
	return row
}
func (l *workerOwnedLease) path(kind string) string {
	return filepath.Join(l.policy.bootstrap.LedgerDir, l.binding.LeaseID+"-"+kind+".json")
}
func (l *workerOwnedLease) writeNew(kind string, raw []byte) error {
	if !time.Now().Before(l.binding.HardDeadline) {
		return errors.New("ledger write outside original hard deadline")
	}
	if err := verifyWorkerFlock(l.policy.bootstrap.WorkerLease.LockFile); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path(kind), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, w := f.Write(append(raw, '\n'))
	s := f.Sync()
	c := f.Close()
	if err := errors.Join(w, s, c); err != nil {
		return err
	}
	return settleWorkerRecord(l.path(kind), append(raw, '\n'), l.binding.HardDeadline)
}

// CIFS may publish final mtime/ctime shortly after close. Settle only records
// this writer just published, then keep the ordinary strict capture unchanged.
func settleWorkerRecord(name string, expected []byte, hard time.Time) error {
	return settleWorkerRecordWith(name, expected, hard, os.Lstat, snapshotRecord, time.Now, time.Sleep)
}

func settleWorkerRecordWith(name string, expected []byte, hard time.Time, stat func(string) (os.FileInfo, error), capture func(string, *uint32, bool) ([]byte, error), now func() time.Time, sleep func(time.Duration)) error {
	deadline := now().Add(6 * time.Second)
	if hard.Before(deadline) {
		deadline = hard
	}
	var previous os.FileInfo
	var stableSince time.Time
	for now().Before(deadline) {
		sampleStart := now()
		current, err := stat(name)
		if err != nil {
			return err
		}
		if !current.Mode().IsRegular() || current.Size() != int64(len(expected)) {
			return errors.New("written worker record size/type changed")
		}
		if !now().Before(deadline) {
			break
		}
		if previous == nil || !sameRecord(previous, current) {
			stableSince = now()
		} else if sampleStart.Sub(stableSince) >= 2500*time.Millisecond {
			raw, err := capture(name, nil, false)
			if err != nil {
				return err
			}
			if !now().Before(deadline) || string(raw) != string(expected) {
				return errors.New("written worker record capture changed or expired")
			}
			return nil
		}
		previous = current
		wait := deadline.Sub(now())
		if wait > 250*time.Millisecond {
			wait = 250 * time.Millisecond
		}
		if wait > 0 {
			sleep(wait)
		}
	}
	return errors.New("worker record 2.5s stable window exceeded 6s/original deadline")
}
func (l *workerOwnedLease) qualify() error {
	if err := verifyWorkerFlock(l.policy.bootstrap.WorkerLease.LockFile); err != nil {
		return err
	}
	raw, err := snapshotRecord(l.path("acquire"), nil, false)
	if err != nil {
		return err
	}
	if string(raw) != string(l.acquired)+"\n" {
		return errors.New("worker lease acquire changed")
	}
	_, err = ReadLiveLease(l.policy.bootstrap.LedgerDir, l.binding.LeaseID, l.binding.LeaseOwner)
	return err
}
func (l *workerOwnedLease) renew() error {
	if err := l.qualify(); err != nil {
		return err
	}
	if !time.Now().Before(l.binding.HardDeadline) {
		return errors.New("renew outside original envelope")
	}
	raw, err := json.Marshal(l.row("renewed"))
	if err != nil {
		return err
	}
	// Only this lifetime's renewal is replaced, after exact own-acquire/live checks.
	f, err := os.CreateTemp(l.policy.bootstrap.LedgerDir, l.binding.LeaseID+"-renew-*.tmp")
	if err != nil {
		return err
	}
	_, w := f.Write(append(raw, '\n'))
	s := f.Sync()
	c := f.Close()
	if err = errors.Join(w, s, c); err != nil {
		return err
	}
	if err = l.qualify(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), l.path("renew")); err != nil {
		return err
	}
	if err = settleWorkerRecord(l.path("renew"), append(raw, '\n'), l.binding.HardDeadline); err != nil {
		return err
	}
	return l.qualify()
}
func (l *workerOwnedLease) finish(settled bool) error {
	l.once.Do(func() {
		close(l.stop)
		timer := time.NewTimer(time.Until(l.binding.HardDeadline))
		defer timer.Stop()
		select {
		case <-l.done:
		case <-timer.C:
			l.finishErr = errors.New("worker lease writer join UNKNOWN")
			return
		}
		if !settled {
			l.finishErr = errors.Join(l.err, errors.New("worker lease retained UNKNOWN; resources not proven settled"))
			return
		}
		if err := l.qualify(); err != nil {
			l.finishErr = errors.Join(l.err, err)
			return
		}
		raw, _ := json.Marshal(l.row("released"))
		writeErr := l.writeNew("release", raw)
		l.released = writeErr == nil
		l.finishErr = errors.Join(l.err, writeErr)
	})
	return l.finishErr
}

// Resource proof is independent of the test outcome. A proven failed job may
// release; a missing exit, possible issuer, incomplete restore or reader may not.
func (a *ApprovedRun) resourcesSettled() bool {
	a.mu.Lock()
	readers := a.readers
	enginePending := a.engineStarted && !a.engineJoined
	a.mu.Unlock()
	if readers != 0 || enginePending {
		return false
	}
	if a.watchDone != nil {
		select {
		case <-a.watchDone:
		default:
			return false
		}
	}
	s := a.scope
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
	default:
		return false
	}
	if len(s.jobs) != 0 {
		return false
	}
	for _, r := range s.restores {
		// A concurrent restoration must have joined its own existing gate;
		// neither a deadline nor scope.done proves that callback is absent.
		select {
		case r.gate <- struct{}{}:
		default:
			return false
		}
		finalVerified := r.finalVerified
		<-r.gate
		if !finalVerified {
			return false
		}
	}
	return true
}
