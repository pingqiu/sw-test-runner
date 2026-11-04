//go:build linux

package seaweed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
	"golang.org/x/sys/unix"
)

// soakAdapter is the production soakOps: the existing ActionContext calls,
// the M01 controller clock, and the fault-window files in the M01 run root
// (the controller runs on M01, boot-checked). Its publish/read pair is the Go
// side of soak_load.py's publish/read_bound contract, not a second rule.
type soakAdapter struct {
	soakWindow
	a        *tr.ActionContext
	deadline time.Time
}

func (s *soakAdapter) StartTask(ctx context.Context, label string) (tr.JobHandle, error) {
	return s.a.StartApprovedPackTask(ctx, label)
}
func (s *soakAdapter) WaitTask(ctx context.Context, h tr.JobHandle) (tr.ExitInfo, error) {
	return s.a.WaitApprovedPackTask(ctx, h)
}
func (s *soakAdapter) Restart(ctx context.Context, episode string, victim tr.JobHandle) (tr.JobHandle, tr.GenerationRestartReceipt, error) {
	return s.a.RestartApprovedPackGeneration(ctx, episode, victim)
}
func (s *soakAdapter) RunJob(ctx context.Context, label string) (tr.ExitInfo, error) {
	_, e, err := s.a.RunApprovedPackJob(ctx, label, false)
	return e, err
}
func (s *soakAdapter) Mono() (float64, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0, err
	}
	return float64(ts.Sec) + float64(ts.Nsec)/1e9, nil
}
func (s *soakAdapter) Now() time.Time                         { return time.Now() }
func (s *soakAdapter) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (s *soakAdapter) WorkDeadline() time.Time                { return s.deadline }

// soak is the seaweed_kv_soak phase: the fault loop over the production
// adapter, then the FaultRecords published for the verdict (also on failure).
func (r *runtimePack) soak(ctx context.Context, a *tr.ActionContext, p Plan, d *inventory.Document, in tr.PackInputs, sess *session) (map[string]string, error) {
	boot := observedBoot(d, "m01")
	if b, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err != nil || boot == "" || string(bytes.TrimSpace(b)) != boot {
		return refusal("seaweed_kv_soak", "BINDING_REFUSED", errors.New("runner is not the admitted M01 clock"))
	}
	ops := &soakAdapter{soakWindow: soakWindow{root: runRoot(d, "m01"), runID: d.RunID, bootID: boot, roles: map[string]string{}}, a: a, deadline: in.Binding.WorkDeadline}
	records, _, err := runSoak(ctx, ops, p, in, sess.jobs)
	if perr := publishFile(filepath.Join(ops.root, "faults.json"), records); err == nil {
		err = perr
	}
	if err != nil {
		return refusal("seaweed_kv_soak", "SOAK_FAILED", err)
	}
	return admittedResult("completed", "OK", "soak load and fault schedule completed; the verdict is computed at collect", "seaweed_kv_soak", d), nil
}

// readBound reads one fault-window/witness file: no symlink, a regular file,
// at most 4096 bytes (more is refused, never truncated), strict JSON.
func readBound(path string, v any) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return errors.New("soak: not a regular file: " + filepath.Base(path))
	}
	body, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return err
	}
	if len(body) > 4096 {
		return errors.New("soak: file over 4096 bytes: " + filepath.Base(path))
	}
	return json.Unmarshal(body, v)
}

// pollBound waits (bounded by ctx) for path to appear, then reads it once.
func pollBound(ctx context.Context, path string, v any) error {
	for {
		err := readBound(path, v)
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
