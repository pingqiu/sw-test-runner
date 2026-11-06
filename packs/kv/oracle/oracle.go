// Package oracle checks KV receipts against independently planned content.
package oracle

import (
	"errors"
	"strings"
	"sync"
)

// Content contains observed bytes or an expectation computed BEFORE dispatch.
// A write expectation must never be learned from the read being checked.
type Content struct {
	Length uint64
	SHA256 [32]byte
}
type Version struct {
	Sequence uint64
	Content  Content
	FIDs     []string
}

type RetirementEvidence struct{ MasterSucceeded, TopologyAbsent, DataAbsent bool }

// Retire consumes the three independently collected assertions; it performs no I/O.
func (o *Oracle) Retire(volumeID string, proof RetirementEvidence) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if volumeID == "" || !proof.MasterSucceeded || !proof.TopologyAbsent || !proof.DataAbsent {
		return errors.New("retirement is not independently confirmed")
	}
	o.retired[volumeID] = true
	return nil
}

// Oracle is one run's in-memory, concurrency-safe correctness state. Use New.
// Callers must join completion identity before calling it, persist ACK receipts,
// and stop on errors. It does not own workers, timeouts or evidence collection.
type Oracle struct {
	mu                       sync.Mutex
	sequence                 uint64
	latest, pending, reading map[string]Version
	unknown                  map[string]bool
	retired                  map[string]bool
}

// UncertainWrite fences a submitted mutation whose terminal outcome is unknown.
// The caller must stop ordinary work. Neither a retry nor a late ACK un-fences it.
func (o *Oracle) UncertainWrite(key string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.pending[key]; !ok {
		return errors.New("no submitted mutation")
	}
	o.unknown[key] = true
	return errors.New("UNKNOWN_WRITE: stop; no automatic retry or invented ACK")
}

func New() *Oracle {
	return &Oracle{latest: map[string]Version{}, pending: map[string]Version{}, reading: map[string]Version{}, unknown: map[string]bool{}, retired: map[string]bool{}}
}
func clone(v Version) Version { v.FIDs = append([]string(nil), v.FIDs...); return v }
func (o *Oracle) BeginWrite(key string, expected Content) (Version, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.pending[key]; ok {
		return Version{}, errors.New("overlapping same-key operation")
	}
	if _, ok := o.reading[key]; ok {
		return Version{}, errors.New("overlapping same-key operation")
	}
	if o.sequence == ^uint64(0) {
		return Version{}, errors.New("sequence exhausted")
	}
	o.sequence++
	v := Version{Sequence: o.sequence, Content: expected}
	o.pending[key] = v
	return clone(v), nil
}
func (o *Oracle) Acknowledge(key string, sequence, length uint64, fids []string) (Version, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	v, ok := o.pending[key]
	if !ok || o.unknown[key] || sequence != v.Sequence || length != v.Content.Length {
		return Version{}, errors.New("ACK does not match planned write")
	}
	if len(fids) == 0 {
		return Version{}, errors.New("ACK lacks independently resolved FIDs")
	}
	for _, fid := range fids {
		if !strings.Contains(fid, ",") {
			return Version{}, errors.New("ACK lacks independently resolved FIDs")
		}
	}
	v.FIDs = append([]string(nil), fids...)
	o.latest[key] = v
	delete(o.pending, key)
	return clone(v), nil
}
func (o *Oracle) BeginRead(key string) (Version, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.pending[key]; ok {
		return Version{}, errors.New("ambiguous read during write")
	}
	if _, ok := o.reading[key]; ok {
		return Version{}, errors.New("existing read")
	}
	v, ok := o.latest[key]
	if !ok {
		return Version{}, errors.New("unacknowledged key")
	}
	o.reading[key] = v
	return clone(v), nil
}
func (o *Oracle) FinishRead(key string, status int, actual Content) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	v, ok := o.reading[key]
	if !ok {
		return "", errors.New("read lacks its per-key guard")
	}
	delete(o.reading, key)
	if status == 200 {
		if actual == v.Content {
			return "hit", nil
		}
		return "", errors.New("WRONG_BYTES: latest ACK hash/length differs")
	}
	if status == 404 {
		for _, fid := range v.FIDs {
			if !o.retired[strings.SplitN(fid, ",", 2)[0]] {
				return "", errors.New("unexpected miss for latest ACK version")
			}
		}
		return "retired_miss", nil
	}
	return "", errors.New("non-success needs explicit fault attribution")
}
