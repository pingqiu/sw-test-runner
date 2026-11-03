package seaweed

import (
	"encoding/json"
	"strings"
	"testing"
)

func writeRows(t *testing.T, mutate func(sets []map[string]any, end map[string]any)) []byte {
	t.Helper()
	c := writeCfg()
	sets := []map[string]any{}
	// One SET each second from t=100; ten of them; the kill is at 104, READY at 106.
	for i := 0; i < 10; i++ {
		start, end := 100+float64(i), 100+float64(i)+0.01
		ok, errText := true, ""
		if i == 5 {
			end, ok, errText = start+0.5, false, "no writable volumes"
		}
		if i == 7 {
			end = start + 0.9
		}
		sets = append(sets, map[string]any{"event": "SET", "operation_id": i, "key": "m01-w" + string(rune('0'+i)), "started": start, "ended": end, "ok": ok, "error": errText})
	}
	stats := map[string]any{"loader": map[string]any{"transport": "rdma"}}
	end := map[string]any{"status": "OBSERVED", "code": "OK", "stage": "write-load", "transport": "dc", "run_id": c.RunID, "client": c.Client, "root": c.Root, "sets": 10, "acked": 9, "failed": 1, "readback_exact": 9, "readback_missing": 0, "started": 100.0, "ended": 120.0, "before": stats, "after": stats}
	if mutate != nil {
		mutate(sets, end)
	}
	out := []byte{}
	for _, r := range append(sets, end) {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		out = append(append(out, b...), '\n')
	}
	return out
}

func writeCfg() SmokeConfig {
	return SmokeConfig{RunID: "run", Client: "m01", Root: "/testops/run/m01", Transport: "dc", Keys: 1, Length: 64}
}

func TestWriteReportSplitsAroundKillAndReady(t *testing.T) {
	r, err := writeReport(writeRows(t, nil), writeCfg(), 104.5, 106.5)
	if err != nil {
		t.Fatal(err)
	}
	if r.Before.Sets != 5 || r.Outage.Sets != 2 || r.After.Sets != 3 {
		t.Fatalf("windows %+v", r)
	}
	if r.Outage.Failed != 1 || r.Before.Failed != 0 || r.After.Failed != 0 {
		t.Fatalf("failed %+v", r)
	}
	if r.After.MaxS < 0.89 || r.After.MaxS > 0.91 {
		t.Fatalf("max %+v", r.After)
	}
	if r.FirstAckAfterKillS != 1.51 {
		t.Fatalf("first ACK after kill %v", r.FirstAckAfterKillS)
	}
	if len(r.Errors) != 1 || r.Errors[0].Error != "no writable volumes" {
		t.Fatalf("errors %+v", r.Errors)
	}
	if r.Verdict != "REPORT_ONLY" {
		t.Fatalf("a threshold crept in: %s", r.Verdict)
	}
}

func TestWriteReportRefusesEvidenceItCannotBind(t *testing.T) {
	for name, m := range map[string]func([]map[string]any, map[string]any){
		"count":      func(_ []map[string]any, e map[string]any) { e["sets"] = 11 },
		"acked":      func(_ []map[string]any, e map[string]any) { e["acked"] = 10 },
		"run":        func(_ []map[string]any, e map[string]any) { e["run_id"] = "other" },
		"stage":      func(_ []map[string]any, e map[string]any) { e["stage"] = "load" },
		"id":         func(s []map[string]any, _ map[string]any) { s[3]["operation_id"] = 9 },
		"reused key": func(s []map[string]any, _ map[string]any) { s[3]["key"] = s[2]["key"] },
		"seed key":   func(s []map[string]any, _ map[string]any) { s[3]["key"] = "m01-0" },
		"overlap":    func(s []map[string]any, _ map[string]any) { s[3]["started"] = 100.5 },
		"backwards":  func(s []map[string]any, _ map[string]any) { s[3]["ended"] = 1.0 },
		"readback":   func(_ []map[string]any, e map[string]any) { e["readback_exact"] = 1 },
	} {
		if _, err := writeReport(writeRows(t, m), writeCfg(), 104.5, 106.5); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestWriteReportNeedsLoadSpanningTheRestart(t *testing.T) {
	for _, w := range [][2]float64{{50, 51}, {300, 301}, {106, 104}} {
		if _, err := writeReport(writeRows(t, nil), writeCfg(), w[0], w[1]); err == nil || !strings.Contains(err.Error(), "span") {
			t.Fatalf("window %v: %v", w, err)
		}
	}
}
