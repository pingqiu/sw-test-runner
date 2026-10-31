package seaweed

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
)

func checkSeed(rows []map[string]json.RawMessage, c SmokeConfig, route string) (map[string]any, error) {
	if route != "dc" || len(rows) != 2*c.Keys+1 {
		return nil, errors.New("settled seed ACK/exact rows missing")
	}
	for i := 0; i < c.Keys; i++ {
		key := fmt.Sprintf("%s-%d", c.Client, i)
		block := sha256.Sum256([]byte(c.RunID + "/" + key))
		h := sha256.New()
		for n := 0; n < c.Length; n += len(block) {
			h.Write(block[:min(len(block), c.Length-n)])
		}
		digest := fmt.Sprintf("%x", h.Sum(nil))
		for k, event := range []string{"ACK", "EXACT"} {
			var r struct {
				Event, Key, SHA256 string
				Length             int
				Sequence           uint64
				FIDs               []string
			}
			body, _ := json.Marshal(rows[2*i+k])
			if err := json.Unmarshal(body, &r); err != nil {
				return nil, err
			}
			if r.Event != event || r.Key != key || r.SHA256 != digest || r.Length != c.Length || (event == "ACK" && (r.Sequence != uint64(i+1) || len(r.FIDs) == 0)) {
				return nil, errors.New("seed ACK/exact digest differs")
			}
			if event == "ACK" {
				for _, fid := range r.FIDs {
					volume, needle, found := strings.Cut(fid, ",")
					vid, err := strconv.ParseUint(volume, 10, 64)
					if err != nil || vid == 0 || !found || needle == "" {
						return nil, errors.New("seed ACK full FID missing")
					}
				}
			}
		}
	}
	var last struct {
		Status     string
		ACK, Exact int
		Wrong      *int
		Witness    struct {
			ReadTransport string `json:"read_transport"`
			ReadBytes     int    `json:"read_bytes"`
			Fallback      *bool
			WriteBytes    int `json:"loader_write_bytes"`
		}
	}
	body, _ := json.Marshal(rows[len(rows)-1])
	if err := json.Unmarshal(body, &last); err != nil {
		return nil, err
	}
	if last.Status != "PASS" || last.ACK != c.Keys || last.Exact != c.Keys || last.Wrong == nil || *last.Wrong != 0 || last.Witness.ReadTransport != "dc" || last.Witness.ReadBytes != c.Keys*c.Length || last.Witness.WriteBytes != c.Keys*c.Length || last.Witness.Fallback == nil || *last.Witness.Fallback {
		return nil, errors.New("settled seed terminal proof differs")
	}
	if err := checkReadCounters(rows[len(rows)-1]["before"], rows[len(rows)-1]["after"], route, c.Keys, c.Length, 0, c.Keys); err != nil {
		return nil, err
	}
	var observed map[string]any
	err := json.Unmarshal(body, &observed)
	return observed, err
}

// ReadLoadArgs is the small CLI over the accepted worker/oracle leaves. Seed
// settles before load admission; load performs GETs only for a fixed 120 s.
func ReadLoadArgs(d *inventory.Document, client string, s SmokeSettings, stage, route string) ([]string, error) {
	if d == nil || d.Transport != "dc" || !slices.Contains([]string{"seed", "load", "recovery"}, stage) || !slices.Contains([]string{"dc", "http"}, route) || (stage == "seed" && route != "dc") {
		return nil, errors.New("unsupported read-only case stage/path")
	}
	args, err := SmokeArgs(d, client, s)
	if err != nil {
		return nil, err
	}
	return append(args, "--stage", stage, "--read-path", route), nil
}

func checkReadTask(j tr.JobPin, in tr.PackInputs, d *inventory.Document, client string, s SmokeSettings, stage, route string) error {
	want, err := ReadLoadArgs(d, client, s, stage, route)
	if err != nil {
		return err
	}
	if !slices.Equal(want, j.Argv) || j.Node != client || j.Executable != "/usr/bin/python3.12" || in.Files[client][j.Executable] != j.SHA256 {
		return errors.New("read task not exact joined/pinned CLI")
	}
	j.Argv = want[:len(want)-4]
	return checkSmoke(j, in, d, client, s)
}

// CheckReadTaskOutput checks the captured owned stdout, not an external receipt
// or self-reported file. Completed outage observations earn NO error credit
// here: the scenario must join each one to the exact witnessed outage.
func CheckReadTaskOutput(exit tr.ExitInfo, c SmokeConfig, stage, route string, window ...ReadWindow) (map[string]any, error) {
	if c.Keys < 1 || c.Keys > 16 || c.Length < 1 || c.Length > 4*1024*1024 || c.Transport != "dc" {
		return nil, errors.New("invalid read-only bounds")
	}
	if exit.Code != 0 || exit.Signal != 0 || !exit.Settled {
		return nil, errors.New("read task not cleanly settled")
	}
	raw, _, err := exit.Output.Decode()
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var rows []map[string]json.RawMessage
	for {
		var row json.RawMessage
		if err = dec.Decode(&row); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if len(rows) >= 2*c.Keys+1 {
			return nil, errors.New("excess read task rows")
		}
		scan := json.NewDecoder(bytes.NewReader(row))
		if err = uniqueJSON(scan, 0); err != nil {
			return nil, err
		}
		var fields map[string]json.RawMessage
		if err = json.Unmarshal(row, &fields); err != nil {
			return nil, err
		}
		rows = append(rows, fields)
	}
	if len(rows) == 0 {
		return nil, errors.New("read task output missing")
	}
	if stage == "seed" {
		return checkSeed(rows, c, route)
	}
	if stage != "load" && stage != "recovery" {
		return nil, errors.New("unsupported read stage")
	}
	if len(rows) != 1 {
		return nil, errors.New("read task terminal must be unique")
	}
	for _, name := range []string{"exact", "attempts", "faults", "elapsed", "started", "ended", "before", "after"} {
		if v, ok := rows[0][name]; !ok || string(v) == "null" {
			return nil, errors.New("read observation field missing")
		}
	}
	var r struct {
		Status, Stage, Transport string
		RunID                    string `json:"run_id"`
		Client, Root             string
		Exact, Attempts          int
		Started, Ended, Elapsed  float64
		FirstExact               *float64 `json:"first_exact"`
		LastExact                *float64 `json:"last_exact"`
		Faults                   []map[string]json.RawMessage
		Before, After            json.RawMessage
		Outcomes                 []GETOutcome
	}
	body, _ := json.Marshal(rows[0])
	if err = json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if r.Status != "OBSERVED" || r.Stage != stage || r.Transport != route || (route != "dc" && route != "http") || r.RunID != c.RunID || r.Client != c.Client || r.Root != c.Root || r.Exact < 0 || r.Attempts < 1 || r.Attempts > 1024 || r.Attempts != r.Exact+len(r.Faults) || len(r.Faults) > 256 || r.Started <= 0 || r.Ended < r.Started || r.Elapsed < 0 || r.Elapsed > 155 || math.Abs(r.Ended-r.Started-r.Elapsed) > 1 {
		return nil, errors.New("read task binding/count/time differs")
	}
	if stage == "recovery" && (r.Exact != c.Keys || len(r.Faults) != 0) {
		return nil, errors.New("named recovery GETs not exact")
	}
	if stage == "load" && r.Elapsed < 120 {
		return nil, errors.New("read load shorter than 120 seconds")
	}
	if r.Exact > 0 && (r.FirstExact == nil || r.LastExact == nil || *r.FirstExact < r.Started || *r.LastExact < *r.FirstExact || *r.LastExact > r.Ended) {
		return nil, errors.New("exact GET boundaries missing")
	}
	seen := map[int]bool{}
	for _, fault := range r.Faults {
		var f struct {
			Action, Key, Target, Transport string
			OperationID                    *int `json:"operation_id"`
			Started, Ended                 float64
			OK                             *bool
			Results                        json.RawMessage
		}
		b, _ := json.Marshal(fault)
		if err = json.Unmarshal(b, &f); err != nil {
			return nil, err
		}
		if f.OperationID == nil || *f.OperationID < 0 || *f.OperationID >= r.Attempts {
			return nil, errors.New("missing/out-of-range operation ID")
		}
		id := *f.OperationID
		if f.Action != "get" || f.Key != fmt.Sprintf("%s-%d", c.Client, id%c.Keys) || f.Target != c.RDMAAddress || f.Transport != route || seen[id] || f.OK == nil || (*f.OK && string(f.Results) != "[false]") || f.Started < r.Started || f.Ended < f.Started || f.Ended > r.Ended {
			return nil, errors.New("unbound completed GET failure")
		}
		seen[id] = true
	}
	if err = checkReadCounters(r.Before, r.After, route, r.Exact, c.Length, len(r.Faults), 0); err != nil {
		return nil, err
	}
	if len(window) > 0 {
		if err = checkWindow(r.Outcomes, seen, r.Attempts, r.Elapsed, c, window[0]); err != nil {
			return nil, err
		}
	}
	var observed map[string]any
	err = json.Unmarshal(body, &observed)
	return observed, err
}

func checkReadCounters(before, after json.RawMessage, route string, exact, length, faults, writes int) error {
	var a, b struct {
		ReadErrors *uint64 `json:"read_errors"`
		Loader     map[string]json.RawMessage
	}
	if json.Unmarshal(before, &a) != nil || json.Unmarshal(after, &b) != nil || a.ReadErrors == nil || b.ReadErrors == nil || *b.ReadErrors < *a.ReadErrors || *b.ReadErrors-*a.ReadErrors > uint64(faults) {
		return errors.New("missing/unattributed read errors")
	}
	transport := "rdma"
	selected := "direct_dc"
	if route == "http" {
		transport = "http"
		selected = "copied"
	}
	for _, stats := range []map[string]json.RawMessage{a.Loader, b.Loader} {
		var actual string
		if json.Unmarshal(stats["transport"], &actual) != nil || actual != transport {
			return errors.New("wrong loader transport")
		}
		if route == "dc" {
			var fallback *bool
			if json.Unmarshal(stats["http_fallback_allowed"], &fallback) != nil || fallback == nil || *fallback {
				return errors.New("DC fallback refused")
			}
		}
	}
	delta := func(name string) (uint64, error) {
		var x, y uint64
		if string(a.Loader[name]) == "null" || string(b.Loader[name]) == "null" || json.Unmarshal(a.Loader[name], &x) != nil || json.Unmarshal(b.Loader[name], &y) != nil || y < x {
			return 0, errors.New("missing/regressed path counter")
		}
		return y - x, nil
	}
	counts := map[string]uint64{}
	for _, name := range []string{"read_objects", "read_bytes", "direct_dc_reads", "direct_dc_bytes", "direct_rc_reads", "direct_rc_bytes", "copied_reads", "copied_bytes", "write_objects", "write_bytes"} {
		v, err := delta(name)
		if err != nil {
			return err
		}
		counts[name] = v
	}
	if counts["write_objects"] != uint64(writes) || counts["write_bytes"] != uint64(writes*length) || (writes > 0 && (counts["read_objects"] != uint64(exact) || counts["read_bytes"] != uint64(exact*length))) || counts["read_objects"] < uint64(exact) || counts["read_bytes"] < uint64(exact*length) || counts[selected+"_reads"] != counts["read_objects"] || counts[selected+"_bytes"] != counts["read_bytes"] {
		return errors.New("read-only path counters differ")
	}
	for _, other := range []string{"direct_dc", "direct_rc", "copied"} {
		if other != selected && (counts[other+"_reads"] != 0 || counts[other+"_bytes"] != 0) {
			return errors.New("foreign read path")
		}
	}
	return nil
}
