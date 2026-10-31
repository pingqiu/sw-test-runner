//go:build linux

package testrunner

import (
	"context"
	"encoding/json"
	"testing"

	"golang.org/x/sys/unix"
)

func TestGenerationReceiptAnchorsBeforeKillDispatch(t *testing.T) {
	f, n := generationFixture(t)
	mono := func() float64 {
		var ts unix.Timespec
		if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
			t.Fatal(err)
		}
		return float64(ts.Sec) + float64(ts.Nsec)/1e9
	}
	var before, dispatch float64
	n.beforeKill = func() { dispatch = mono() }
	run, result := runGenerationCase(t, f, func(ctx context.Context, a *ActionContext) error {
		old, _, err := a.RunApprovedPackJob(ctx, "volume0", true)
		if err != nil {
			return err
		}
		before = mono()
		_, _, err = a.RestartApprovedPackGeneration(ctx, "volume-once", old)
		return err
	})
	if result.Status != StatusPass {
		t.Fatalf("restart: %+v", result)
	}
	defer run.Close()
	raw, _ := json.Marshal(run.GenerationRestartReceipts()["volume-once"])
	var receipt struct{ Kill struct{ PreKillMono float64 } }
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Kill.PreKillMono < before || receipt.Kill.PreKillMono > dispatch {
		t.Fatalf("pre-Kill marker %f outside runner interval [%f,%f]", receipt.Kill.PreKillMono, before, dispatch)
	}
}
