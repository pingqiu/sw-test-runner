package seaweed

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func writeCfg() SmokeConfig {
	return SmokeConfig{RunID: "run", Client: "m01", Root: "/testops/run/m01", Transport: "dc", Keys: 1, Length: 64}
}

// One SET each second from t=100, ten of them; id 5 fails, id 7 is slow. Three
// GETs, one stalled across READY. The kill is at 104.5, READY at 106.5.
func fixtureRows(t *testing.T, mutate func(sets, gets []map[string]any, end map[string]any)) []byte {
	t.Helper()
	c := writeCfg()
	sets, gets := []map[string]any{}, []map[string]any{}
	for i := 0; i < 10; i++ {
		start, stop := 100+float64(i), 100+float64(i)+0.01
		ok, text := true, ""
		if i == 5 {
			stop, ok, text = start+0.5, false, "no writable volumes"
		}
		if i == 7 {
			stop = start + 0.9
		}
		sets = append(sets, map[string]any{"event": "SET", "operation_id": i, "key": "m01-w" + strconv.Itoa(i), "started": start, "ended": stop, "ok": ok, "error": text})
	}
	for i, w := range [][2]float64{{101, 101.01}, {105, 108}, {109, 109.01}} {
		gets = append(gets, map[string]any{"event": "GET", "operation_id": i, "key": "m01-w0", "started": w[0], "ended": w[1], "ok": true, "error": ""})
	}
	stats := map[string]any{"loader": map[string]any{"transport": "rdma"}}
	end := map[string]any{"status": "OBSERVED", "code": "OK", "stage": "write-load", "transport": "dc", "run_id": c.RunID, "client": c.Client, "root": c.Root, "sets": 10, "gets": 3, "acked": 9, "failed": 1, "readback_exact": 9, "readback_missing": 0, "started": 100.0, "ended": 220.0, "before": stats, "after": stats}
	if mutate != nil {
		mutate(sets, gets, end)
	}
	out := []byte{}
	for _, r := range append(append(sets, gets...), end) {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		out = append(append(out, b...), '\n')
	}
	return out
}

func TestWriteReportSplitsAroundKillAndReady(t *testing.T) {
	r, err := writeReport(fixtureRows(t, nil), writeCfg(), 104.5, 106.5)
	if err != nil {
		t.Fatal(err)
	}
	if r.Sets[0].Ops != 5 || r.Sets[1].Ops != 2 || r.Sets[2].Ops != 3 {
		t.Fatalf("set windows %+v", r.Sets)
	}
	if r.Sets[1].Failed != 1 || r.Sets[0].Failed != 0 || r.Sets[2].Failed != 0 {
		t.Fatalf("failed %+v", r.Sets)
	}
	if r.Sets[2].MaxS < 0.89 || r.Sets[2].MaxS > 0.91 {
		t.Fatalf("max %+v", r.Sets[2])
	}
	if r.RereadGets[0].Ops != 1 || r.RereadGets[1].Ops != 1 || r.RereadGets[2].Ops != 1 || r.RereadGets[1].MaxS != 3 {
		t.Fatalf("get windows %+v", r.RereadGets)
	}
	if r.FirstAckAfterKillS < 1.50 || r.FirstAckAfterKillS > 1.52 {
		t.Fatalf("first ACK after kill %v", r.FirstAckAfterKillS)
	}
	if len(r.Errors) != 1 || r.Errors[0].Error != "no writable volumes" || r.Errors[0].Event != "SET" {
		t.Fatalf("errors %+v", r.Errors)
	}
	if r.FreshGets[1].Ops != 0 || r.RereadGets[1].Ops != 1 {
		t.Fatalf("fresh windows %+v", r.FreshGets)
	}
	if r.Verdict != "REPORT_ONLY" {
		t.Fatalf("a threshold crept in: %s", r.Verdict)
	}
}

// codex01: a SET submitted before the kill that completes after it is the first ACK.
func TestFirstAckUsesCompletionNotSubmission(t *testing.T) {
	r, err := writeReport(fixtureRows(t, nil), writeCfg(), 104.005, 106.5)
	if err != nil {
		t.Fatal(err)
	}
	if r.FirstAckAfterKillS < 0.004 || r.FirstAckAfterKillS > 0.006 {
		t.Fatalf("first ACK after kill %v", r.FirstAckAfterKillS)
	}
}

func TestFreshGetsAreCountedApartAndOnlyOnce(t *testing.T) {
	r, err := writeReport(fixtureRows(t, func(_, g []map[string]any, _ map[string]any) {
		g[1]["fresh"], g[1]["key"] = true, "m01-w1"
	}), writeCfg(), 104.5, 106.5)
	if err != nil {
		t.Fatal(err)
	}
	if r.FreshGets[1].Ops != 1 || r.RereadGets[1].Ops != 0 || r.FreshGets[1].MaxS != 3 {
		t.Fatalf("fresh %+v gets %+v", r.FreshGets, r.RereadGets)
	}
	_, err = writeReport(fixtureRows(t, func(_, g []map[string]any, _ map[string]any) {
		g[1]["fresh"], g[2]["fresh"] = true, true
		g[1]["key"], g[2]["key"] = "m01-w1", "m01-w1"
	}), writeCfg(), 104.5, 106.5)
	if err == nil {
		t.Fatal("a repeated fresh GET was accepted")
	}
}

// codex01: a fresh GET is the key's first GET of any kind, whatever order the rows arrive in.
func TestFreshGetAfterAnOrdinaryGetIsRefused(t *testing.T) {
	_, err := writeReport(fixtureRows(t, func(_, g []map[string]any, _ map[string]any) {
		g[1]["fresh"] = true // g[0] already read this key
	}), writeCfg(), 104.5, 106.5)
	if err == nil {
		t.Fatal("fresh credit for a key already read")
	}
	_, err = writeReport(fixtureRows(t, func(_, g []map[string]any, _ map[string]any) {
		g[1]["fresh"], g[1]["key"] = true, "m01-w1"
		g[2]["key"] = "m01-w1" // an ordinary reread after the fresh read is fine
	}), writeCfg(), 104.5, 106.5)
	if err != nil {
		t.Fatal(err)
	}
}

// codex01: two fresh GETs identical but for their operation id are still two reads of one key.
func TestTwoIdenticalFreshGetsAreRefused(t *testing.T) {
	_, err := writeReport(fixtureRows(t, func(_, g []map[string]any, _ map[string]any) {
		for _, i := range []int{1, 2} {
			g[i]["fresh"], g[i]["key"], g[i]["started"], g[i]["ended"] = true, "m01-w1", 105.0, 108.0
		}
	}), writeCfg(), 104.5, 106.5)
	if err == nil {
		t.Fatal("two fresh reads of one key accepted")
	}
}

// Reread and fresh arrays are disjoint, and named so: together they are every GET.
func TestRereadAndFreshGetsAddUpToAllGets(t *testing.T) {
	r, err := writeReport(fixtureRows(t, func(_, g []map[string]any, _ map[string]any) {
		g[1]["fresh"], g[1]["key"] = true, "m01-w1"
	}), writeCfg(), 104.5, 106.5)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for i := range r.RereadGets {
		total += r.RereadGets[i].Ops + r.FreshGets[i].Ops
	}
	if total != 3 {
		t.Fatalf("reread %+v fresh %+v", r.RereadGets, r.FreshGets)
	}
	b, _ := json.Marshal(r)
	if !strings.Contains(string(b), "reread_gets_before_outage_after") || strings.Contains(string(b), `"gets_before`) {
		t.Fatalf("report names: %s", b)
	}
}

func TestWriteReportToleratesCompletionsOutOfOrder(t *testing.T) {
	raw := fixtureRows(t, nil)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	lines[6], lines[7] = lines[7], lines[6]
	if _, err := writeReport([]byte(strings.Join(lines, "\n")), writeCfg(), 104.5, 106.5); err != nil {
		t.Fatal(err)
	}
}

func TestWriteReportRefusesEvidenceItCannotBind(t *testing.T) {
	for name, m := range map[string]func(s, g []map[string]any, e map[string]any){
		"count":             func(_, _ []map[string]any, e map[string]any) { e["sets"] = 11 },
		"get count":         func(_, _ []map[string]any, e map[string]any) { e["gets"] = 4 },
		"acked":             func(_, _ []map[string]any, e map[string]any) { e["acked"] = 10 },
		"failed":            func(_, _ []map[string]any, e map[string]any) { e["failed"] = 0 },
		"run":               func(_, _ []map[string]any, e map[string]any) { e["run_id"] = "other" },
		"stage":             func(_, _ []map[string]any, e map[string]any) { e["stage"] = "load" },
		"short phase":       func(_, _ []map[string]any, e map[string]any) { e["ended"] = 150.0 },
		"id reused":         func(s, _ []map[string]any, _ map[string]any) { s[3]["operation_id"] = 2 },
		"id out of range":   func(s, _ []map[string]any, _ map[string]any) { s[9]["operation_id"] = 10; s[9]["key"] = "m01-w10" },
		"get id reused":     func(_, g []map[string]any, _ map[string]any) { g[1]["operation_id"] = 0 },
		"reused key":        func(s, _ []map[string]any, _ map[string]any) { s[3]["key"] = s[2]["key"] },
		"seed key":          func(s, _ []map[string]any, _ map[string]any) { s[3]["key"] = "m01-0" },
		"out of order":      func(s, _ []map[string]any, _ map[string]any) { s[3]["started"] = 101.5; s[3]["ended"] = 101.6 },
		"backwards":         func(s, _ []map[string]any, _ map[string]any) { s[3]["ended"] = 1.0 },
		"set before span":   func(s, _ []map[string]any, _ map[string]any) { s[0]["started"] = 50.0 },
		"set after span":    func(s, _ []map[string]any, _ map[string]any) { s[9]["ended"] = 500.0 },
		"get outside span":  func(_, g []map[string]any, _ map[string]any) { g[2]["ended"] = 500.0 },
		"readback":          func(_, _ []map[string]any, e map[string]any) { e["readback_exact"] = 1 },
		"foreign get key":   func(_, g []map[string]any, _ map[string]any) { g[0]["key"] = "foreign-never-ACKed" },
		"unacked get key":   func(_, g []map[string]any, _ map[string]any) { g[0]["key"] = "m01-w5" },
		"get before ack":    func(_, g []map[string]any, _ map[string]any) { g[0]["key"] = "m01-w3" },
		"null set start":    func(s, _ []map[string]any, e map[string]any) { e["started"] = nil; s[0]["started"] = nil },
		"null set end":      func(s, _ []map[string]any, _ map[string]any) { s[1]["ended"] = nil },
		"absent set start":  func(s, _ []map[string]any, _ map[string]any) { delete(s[1], "started") },
		"null get start":    func(_, g []map[string]any, _ map[string]any) { g[1]["started"] = nil },
		"null end started":  func(_, _ []map[string]any, e map[string]any) { e["started"] = nil },
		"null end ended":    func(_, _ []map[string]any, e map[string]any) { e["ended"] = nil },
		"zero start":        func(s, _ []map[string]any, e map[string]any) { e["started"] = 0.0; e["ended"] = 220.0 },
		"negative readback": func(_, _ []map[string]any, e map[string]any) { e["readback_exact"] = 10; e["readback_missing"] = -1 },
	} {
		if _, err := writeReport(fixtureRows(t, m), writeCfg(), 104.5, 106.5); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestWriteReportRefusesDuplicateFields(t *testing.T) {
	raw := strings.Replace(string(fixtureRows(t, nil)), `"event":"SET",`, `"event":"SET","ok":false,`, 1)
	if _, err := writeReport([]byte(raw), writeCfg(), 104.5, 106.5); err == nil {
		t.Fatal("duplicate field accepted")
	}
	end := strings.Replace(string(fixtureRows(t, nil)), `"acked":9,`, `"acked":9,"acked":9,`, 1)
	if _, err := writeReport([]byte(end), writeCfg(), 104.5, 106.5); err == nil {
		t.Fatal("duplicate terminal field accepted")
	}
}

func TestWriteReportNeedsLoadSpanningTheRestart(t *testing.T) {
	for _, w := range [][2]float64{{50, 51}, {300, 301}, {106, 104}} {
		if _, err := writeReport(fixtureRows(t, nil), writeCfg(), w[0], w[1]); err == nil || !strings.Contains(err.Error(), "span") {
			t.Fatalf("window %v: %v", w, err)
		}
	}
}
