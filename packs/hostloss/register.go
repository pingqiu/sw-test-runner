// Package hostloss registers the host-loss drill's judge actions. Each action reads collected artifact files on the
// controller (never a host), runs one pure judge from internal/hostloss, writes a row receipt next to its first input
// and saves verdict/counted/reason as outputs.
//
// Action status: verdict FAIL -> action FAIL; verdict NOT_COUNTED -> action FAIL unless the step sets
// allow_not_counted: "true" (rehearsal templates), then the action passes and the receipt carries NOT_COUNTED.
// Counting reads receipts, never action status.
package hostloss

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	tr "github.com/pingqiu/sw-test-runner"
	hl "github.com/pingqiu/sw-test-runner/internal/hostloss"
)

// RegisterPack registers the judge actions. None mutates anything but its own receipt file.
func RegisterPack(r *tr.Registry) {
	r.RegisterFunc("hostloss_row1", tr.TierBlock, row1Action)
	r.RegisterFunc("hostloss_final", tr.TierBlock, finalAction)
	r.RegisterFunc("hostloss_image", tr.TierBlock, imageAction)
	r.RegisterFunc("hostloss_lostrange", tr.TierBlock, lostRangeAction)
	r.RegisterFunc("hostloss_stopbound", tr.TierBlock, stopBoundAction)
}

// Evidence names one input artifact by path and content hash.
type Evidence struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Receipt is the structured result of one judged row.
type Receipt struct {
	Row      string            `json:"row"`
	Verdict  hl.Outcome        `json:"verdict"`
	Reason   string            `json:"reason"`
	Counted  bool              `json:"counted"`
	Evidence []Evidence        `json:"evidence"`
	Values   map[string]string `json:"values,omitempty"`
}

type inputs struct {
	ev []Evidence
}

// read returns an artifact's bytes and records its path and hash as evidence.
func (in *inputs) read(act tr.Action, key string) ([]byte, error) {
	p := act.Params[key]
	if p == "" {
		return nil, fmt.Errorf("%s: missing %s", act.Action, key)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("%s: %s: %w", act.Action, key, err)
	}
	s := sha256.Sum256(b)
	in.ev = append(in.ev, Evidence{Path: p, SHA256: hex.EncodeToString(s[:])})
	return b, nil
}

func intParam(act tr.Action, key string, def int64) (int64, error) {
	v := act.Params[key]
	if v == "" {
		if def < 0 {
			return 0, fmt.Errorf("%s: missing %s", act.Action, key)
		}
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s: %s=%q is not a non-negative integer", act.Action, key, v)
	}
	return n, nil
}

// finish writes the receipt and maps the verdict onto the action result.
func finish(actx *tr.ActionContext, act tr.Action, in *inputs, row hl.Row, values map[string]string) (map[string]string, error) {
	rc := Receipt{Row: row.Name, Verdict: row.Outcome, Reason: row.Text, Counted: row.Outcome == hl.Pass, Evidence: in.ev, Values: values}
	path := act.Params["receipt"]
	if path == "" && len(in.ev) > 0 {
		path = filepath.Join(filepath.Dir(in.ev[0].Path), row.Name+".receipt.json")
	}
	out := map[string]string{"verdict": string(row.Outcome), "counted": strconv.FormatBool(rc.Counted), "reason": row.Text, "receipt": path}
	for k, v := range values {
		out[k] = v
	}
	if path != "" {
		b, err := json.MarshalIndent(rc, "", "  ")
		if err == nil {
			err = os.WriteFile(path, append(b, '\n'), 0o644)
		}
		if err != nil {
			return out, fmt.Errorf("%s: receipt: %w", act.Action, err)
		}
	}
	if actx != nil && actx.Log != nil {
		actx.Log("%s", row.String())
	}
	switch {
	case row.Outcome == hl.Fail:
		return out, fmt.Errorf("%s", row.String())
	case row.Outcome == hl.NotCounted && act.Params["allow_not_counted"] != "true":
		return out, fmt.Errorf("%s (allow_not_counted is not set)", row.String())
	}
	return out, nil
}

// hostloss_row1: ledger = the live ledger snapshot (only complete lines are judged).
func row1Action(_ context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	var in inputs
	b, err := in.read(act, "ledger")
	if err != nil {
		return nil, err
	}
	s := hl.ScanLive(b)
	return finish(actx, act, &in, hl.JudgeRow1(s), map[string]string{"acked": strconv.Itoa(s.Acked)})
}

// hostloss_final: ledger = the whole ledger after the writer was proven stopped; row1_receipt = hostloss_row1's
// receipt; at most one of kill_ns (the kill receipt's host kill-issued ns) or fence_sec (the old owner's exit line,
// whole seconds) sets the race window of the one write that may lack an LSN.
func finalAction(_ context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	var in inputs
	b, err := in.read(act, "ledger")
	if err != nil {
		return nil, err
	}
	rb, err := in.read(act, "row1_receipt")
	if err != nil {
		return nil, err
	}
	var r1 Receipt
	if err := json.Unmarshal(rb, &r1); err != nil || r1.Row != "d1-lsn-exact" {
		return nil, fmt.Errorf("%s: row1_receipt is not a d1-lsn-exact receipt", act.Action)
	}
	row1, err := strconv.Atoi(r1.Values["acked"])
	if err != nil {
		return nil, fmt.Errorf("%s: row1_receipt carries no acked count", act.Action)
	}
	if act.Params["kill_ns"] != "" && act.Params["fence_sec"] != "" {
		return nil, fmt.Errorf("%s: kill_ns and fence_sec are exclusive", act.Action)
	}
	var race *hl.Window
	if act.Params["kill_ns"] != "" {
		k, err := intParam(act, "kill_ns", -1)
		if err != nil {
			return nil, err
		}
		race = hl.RaceBefore(k)
	} else if act.Params["fence_sec"] != "" {
		f, err := intParam(act, "fence_sec", -1)
		if err != nil {
			return nil, err
		}
		race = hl.RaceAroundSecond(f)
	}
	s := hl.ScanFinal(b, race)
	return finish(actx, act, &in, hl.JudgeFinal(s, row1, race), map[string]string{"acked": strconv.Itoa(s.Acked), "raced": strconv.FormatBool(s.Raced)})
}

// hostloss_image: ledger + image (a full read of the volume or a restore) judged by LSN at boundary L.
func imageAction(_ context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	var in inputs
	lb, err := in.read(act, "ledger")
	if err != nil {
		return nil, err
	}
	img, err := in.read(act, "image")
	if err != nil {
		return nil, err
	}
	L, err := intParam(act, "L", -1)
	if err != nil {
		return nil, err
	}
	blocks, err := intParam(act, "blocks", -1)
	if err != nil {
		return nil, err
	}
	bs, err := intParam(act, "bs", 4096)
	if err != nil || bs == 0 {
		return nil, fmt.Errorf("%s: bad bs", act.Action)
	}
	l, err := hl.ParseLedger(lb)
	if err != nil {
		return nil, fmt.Errorf("%s: ledger: %w", act.Action, err)
	}
	name := act.Params["row"]
	if name == "" {
		name = "d7-f5"
	}
	return finish(actx, act, &in, hl.JudgeImage(name, l, bytes.NewReader(img), L, int(blocks), int(bs)), nil)
}

// hostloss_lostrange: grant = the force-move output, range = the lost-range readout, request = the move's id.
func lostRangeAction(_ context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	var in inputs
	g, err := in.read(act, "grant")
	if err != nil {
		return nil, err
	}
	rg, err := in.read(act, "range")
	if err != nil {
		return nil, err
	}
	row, lr := hl.JudgeLostRange(string(g), string(rg), act.Params["request"])
	var v map[string]string
	if lr != nil {
		v = map[string]string{"L": strconv.FormatInt(lr.L, 10), "old_epoch": strconv.FormatInt(lr.OldEpoch, 10), "new_epoch": strconv.FormatInt(lr.NewEpoch, 10)}
	}
	return finish(actx, act, &in, row, v)
}

// hostloss_stopbound: ledger + journal (the old owner's unit journal) of a partition rep.
func stopBoundAction(_ context.Context, actx *tr.ActionContext, act tr.Action) (map[string]string, error) {
	var in inputs
	lb, err := in.read(act, "ledger")
	if err != nil {
		return nil, err
	}
	j, err := in.read(act, "journal")
	if err != nil {
		return nil, err
	}
	l, err := hl.ParseLedger(lb)
	if err != nil {
		return nil, fmt.Errorf("%s: ledger: %w", act.Action, err)
	}
	return finish(actx, act, &in, hl.JudgeStopBound(l, string(j)), nil)
}
