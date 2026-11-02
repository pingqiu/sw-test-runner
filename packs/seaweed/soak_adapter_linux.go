//go:build linux

package seaweed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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
	a        *tr.ActionContext
	root     string // the M01 run root
	runID    string
	bootID   string // the admitted M01 boot
	deadline time.Time
	roles    map[string]string // episode -> role, bound by Pause
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
	boot := ""
	for _, h := range d.Observation.Hosts {
		if h.HostID == "m01" {
			boot = h.BootID
		}
	}
	if b, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err != nil || boot == "" || string(bytes.TrimSpace(b)) != boot {
		return refusal("seaweed_kv_soak", "BINDING_REFUSED", errors.New("runner is not the admitted M01 clock"))
	}
	ops := &soakAdapter{a: a, root: runRoot(d, "m01"), runID: d.RunID, bootID: boot, deadline: in.Binding.WorkDeadline, roles: map[string]string{}}
	records, _, err := runSoak(ctx, ops, p, in, sess.jobs)
	if perr := publishFile(filepath.Join(ops.root, "faults.json"), records); err == nil {
		err = perr
	}
	if err != nil {
		return refusal("seaweed_kv_soak", "SOAK_FAILED", err)
	}
	return admittedResult("completed", "OK", "soak load and fault schedule completed; the verdict is computed at collect", "seaweed_kv_soak", d), nil
}

// LoadStart reads the worker's witness: this boot, a finite positive clock.
func (s *soakAdapter) LoadStart(ctx context.Context) (float64, error) {
	var w struct {
		BootID string  `json:"boot_id"`
		Mono   float64 `json:"mono"`
	}
	if err := pollBound(ctx, filepath.Join(s.root, "soak-load-start.json"), &w); err != nil {
		return 0, err
	}
	if w.BootID != s.bootID || math.IsNaN(w.Mono) || math.IsInf(w.Mono, 0) || w.Mono <= 0 {
		return 0, errors.New("soak: load-start witness foreign or malformed")
	}
	return w.Mono, nil
}

func (s *soakAdapter) binding(episode string) map[string]any {
	return map[string]any{"run_id": s.runID, "boot_id": s.bootID, "episode": episode, "role": s.roles[episode]}
}
func (s *soakAdapter) Pause(episode, role string, pauseMono float64) error {
	s.roles[episode] = role
	b := s.binding(episode)
	b["pause_mono"] = pauseMono
	return publishFile(filepath.Join(s.root, "fault-pause-"+episode+".json"), b)
}
func (s *soakAdapter) Close(episode string, endMono float64) error {
	b := s.binding(episode)
	b["end_mono"] = endMono
	return publishFile(filepath.Join(s.root, "fault-close-"+episode+".json"), b)
}
func (s *soakAdapter) Ack(ctx context.Context, episode string) (SoakAck, error) {
	var raw map[string]json.RawMessage
	var a SoakAck
	path := filepath.Join(s.root, "fault-ack-"+episode+".json")
	if err := pollBound(ctx, path, &raw); err != nil {
		return a, err
	}
	for k, want := range s.binding(episode) {
		var got string
		if json.Unmarshal(raw[k], &got) != nil || got != want {
			return a, fmt.Errorf("soak: fault-window ack binding differs on %s", k)
		}
	}
	body, _ := json.Marshal(raw)
	return a, json.Unmarshal(body, &a)
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
