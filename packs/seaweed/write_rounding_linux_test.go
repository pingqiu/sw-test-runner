//go:build linux

package seaweed

import (
	"encoding/json"
	"strconv"
	"testing"
)

func writeLoadRows(c SmokeConfig, kill float64, mode string) []byte {
	out := []byte{}
	row := func(v map[string]any) {
		b, _ := json.Marshal(v)
		out = append(append(out, b...), '\n')
	}
	stats := map[string]any{"loader": map[string]any{"transport": "rdma"}}
	sets, acked := 10, 0
	for i := 0; i < sets; i++ {
		start := kill - 30 + float64(i)*10
		ok := i != 3
		if ok {
			acked++
		}
		id := i
		if mode == "unbound-set" && i == 4 {
			id = 7
		}
		errText := ""
		if !ok {
			errText = "no writable volumes"
		}
		row(map[string]any{"event": "SET", "operation_id": id, "key": c.Client + "-w" + strconv.Itoa(i), "started": start, "ended": start + 1, "ok": ok, "error": errText})
	}
	row(map[string]any{"event": "GET", "operation_id": 0, "key": c.Client + "-w0", "started": kill + 5, "ended": kill + 8, "ok": true, "error": ""})
	// Give the positive fixture 1s headroom above the strict 120s minimum:
	// independently rounded kill-30 and kill+90 can subtract to just under 120.
	end := map[string]any{"status": "OBSERVED", "code": "OK", "stage": "write-load", "transport": "dc", "run_id": c.RunID, "client": c.Client, "root": c.Root, "sets": sets, "gets": 1, "acked": acked, "failed": sets - acked, "readback_exact": acked, "readback_missing": 0, "started": kill - 30, "ended": kill + 91, "before": stats, "after": stats}
	row(end)
	return out
}

func TestMasterWritePositiveFixtureAcrossClockRounding(t *testing.T) {
	for _, clock := range []struct{ sec, nsec int64 }{{100, 0}, {100, 1}, {166, 2}, {4095, 3}, {65500, 1}} {
		kill := float64(clock.sec) + float64(clock.nsec)/1e9 // same conversion as restartCPU.Kill
		raw := writeLoadRows(writeCfg(), kill, "positive")
		_, terminal, err := writeRows(raw)
		if err != nil {
			t.Fatal(err)
		}
		span := *terminal.Ended - *terminal.Started
		t.Logf("kill=%.17g started=%.17g ended=%.17g span=%.17g", kill, *terminal.Started, *terminal.Ended, span)
		if _, err := writeReport(raw, writeCfg(), kill, kill+40); err != nil {
			t.Fatalf("valid positive fixture rejected: %v", err)
		}
	}
	short := fixtureRows(t, func(_, _ []map[string]any, end map[string]any) { end["ended"] = 219.999999999 })
	if _, err := writeReport(short, writeCfg(), 104.5, 106.5); err == nil || err.Error() != "write load phase or totals out of bounds" {
		t.Fatalf("strict 120s bound weakened: %v", err)
	}
}
