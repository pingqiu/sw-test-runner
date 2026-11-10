package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
)

// rowPlan is one admissible stage-3 row of the given kind (plan "Stage 3: RDMA and resource faults").
func rowPlan(kind string) *SoakPlan {
	role := "volume"
	if kind == KindClientKillPut {
		role = "client"
	}
	r := &FaultRow{RecoveryS: rowRecovery[kind], HoldS: rowHold[kind], AuditS: 20, QuiescenceS: 120,
		Refused: []string{"No space left on device"}, MetricsURL: "http://127.0.0.1:18080/metrics",
		Metrics: map[string]string{"released": "SeaweedFS_rdma_connections_released_total", "pinned": "SeaweedFS_rdma_range_pinned_bytes",
			"pool_bytes": "SeaweedFS_rdma_cold_pool_bytes", "cache_bytes": "SeaweedFS_rdma_write_staging_bytes"},
		PinBound:            PinBound{MaxInflightRanges: 4, LargestRangeBytes: 1 << 20, PoolBytes: ptr(int64(2 << 20)), CacheBytes: ptr(int64(1 << 20))},
		RetainedBudgetBytes: 4 << 20}
	switch kind {
	case KindControl, KindConnKill, KindDiskFull:
		r.HoldS = 10
	case KindLinkFlap:
		r.Netdev, r.RDMADevice, r.RDMAPort = "ens1f0", "mlx5_0", 1
	case KindMRPressure:
		r.MemlockBytes = 1 << 20
	case KindFDExhaust:
		r.NofileExtra = 4
	}
	switch kind {
	case KindConnKill:
		r.SetupPorts = []int{18888}
	case KindDiskFull:
		r.Mount, r.FreeBelowBytes, r.MaxBallastBytes = "/data/run/disk", 1<<20, 1<<30
	}
	return &SoakPlan{DurationS: 360, PacingS: 2, Faults: []SoakFault{{AtS: 60, Role: role, Episode: "E1", Kind: kind}}, Row: r,
		Layout: AssignLayout{Collection: "lmcache", Replication: "000", TTL: "2h"}, Pattern: rowPattern()}
}

// rowPattern is a fixed-size kv_load pattern: every object is object_bytes long.
func rowPattern() *LoadPattern {
	return &LoadPattern{Clients: 4, Transport: "dc", Keys: 64, Distribution: "uniform", ObjectBytes: 1 << 20, RatePerClient: 4,
		Phases: []LoadPhase{{Kind: "write_once"}, {Kind: "mixed", DurationS: 200, ReadRatio: 0.5}}}
}

var allRowKinds = []string{KindControl, KindVolumeKillIO, KindClientKillPut, KindLinkFlap, KindConnKill, KindMRPressure, KindDiskFull, KindFDExhaust}

func TestEveryRowKindIsAdmitted(t *testing.T) {
	for _, k := range allRowKinds {
		if err := rowPlan(k).checkScheduleWithin(DirectSoakHorizon); err != nil {
			t.Errorf("%s: %v", k, err)
		}
	}
}

func TestRowAdmissionRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		kind string
		edit func(*SoakPlan)
		want string
	}{
		"moved recovery bound":   {KindLinkFlap, func(x *SoakPlan) { x.Row.RecoveryS = 45 }, "frozen"},
		"F1 bound moved to 30":   {KindVolumeKillIO, func(x *SoakPlan) { x.Row.RecoveryS = 30 }, "frozen"},
		"short quiescence":       {KindControl, func(x *SoakPlan) { x.Row.QuiescenceS = 119 }, "frozen"},
		"soak client kill":       {KindControl, func(x *SoakPlan) { x.ClientKillAtS = 200 }, "client kill"},
		"F3 flap not 30 s":       {KindLinkFlap, func(x *SoakPlan) { x.Row.HoldS = 29 }, "hold"},
		"F5 hold not 60 s":       {KindMRPressure, func(x *SoakPlan) { x.Row.HoldS = 30 }, "hold"},
		"F1 with a hold":         {KindVolumeKillIO, func(x *SoakPlan) { x.Row.HoldS = 5 }, "hold"},
		"two faults":             {KindControl, func(x *SoakPlan) { x.Faults = append(x.Faults, x.Faults[0]) }, "exactly one fault"},
		"F2 on the volume":       {KindClientKillPut, func(x *SoakPlan) { x.Faults[0].Role = "volume" }, "role must be client"},
		"unknown kind":           {KindControl, func(x *SoakPlan) { x.Faults[0].Kind = "qp_kill" }, "kind unknown"},
		"restart with a row":     {KindControl, func(x *SoakPlan) { x.Faults[0].Kind = "" }, "kind unknown"},
		"kind without a row":     {KindControl, func(x *SoakPlan) { x.Row = nil }, "needs fault_row"},
		"F3 without its netdev":  {KindLinkFlap, func(x *SoakPlan) { x.Row.Netdev = "" }, "data netdev"},
		"F4 on the ssh port":     {KindConnKill, func(x *SoakPlan) { x.Row.SetupPorts = []int{22} }, "setup port"},
		"F6 relative mount":      {KindDiskFull, func(x *SoakPlan) { x.Row.Mount = "disk" }, "loop mount"},
		"F7 large margin":        {KindFDExhaust, func(x *SoakPlan) { x.Row.NofileExtra = 1000 }, "small"},
		"over 10 minutes":        {KindControl, func(x *SoakPlan) { x.DurationS = 420; x.Row.AuditS = 60 }, "10 minutes"},
		"unknown metric key":     {KindControl, func(x *SoakPlan) { x.Row.Metrics["leaks"] = "x" }, "metric"},
		"metrics url not http":   {KindControl, func(x *SoakPlan) { x.Row.MetricsURL = "file:///etc/passwd" }, "metrics url"},
		"no pin bound":           {KindControl, func(x *SoakPlan) { x.Row.PinBound = PinBound{} }, "pin bound"},
		"negative pool size":     {KindControl, func(x *SoakPlan) { x.Row.PinBound.PoolBytes = ptr(int64(-1)) }, "pin bound"},
		"huge cache size":        {KindControl, func(x *SoakPlan) { x.Row.PinBound.CacheBytes = ptr(int64(1 << 41)) }, "pin bound"},
		"fault without recovery": {KindMRPressure, func(x *SoakPlan) { x.Faults[0].AtS = 200 }, "no recovery"},
		"refused class empty":    {KindDiskFull, func(x *SoakPlan) { x.Row.Refused = []string{""} }, "refused class"},
		"soak mix, many sizes":   {KindVolumeKillIO, func(x *SoakPlan) { x.Pattern = nil }, "fixed-size kv_load"},
	} {
		x := rowPlan(c.kind)
		c.edit(x)
		if err := x.checkScheduleWithin(DirectSoakHorizon); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err=%v, want %q", name, err, c.want)
		}
	}
}

func TestExistingSoakPlansCarryNoRowBytes(t *testing.T) {
	p, _ := soakTestPlan()
	raw, err := json.Marshal(p.Soak)
	if err != nil || strings.Contains(string(raw), `"kind"`) || strings.Contains(string(raw), "fault_row") || p.Soak.rowMode() {
		t.Fatalf("an existing soak plan must marshal as before: %s %v", raw, err)
	}
	if !slicesEqual(p.Soak.criteria(), soakCriteria) {
		t.Fatal("an existing soak keeps its criteria")
	}
}

func ptr[T any](v T) *T { return &v }

// The configured pool and cache sizes reach the assessor as given; an absent one reaches it as null (NOT_COVERED
// there), never as 0.
func TestThePinSizesReachTheAssessorAsConfigured(t *testing.T) {
	d := &inventory.Document{RunID: "run-1", Hosts: []inventory.Host{{ID: "m01", RunRoot: "/r1"}},
		Observation: &inventory.Observation{Hosts: []inventory.HostObservation{{HostID: "m01", BootID: "b1"}, {HostID: "m02", BootID: "b2"}}}}
	for name, c := range map[string]struct {
		pool *int64
		want string
	}{"configured": {ptr(int64(2 << 20)), `"pin_pool_bytes":2097152`}, "absent": {nil, `"pin_pool_bytes":null`}} {
		x := rowPlan(KindControl)
		x.Row.PinBound.PoolBytes = c.pool
		argv, err := SoakAssessArgs(d, SmokeSettings{HelperDir: "/w"}, x)
		if err != nil || !strings.Contains(argv[3], c.want) || !strings.Contains(argv[3], `"pin_cache_bytes":1048576`) || !strings.Contains(argv[3], `"pin_ranges_bytes":4194304`) {
			t.Errorf("%s: %v %s", name, err, argv)
		}
	}
}

func slicesEqual(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

func TestARowNeedsExactlyOneVolumeOnM02(t *testing.T) {
	doc := func(m01Roles ...string) *inventory.Document {
		return &inventory.Document{RunID: "run-1", Hosts: []inventory.Host{{ID: "m01", RunRoot: "/r1", Roles: m01Roles},
			{ID: "m02", RunRoot: "/r2", Roles: []string{"master", "volume"}}}}
	}
	if _, err := SoakFaultArgs(doc("client"), SmokeSettings{HelperDir: "/w"}, rowPlan(KindVolumeKillIO), "apply"); err != nil {
		t.Fatal(err)
	}
	if _, err := SoakFaultArgs(doc("client", "volume"), SmokeSettings{HelperDir: "/w"}, rowPlan(KindVolumeKillIO), "apply"); err == nil {
		t.Fatal("F1 attributes PUTs to the one volume it kills: a second volume must refuse")
	}
}

func TestTheApprovedPathRefusesRows(t *testing.T) {
	p := Plan{Soak: rowPlan(KindLinkFlap)}
	if err := bindSoak(p, nil, tr.PackInputs{}, nil, time.Now()); err == nil || !strings.Contains(err.Error(), "only under seaweed-cloud direct") {
		t.Fatalf("err=%v", err)
	}
}

// rowFake is the runtime fake with an armed ack and a scripted injector.
type rowFake struct {
	*fakeSoak
	injects    []string
	unarmed    bool
	applyErr   error
	notRestore bool
}

func (r *rowFake) Ack(ctx context.Context, episode string) (SoakAck, error) {
	a, err := r.fakeSoak.Ack(ctx, episode)
	a.Armed = !r.unarmed
	a.PendingSets = append(a.PendingSets, struct {
		Op        string  `json:"op"`
		Submitted float64 `json:"submitted"`
	}{"42", a.ObservedMono - 0.5})
	a.InflightSets = 1
	return a, err
}

func (r *rowFake) Inject(ctx context.Context, action string) (json.RawMessage, error) {
	r.injects = append(r.injects, action)
	if action == "apply" {
		return json.RawMessage(`{"action":"apply"}`), r.applyErr
	}
	return json.RawMessage(`{"action":"restore","restored":` + map[bool]string{true: "false", false: "true"}[r.notRestore] + `}`), nil
}

func runRow(t *testing.T, kind string, r *rowFake) ([]FaultRecord, error) {
	t.Helper()
	p := Plan{Smoke: SmokeSettings{HelperDir: "/opt/profile/worker"}, Workload: []Step{{Host: "m01", Role: "client", Job: "load"}}, Soak: rowPlan(kind)}
	p.Soak.FaultReady = []Step{{Host: "m01", Job: "fault-ready-m01"}, {Host: "m02", Job: "fault-ready-m02"}}
	in := tr.PackInputs{Restarts: map[string]tr.GenerationRestartPlan{"E1": {Victim: "deploy-volume", Replacement: "replacement-volume"}},
		Files: map[string]map[string]string{"m02": {"/opt/profile/bin/weed-volume": strings.Repeat("d", 64)}}}
	r.exitAt = r.start + float64(p.Soak.DurationS) + QuiescenceReserve.Seconds() + float64(p.Soak.Row.AuditS) + 0.01
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	defer cancel()
	records, _, err := runSoak(ctx, r, p, in, map[string]tr.JobHandle{})
	return records, err
}

func TestRowInjectedFaultAppliesHoldsRestoresThenCloses(t *testing.T) {
	r := &rowFake{fakeSoak: newFakeSoak(t)}
	records, err := runRow(t, KindLinkFlap, r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.injects, ",") != "apply,restore" || len(r.restarts) != 0 || strings.Join(r.closes, ",") != "E1" || len(r.pauses) != 1 {
		t.Fatalf("injects=%v restarts=%v closes=%v", r.injects, r.restarts, r.closes)
	}
	rec := records[0]
	if rec.Kind != KindLinkFlap || len(rec.Injection) != 2 || rec.ReadyMono < rec.DispatchMono+30 || !rec.Ack.Armed {
		t.Fatalf("record %+v: the heal is after the 30 s hold", rec)
	}
}

func TestRowRestoreNotProvenIsCleanupUnknown(t *testing.T) {
	r := &rowFake{fakeSoak: newFakeSoak(t), notRestore: true}
	records, err := runRow(t, KindMRPressure, r)
	if err == nil || !strings.Contains(err.Error(), "CLEANUP_UNKNOWN") || len(r.closes) != 0 || records[0].Error == "" {
		t.Fatalf("err=%v closes=%v", err, r.closes)
	}
}

func TestRowApplyFailureStillRestores(t *testing.T) {
	r := &rowFake{fakeSoak: newFakeSoak(t), applyErr: errors.New("refused: port not ACTIVE")}
	_, err := runRow(t, KindLinkFlap, r)
	if err == nil || strings.Join(r.injects, ",") != "apply,restore" {
		t.Fatalf("err=%v injects=%v: a failed apply still runs its restore", err, r.injects)
	}
}

func TestRowUnarmedAckGetsNoInjection(t *testing.T) {
	r := &rowFake{fakeSoak: newFakeSoak(t), unarmed: true}
	_, err := runRow(t, KindFDExhaust, r)
	if err == nil || !strings.Contains(err.Error(), "not exact") || len(r.injects) != 0 {
		t.Fatalf("err=%v injects=%v", err, r.injects)
	}
}

func TestARestartRefusesAnArmedAck(t *testing.T) {
	r := &rowFake{fakeSoak: newFakeSoak(t)}
	p, in := soakTestPlan()
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	defer cancel()
	if _, _, err := runSoak(ctx, r, p, in, map[string]tr.JobHandle{}); err == nil || !strings.Contains(err.Error(), "not exact") || len(r.restarts) != 0 {
		t.Fatalf("err=%v restarts=%v: today's restart keeps its drained, unarmed ack", err, r.restarts)
	}
}

func TestRowF1RestartsUnderItsArmedWindow(t *testing.T) {
	r := &rowFake{fakeSoak: newFakeSoak(t)}
	records, err := runRow(t, KindVolumeKillIO, r)
	if err != nil || strings.Join(r.restarts, ",") != "E1" || len(r.injects) != 0 || records[0].Kind != KindVolumeKillIO {
		t.Fatalf("err=%v restarts=%v injects=%v", err, r.restarts, r.injects)
	}
}

func TestRowF2HasNoWindowAndNoRunnerEffect(t *testing.T) {
	r := &rowFake{fakeSoak: newFakeSoak(t)}
	records, err := runRow(t, KindClientKillPut, r)
	if err != nil || len(r.pauses) != 0 || len(r.restarts) != 0 || len(r.injects) != 0 || len(records) != 1 || records[0].Kind != KindClientKillPut {
		t.Fatalf("err=%v pauses=%v restarts=%v injects=%v records=%+v", err, r.pauses, r.restarts, r.injects, records)
	}
}

func rowVerdict(status string, outcomes map[string]string, extra string) string {
	crit := []string{}
	for _, c := range rowCriteria {
		o := outcomes[c]
		if o == "" {
			o = "PASS"
		}
		crit = append(crit, `"`+c+`":{"outcome":"`+o+`","reason":""}`)
	}
	return `{"status":"` + status + `","row":"F3","criteria":{` + strings.Join(crit, ",") + `},"claims":{},"unknown_attributed":0,` +
		`"resources":"ACCOUNTED","resource_classes":{"fds":"released"},"layout_scope":"` + rowLayoutScope + `",` + extra + `"binding":{"schema":"seaweed-soak-verdict/1","run_id":"run-1","boot_id":"boot-1","root":"/r"}}`
}

func TestRowVerdictDecodesOnlyItsWorstStatus(t *testing.T) {
	want := SoakBinding{SoakVerdictSchema, "run-1", "boot-1", "/r"}
	for name, c := range map[string]struct {
		row string
		ok  bool
	}{
		"pass":                      {rowVerdict("PASS", nil, ""), true},
		"not covered":               {rowVerdict("NOT_COVERED", map[string]string{"causal": "NOT_COVERED"}, ""), true},
		"fail over not covered":     {rowVerdict("FAIL", map[string]string{"causal": "NOT_COVERED", "audit": "FAIL"}, ""), true},
		"pass with a missing claim": {rowVerdict("PASS", map[string]string{"causal": "NOT_COVERED"}, ""), false},
		"not covered over a fail":   {rowVerdict("NOT_COVERED", map[string]string{"bytes": "FAIL"}, ""), false},
		"pass without its count":    {strings.Replace(rowVerdict("PASS", nil, ""), `"unknown_attributed":0,`, "", 1), false},
		"unknown claim":             {strings.Replace(rowVerdict("PASS", nil, ""), `"claims":{}`, `"claims":{"qp":"PASS"}`, 1), false},
		"soak resources form":       {strings.Replace(rowVerdict("PASS", nil, ""), `"ACCOUNTED"`, `"NOT_OBSERVED"`, 1), false},
		"no layout scope":           {strings.Replace(rowVerdict("PASS", nil, ""), `"layout_scope":"`+rowLayoutScope+`",`, "", 1), false},
		"wider layout scope":        {strings.Replace(rowVerdict("PASS", nil, ""), rowLayoutScope, "any layout", 1), false},
	} {
		_, err := decodeVerdict(captured([]byte(c.row)), want, rowCriteria)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%v", name, err, c.ok)
		}
	}
	if _, err := decodeVerdict(captured([]byte(rowVerdict("PASS", nil, ""))), want, soakCriteria); err == nil {
		t.Error("a row verdict must not decode as the soak's")
	}
}
