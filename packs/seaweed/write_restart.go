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
	Sets   int     `json:"sets"`
	Failed int     `json:"failed"`
	P50S   float64 `json:"p50_s"`
	MaxS   float64 `json:"max_s"`
}

type WriteError struct {
	OperationID int     `json:"operation_id"`
	AtS         float64 `json:"at_s"` // after the pre-Kill mark
	Error       string  `json:"error"`
}

// WriteReport describes SETs around a master restart. It judges nothing: no
// threshold is known yet, so Verdict is always REPORT_ONLY.
type WriteReport struct {
	Verdict            string       `json:"verdict"`
	Before             WriteWindow  `json:"before_kill"`
	Outage             WriteWindow  `json:"kill_to_ready"`
	After              WriteWindow  `json:"after_ready"`
	KillToReadyS       float64      `json:"kill_to_ready_s"`
	FirstAckAfterKillS float64      `json:"first_ack_after_kill_s"`
	Errors             []WriteError `json:"errors"`
	ErrorsDropped      int          `json:"errors_dropped"`
	Acked              int          `json:"acked"`
	ReadbackExact      int          `json:"readback_exact"`
	ReadbackMissing    int          `json:"readback_missing"`
}

const maxWriteErrors = 16

// writeReport binds the worker's rows to this run and splits them at the
// pre-Kill mark and at READY, both on the M01 clock that wrote the rows.
func writeReport(raw []byte, c SmokeConfig, preKill, ready float64) (WriteReport, error) {
	r := WriteReport{Verdict: "REPORT_ONLY", KillToReadyS: ready - preKill}
	if preKill <= 0 || ready <= preKill {
		return r, errors.New("write load does not span the restart: kill/READY order")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var lat [3][]float64
	windows := [3]*WriteWindow{&r.Before, &r.Outage, &r.After}
	var end *struct {
		Status, Stage, Transport, Client, Root string
		RunID                                  string `json:"run_id"`
		Sets, Acked, Failed                    int
		ReadbackExact                          int `json:"readback_exact"`
		ReadbackMissing                        int `json:"readback_missing"`
		Started, Ended                         float64
	}
	sets, acked, prior, first, last := 0, 0, 0., 0., 0.
	for {
		var body json.RawMessage
		err := dec.Decode(&body)
		if err == io.EOF {
			break
		}
		if err != nil {
			return r, err
		}
		if end != nil {
			return r, errors.New("write load rows after the terminal")
		}
		var row struct {
			Event, Key, Error string
			OperationID       *int `json:"operation_id"`
			Started, Ended    float64
			OK                *bool
		}
		if err = json.Unmarshal(body, &row); err != nil {
			return r, err
		}
		if row.Event != "SET" {
			if err = json.Unmarshal(body, &end); err != nil {
				return r, err
			}
			continue
		}
		if row.OperationID == nil || *row.OperationID != sets || row.OK == nil || row.Key != c.Client+"-w"+strconv.Itoa(sets) {
			return r, errors.New("SET row binding differs")
		}
		if row.Ended < row.Started || row.Started < prior {
			return r, errors.New("SET rows not serial")
		}
		prior, sets = row.Ended, sets+1
		if first == 0 {
			first = row.Ended
		}
		last = row.Started
		w := 0
		if row.Started >= preKill {
			w = 1
		}
		if row.Started >= ready {
			w = 2
		}
		windows[w].Sets++
		lat[w] = append(lat[w], row.Ended-row.Started)
		if *row.OK {
			acked++
			if row.Started >= preKill && r.FirstAckAfterKillS == 0 {
				r.FirstAckAfterKillS = math.Round((row.Ended-preKill)*1e3) / 1e3
			}
			continue
		}
		windows[w].Failed++
		if len(r.Errors) == maxWriteErrors {
			r.ErrorsDropped++
			continue
		}
		r.Errors = append(r.Errors, WriteError{*row.OperationID, row.Started - preKill, row.Error})
	}
	if end == nil || end.Status != "OBSERVED" || end.Stage != "write-load" || end.Transport != "dc" || end.RunID != c.RunID || end.Client != c.Client || end.Root != c.Root {
		return r, errors.New("write load terminal binding differs")
	}
	if end.Sets != sets || end.Acked != acked || end.Failed != sets-acked || end.ReadbackExact+end.ReadbackMissing != acked || end.Started > end.Ended {
		return r, errors.New("write load counts differ")
	}
	if sets == 0 || first >= preKill || last <= ready {
		return r, errors.New("write load does not span the restart")
	}
	r.Acked, r.ReadbackExact, r.ReadbackMissing = acked, end.ReadbackExact, end.ReadbackMissing
	for i, l := range lat {
		if len(l) > 0 {
			slices.Sort(l)
			windows[i].P50S, windows[i].MaxS = l[len(l)/2], l[len(l)-1]
		}
	}
	return r, nil
}

// writeRemedy carries the report in the pack result. A worker that exited
// non-zero (an unknown completion, wrong bytes) fails the scenario.
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
