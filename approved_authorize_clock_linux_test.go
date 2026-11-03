//go:build linux

package testrunner

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/pingqiu/sw-test-runner/infra"
)

// authorizeClockNode records the controller clock as the underlying Authorize is entered.
type authorizeClockNode struct {
	*taskNode
	mu        sync.Mutex
	authEntry map[string]float64
}

func (n *authorizeClockNode) Authorize(ctx context.Context, id string, i ProcessIdentity) error {
	m, _ := authorizeMono()
	n.mu.Lock()
	n.authEntry[id] = m
	n.mu.Unlock()
	return n.taskNode.Authorize(ctx, id, i)
}

// authorizeClockFixture: a finite task job whose readbacks are timestamped at their end. A readback whose
// 1-based index is at least *failFrom (when set, > 0) fails, so a test can fail the job's final work check.
func authorizeClockFixture(t *testing.T) (*approvedFixture, *authorizeClockNode, *[]float64, *int) {
	f, n := generationFixture(t)
	task := &taskNode{generationNode: n, release: make(chan struct{}), taskWaiting: make(chan struct{})}
	close(task.release)
	node := &authorizeClockNode{taskNode: task, authEntry: map[string]float64{}}
	f.runtime.Jobs = node
	var mu sync.Mutex
	ends, failFrom := []float64{}, new(int)
	readback := f.runtime.Readback
	f.runtime.Readback = func(ctx context.Context, p infra.DeploymentPolicy) (infra.DeploymentReceipt, error) {
		r, err := readback(ctx, p)
		m, _ := authorizeMono()
		mu.Lock()
		defer mu.Unlock()
		ends = append(ends, m)
		if *failFrom > 0 && len(ends) >= *failFrom {
			err = errors.New("injected readback failure")
		}
		return r, err
	}
	return f, node, &ends, failFrom
}

func TestApprovedAuthorizeStampAfterFinalReadbackBeforeAuthorize(t *testing.T) {
	f, node, ends, _ := authorizeClockFixture(t)
	_, result := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
		h, e, err := a.RunApprovedPackJob(ctx, "volume0", false)
		if err != nil {
			return err
		}
		// Identity binding: an unsettled exit or another process identity gets no stamp (and does not consume it).
		unsettled := e
		unsettled.Settled = false
		if _, err := a.ApprovedAuthorizeMono(ctx, h, unsettled); err == nil {
			t.Error("unsettled exit got an authorize stamp")
		}
		other := e
		other.Identity.PID++
		if _, err := a.ApprovedAuthorizeMono(ctx, h, other); err == nil {
			t.Error("another process identity got an authorize stamp")
		}
		m, err := a.ApprovedAuthorizeMono(ctx, h, e)
		if err != nil {
			return err
		}
		node.mu.Lock()
		entry := node.authEntry[h.id]
		node.mu.Unlock()
		last := (*ends)[len(*ends)-1] // the authorize-side readback is the job's final work check
		if !(last <= m && m <= entry) {
			t.Errorf("stamp %.9f not after final readback %.9f and before Authorize entry %.9f", m, last, entry)
		}
		if _, err := a.ApprovedAuthorizeMono(ctx, h, e); err == nil {
			t.Error("authorize stamp was not consumed")
		}
		return nil
	})
	if result.Status != StatusPass {
		t.Fatalf("run failed: %+v", result)
	}
}

func TestApprovedAuthorizeStampAbsentAfterFailedFinalReadback(t *testing.T) {
	f, node, ends, failFrom := authorizeClockFixture(t)
	var enrolled, stamps, authorized int
	_, result := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
		*failFrom = len(*ends) + 2 // the job's enroll-side readback passes; its authorize-side readback fails
		_, _, err := a.RunApprovedPackJob(ctx, "volume0", false)
		if err == nil {
			t.Error("job ran after its final readback failed")
		}
		g := a.Safety.backend.(*approvedJobs)
		g.mu.Lock()
		stamps = len(g.authorized)
		enrolled = len(g.receipts)
		g.mu.Unlock()
		node.mu.Lock()
		authorized = len(node.authEntry)
		node.mu.Unlock()
		return nil
	})
	if enrolled == 0 {
		t.Fatal("control: the job never reached enrollment, so the authorize-side readback was not the one failed")
	}
	if stamps != 0 || authorized != 0 {
		t.Fatalf("failed final readback still produced %d stamps and %d underlying authorizations", stamps, authorized)
	}
	if result.Status == StatusPass {
		t.Fatal("failed readback hidden")
	}
}

func TestApprovedAuthorizeStampSecondAuthorizePoisons(t *testing.T) {
	g := &approvedJobs{}
	i := ProcessIdentity{Node: "m02", PID: 7, PGID: 7, StartTime: 9, BootID: "b"}
	g.stampAuthorize("job", i)
	g.stampAuthorize("job", i) // an ambiguous first authorization retried: no replacement stamp
	if _, err := g.takeAuthorize("job", i); err == nil {
		t.Fatal("a retried authorization kept a usable stamp")
	}
	g.stampAuthorize("one", i)
	other := i
	other.PID = 8
	if _, err := g.takeAuthorize("one", other); err == nil {
		t.Fatal("stamp released to another process identity")
	}
	g.stampAuthorize("two", i)
	if m, err := g.takeAuthorize("two", i); err != nil || m <= 0 {
		t.Fatalf("single authorization lost its stamp: %v %v", m, err)
	}
}

// codex01 review 6ae9daff (red control, adopted): a caller-set Settled flag on a killed task, with no stored
// terminal receipt, gets no stamp, and the refusal leaves the stamp in place.
func TestApprovedAuthorizeStampNeedsTheStoredSettledReceipt(t *testing.T) {
	f, node, _, _ := authorizeClockFixture(t)
	node.release = make(chan struct{})
	completed := false // the run recovers an action panic; reaching the end is part of the assertion
	_, _ = runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
		defer close(node.release)
		h, err := a.StartApprovedPackTask(ctx, "volume0")
		if err != nil {
			return err
		}
		w, err := a.Safety.Kill(ctx, h)
		if err != nil {
			return err
		}
		if h.job.exit != nil {
			t.Fatal("control: the job already has a recorded exit")
		}
		claimed := ExitInfo{Identity: w.Identity, Settled: true}
		if stamp, err := a.ApprovedAuthorizeMono(ctx, h, claimed); err == nil {
			t.Errorf("stamp %.9f returned for a caller-set Settled flag without a stored receipt", stamp)
		}
		g := a.Safety.backend.(*approvedJobs)
		g.mu.Lock()
		_, kept := g.authorized[h.id]
		g.mu.Unlock()
		if !kept {
			t.Error("a refused claim consumed the stamp")
		}
		completed = true
		return nil
	})
	if !completed {
		t.Fatal("the control action did not run to completion")
	}
}

// A genuine wait's receipt, altered in any field, is not that receipt: refused, stamp kept, then the exact receipt
// gets it once.
func TestApprovedAuthorizeStampRefusesAnAlteredReceipt(t *testing.T) {
	f, _, _, _ := authorizeClockFixture(t)
	_, result := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
		h, e, err := a.RunApprovedPackJob(ctx, "volume0", false)
		if err != nil {
			return err
		}
		altered := e
		altered.Code = 1
		if _, err := a.ApprovedAuthorizeMono(ctx, h, altered); err == nil {
			t.Error("an altered exit receipt got the stamp")
		}
		if _, err := a.ApprovedAuthorizeMono(ctx, h, e); err != nil {
			t.Errorf("the exact stored receipt lost its stamp after a refused claim: %v", err)
		}
		return nil
	})
	if result.Status != StatusPass {
		t.Fatalf("run failed: %+v", result)
	}
}
