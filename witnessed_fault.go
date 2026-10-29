package testrunner

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"
)

// FaultObservation is the complete touched configuration, not a command rc.
// The provider reads live state and refuses missing/ambiguous enumeration.
// Reads are bracketed locally, without cross-host clock comparisons.
type FaultObservation struct {
	Node, BootID, Resource string
	State                  map[string]string
	ReadStart, ReadEnd     time.Time
}

type WitnessedFault struct {
	Kind, Node, Resource string
	Before, During       map[string]string
	Apply, Restore       JobSpec
	Observe              func(context.Context) (FaultObservation, error)
}

func (p WitnessedFault) Key() string { return p.Node + "/" + p.Resource }

type FaultReceipt struct {
	Kind, Node, Resource      string
	Before, Applied, Restored FaultObservation
	ApplyExit, RestoreExit    ExitInfo
	RestoreNotNeeded          bool
}

func cloneObservation(v FaultObservation) FaultObservation { v.State = maps.Clone(v.State); return v }

func (s *RunScope) FaultReceipts() map[string]FaultReceipt {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := maps.Clone(s.faultReceipts)
	for key, r := range out {
		r.Before, r.Applied, r.Restored = cloneObservation(r.Before), cloneObservation(r.Applied), cloneObservation(r.Restored)
		out[key] = r
	}
	return out
}

// ApplyWitnessedFault binds a pre-reviewed TierChaos plan. Commands use owned
// jobs; read-only providers and COMPLETE resource-state parsers belong to the
// pack. They are trusted code, not evidence synthesized by this substrate.
func (s *RunScope) ApplyWitnessedFault(actionCtx context.Context, key string, p WitnessedFault) error {
	if key != p.Key() || (p.Kind != "partition" && p.Kind != "netem" && p.Kind != "fill_disk") || p.Node == "" || p.Resource == "" || p.Observe == nil || len(p.Before) == 0 || len(p.During) == 0 || maps.Equal(p.Before, p.During) || p.Apply.Node != p.Node || p.Restore.Node != p.Node {
		return errors.New("incomplete witnessed TierChaos plan")
	}
	p.Before, p.During = maps.Clone(p.Before), maps.Clone(p.During)
	if len(p.Before) != len(p.During) {
		return errors.New("fault state schemas differ")
	}
	for key := range p.Before {
		if _, ok := p.During[key]; !ok {
			return errors.New("fault state schemas differ")
		}
	}
	p.Apply.Argv, p.Restore.Argv = append([]string(nil), p.Apply.Argv...), append([]string(nil), p.Restore.Argv...)
	receipt := FaultReceipt{Kind: p.Kind, Node: p.Node, Resource: p.Resource}
	store := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.faultReceipts == nil {
			s.faultReceipts = make(map[string]FaultReceipt)
		}
		s.faultReceipts[key] = receipt
	}
	observe := func(ctx context.Context) (FaultObservation, error) {
		start := time.Now()
		x, err := p.Observe(ctx)
		x.State, x.ReadStart, x.ReadEnd = maps.Clone(x.State), start, time.Now()
		if err == nil && (x.Node != p.Node || x.Resource != p.Resource || x.BootID == "" || len(x.State) == 0 || (receipt.Before.BootID != "" && x.BootID != receipt.Before.BootID)) {
			err = errors.New("fault observation identity/state UNKNOWN")
		}
		return x, errors.Join(err, ctx.Err())
	}
	var apply JobHandle
	baseline := false
	return s.ApplyFault(key, func(workCtx context.Context) error {
		ctx, cancel := context.WithCancel(actionCtx)
		stop := context.AfterFunc(workCtx, cancel)
		defer stop()
		defer cancel()
		var err error
		receipt.Before, err = observe(ctx)
		store()
		if err != nil || !maps.Equal(receipt.Before.State, p.Before) {
			return errors.Join(err, errors.New("fault baseline differs from reviewed plan"))
		}
		baseline = true
		apply, receipt.ApplyExit, err = s.faultJob(ctx, p.Apply, false)
		var probeErr error
		receipt.Applied, probeErr = observe(ctx)
		store()
		if receipt.ApplyExit.Identity.BootID != receipt.Before.BootID || !maps.Equal(receipt.Applied.State, p.During) {
			probeErr = errors.Join(probeErr, errors.New("fault effect NOT_PROVEN"))
		}
		return errors.Join(err, probeErr)
	}, func(ctx context.Context) error {
		if !baseline {
			return nil
		}
		if apply.scope != nil {
			if err := s.Stop(ctx, apply); err != nil {
				return fmt.Errorf("possible fault issuer: %w", err)
			}
		}
		current, err := observe(ctx)
		if err != nil {
			return err
		}
		if len(current.State) != len(p.Before) {
			return errors.New("restore target schema changed")
		}
		for k, v := range current.State {
			before, known := p.Before[k]
			if !known || (v != before && v != p.During[k]) {
				return errors.New("restore target identity/configuration changed")
			}
		}
		if maps.Equal(current.State, p.Before) {
			receipt.RestoreNotNeeded = true
			store()
			return nil
		}
		_, exit, err := s.faultJob(ctx, p.Restore, true)
		receipt.RestoreExit = exit
		store()
		if exit.Identity.BootID != receipt.Before.BootID {
			err = errors.Join(err, errors.New("restoration job boot does not match resource"))
		}
		return err
	}, func(ctx context.Context) error {
		if !baseline {
			return errors.New("no qualified baseline; no restoration claim")
		}
		var err error
		receipt.Restored, err = observe(ctx)
		store()
		if !maps.Equal(receipt.Restored.State, receipt.Before.State) {
			err = errors.Join(err, errors.New("fault restoration/absence NOT_PROVEN"))
		}
		return err
	})
}

func (s *RunScope) faultJob(ctx context.Context, spec JobSpec, restoration bool) (h JobHandle, exit ExitInfo, err error) {
	h, err = s.start(ctx, spec, restoration)
	defer func() {
		if h.scope != nil {
			cleanup, cancel := s.cleanupContext(context.Background())
			defer cancel()
			err = errors.Join(err, s.Stop(cleanup, h))
		}
	}()
	if err != nil {
		return h, ExitInfo{}, err
	}
	j, unlock, err := s.lockJob(ctx, h)
	if err != nil {
		return h, ExitInfo{}, err
	}
	defer unlock()
	exit, err = s.backend.Wait(ctx, h.id)
	validStatus := (exit.Code >= 0 && exit.Code <= 255 && exit.Signal == 0) || (exit.Code == -1 && exit.Signal > 0 && exit.Signal <= 64)
	if err == nil && exit.Settled && validStatus && exit.Identity == j.identity && exit.StdoutPath == j.spec.StdoutPath && exit.StderrPath == j.spec.StderrPath {
		j.exit = &exit
		s.retired(h)
	} else {
		err = errors.Join(err, errors.New("fault command exit not proven"))
	}
	if exit.Code != 0 || exit.Signal != 0 {
		err = errors.Join(err, errors.New("fault command failed"))
	}
	return h, exit, err
}
