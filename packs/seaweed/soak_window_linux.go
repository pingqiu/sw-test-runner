//go:build linux

package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
)

// soakWindow is the fault-window side of soakOps: the load-start witness and
// the pause/ack/close files in the M01 run root, bound to the run, the M01
// boot and the episode. It needs no runner job; direct ssh uses it unchanged.
type soakWindow struct {
	root   string // the M01 run root
	runID  string
	bootID string            // the admitted M01 boot
	roles  map[string]string // episode -> role, bound by Pause
}

// LoadStart reads the worker's witness: this boot, a finite positive clock.
func (s *soakWindow) LoadStart(ctx context.Context) (float64, error) {
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

func (s *soakWindow) binding(episode string) map[string]any {
	return map[string]any{"run_id": s.runID, "boot_id": s.bootID, "episode": episode, "role": s.roles[episode]}
}
func (s *soakWindow) Pause(episode, role string, pauseMono float64) error {
	s.roles[episode] = role
	b := s.binding(episode)
	b["pause_mono"] = pauseMono
	return publishFile(filepath.Join(s.root, "fault-pause-"+episode+".json"), b)
}
func (s *soakWindow) Close(episode string, endMono float64) error {
	b := s.binding(episode)
	b["end_mono"] = endMono
	return publishFile(filepath.Join(s.root, "fault-close-"+episode+".json"), b)
}
func (s *soakWindow) Ack(ctx context.Context, episode string) (SoakAck, error) {
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
