package seaweed

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"slices"
	"strconv"

	tr "github.com/pingqiu/sw-test-runner"
)

// WriteWindow is one span of the report. Latencies are seconds on the client clock.
type WriteWindow struct {
	Ops    int     `json:"ops"`
	Failed int     `json:"failed"`
	P50S   float64 `json:"p50_s"`
	MaxS   float64 `json:"max_s"`
}

type WriteError struct {
	Event       string  `json:"event"`
	OperationID int     `json:"operation_id"`
	AtS         float64 `json:"at_s"` // after the pre-Kill mark
	Error       string  `json:"error"`
}

// WriteReport describes SETs and GETs around a master restart. It judges
// nothing: no threshold is known yet, so Verdict is always REPORT_ONLY.
type WriteReport struct {
	Verdict            string         `json:"verdict"`
	Sets               [3]WriteWindow `json:"sets_before_outage_after"`
	Gets               [3]WriteWindow `json:"gets_before_outage_after"`
	KillToReadyS       float64        `json:"kill_to_ready_s"`
	FirstAckAfterKillS float64        `json:"first_ack_after_kill_s"`
	Errors             []WriteError   `json:"errors"`
	ErrorsDropped      int            `json:"errors_dropped"`
	Acked              int            `json:"acked"`
	ReadbackExact      int            `json:"readback_exact"`
	ReadbackMissing    int            `json:"readback_missing"`
}

const (
	maxWriteErrors   = 16
	writeLoadSeconds = 120
)

type writeRow struct {
	Event, Key, Error string
	OperationID       *int `json:"operation_id"`
	Started, Ended    float64
	OK                *bool
}

type writeEnd struct {
	Status, Stage, Transport, Client, Root string
	RunID                                  string `json:"run_id"`
	Sets, Gets, Acked, Failed              int
	ReadbackExact                          int `json:"readback_exact"`
	ReadbackMissing                        int `json:"readback_missing"`
	Started, Ended                         float64
}

// writeRows strictly decodes the stdout: unique fields, every row bound by
// shape, exactly one terminal, nothing after it.
func writeRows(raw []byte) ([]writeRow, *writeEnd, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	rows, end := []writeRow{}, (*writeEnd)(nil)
	for {
		var body json.RawMessage
		err := dec.Decode(&body)
		if err == io.EOF {
			return rows, end, nil
		}
		if err != nil {
			return nil, nil, err
		}
		if end != nil {
			return nil, nil, errors.New("write load rows after the terminal")
		}
		if err = uniqueJSON(json.NewDecoder(bytes.NewReader(body)), 0); err != nil {
			return nil, nil, err
		}
		var row writeRow
		if err = json.Unmarshal(body, &row); err != nil {
			return nil, nil, err
		}
		if row.Event == "SET" || row.Event == "GET" {
			rows = append(rows, row)
			continue
		}
		if err = json.Unmarshal(body, &end); err != nil {
			return nil, nil, err
		}
	}
}

// writeReport binds the worker's rows to this run and splits them at the
// pre-Kill mark and at READY, both on the M01 clock that wrote the rows.
func writeReport(raw []byte, c SmokeConfig, preKill, ready float64) (WriteReport, error) {
	r := WriteReport{Verdict: "REPORT_ONLY", KillToReadyS: ready - preKill, FirstAckAfterKillS: -1}
	if preKill <= 0 || ready <= preKill {
		return r, errors.New("write load does not span the restart: kill/READY order")
	}
	rows, end, err := writeRows(raw)
	if err != nil {
		return r, err
	}
	if end == nil || end.Status != "OBSERVED" || end.Stage != "write-load" || end.Transport != "dc" || end.RunID != c.RunID || end.Client != c.Client || end.Root != c.Root {
		return r, errors.New("write load terminal binding differs")
	}
	if end.Ended-end.Started < writeLoadSeconds || end.Sets < 1 || end.Gets < 0 || end.ReadbackExact < 0 || end.ReadbackMissing < 0 {
		return r, errors.New("write load phase or totals out of bounds")
	}
	seen := map[string]map[int]float64{"SET": {}, "GET": {}}
	acked, failed := 0, 0
	first, last := math.Inf(1), math.Inf(-1)
	var lat [2][3][]float64
	for _, row := range rows {
		kind := 0
		if row.Event == "GET" {
			kind = 1
		}
		if row.OperationID == nil || row.OK == nil || *row.OperationID < 0 {
			return r, errors.New("load row binding differs")
		}
		id := *row.OperationID
		if _, dup := seen[row.Event][id]; dup {
			return r, errors.New("load row operation reused")
		}
		seen[row.Event][id] = row.Started
		if row.Event == "SET" && row.Key != c.Client+"-w"+strconv.Itoa(id) {
			return r, errors.New("SET row key differs")
		}
		if row.Started < end.Started || row.Ended < row.Started || row.Ended > end.Ended {
			return r, errors.New("load row outside the terminal span")
		}
		w := 0
		if row.Started >= preKill {
			w = 1
		}
		if row.Started >= ready {
			w = 2
		}
		window := &r.Sets[w]
		if kind == 1 {
			window = &r.Gets[w]
		}
		window.Ops++
		lat[kind][w] = append(lat[kind][w], row.Ended-row.Started)
		if kind == 0 {
			first, last = math.Min(first, row.Ended), math.Max(last, row.Started)
		}
		if *row.OK {
			if kind == 0 {
				acked++
				if row.Ended >= preKill {
					at := row.Ended - preKill
					if r.FirstAckAfterKillS < 0 || at < r.FirstAckAfterKillS {
						r.FirstAckAfterKillS = at
					}
				}
			}
			continue
		}
		window.Failed++
		if kind == 0 {
			failed++
		}
		if len(r.Errors) == maxWriteErrors {
			r.ErrorsDropped++
			continue
		}
		r.Errors = append(r.Errors, WriteError{row.Event, id, row.Started - preKill, row.Error})
	}
	for name, n := range map[string]int{"SET": end.Sets, "GET": end.Gets} {
		if len(seen[name]) != n {
			return r, errors.New("load row counts differ")
		}
		for id := range seen[name] {
			if id >= n {
				return r, errors.New("load row operation out of range")
			}
		}
	}
	for id := 1; id < end.Sets; id++ {
		if seen["SET"][id] < seen["SET"][id-1] {
			return r, errors.New("SET rows not in submission order")
		}
	}
	if end.Acked != acked || end.Failed != failed || end.ReadbackExact+end.ReadbackMissing != acked {
		return r, errors.New("write load counts differ")
	}
	if first >= preKill || last <= ready {
		return r, errors.New("write load does not span the restart")
	}
	r.Acked, r.ReadbackExact, r.ReadbackMissing = acked, end.ReadbackExact, end.ReadbackMissing
	for kind, spans := range lat {
		for i, l := range spans {
			if len(l) == 0 {
				continue
			}
			slices.Sort(l)
			window := &r.Sets[i]
			if kind == 1 {
				window = &r.Gets[i]
			}
			window.P50S, window.MaxS = l[len(l)/2], l[len(l)-1]
		}
	}
	return r, nil
}

// writeRemedy carries the report in the pack result. A worker that exited
// non-zero is failed by the core's recordExit/Wait first; this keeps the same
// contract as CheckReadTaskOutput for any exit it hands back.
func writeRemedy(e tr.ExitInfo, c SmokeConfig, preKill, ready float64) (string, error) {
	if e.Code != 0 || e.Signal != 0 || !e.Settled {
		return "", errors.New("write load not cleanly settled")
	}
	raw, _, err := e.Output.Decode()
	if err != nil {
		return "", err
	}
	r, err := writeReport(raw, c, preKill, ready)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(r)
	return "report only, no pass threshold: " + string(b), err
}
