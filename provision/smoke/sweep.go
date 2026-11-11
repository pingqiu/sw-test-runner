package smoke

import (
	"context"
	"encoding/json"
	"sort"
	"time"
)

// The scheduled sweep: the recovery envelope for a controller that died before its own destroy. It lists the one
// dedicated resource group, and per run tag deletes only what the runner made (managed_by=sw-test-runner) whose
// expires_at (the run's hard deadline) has passed and whose run has no live controller. Everything else is reported
// and left. Deletion is sweepOrphans: the same tag/scope re-check, type order, re-read and wait as the controller's
// own sweep.

const sweepSchema = "seaweed.cloud-sweep/v1"

// SweptRun is one run tag's outcome.
type SweptRun struct {
	Run       string   `json:"run"`
	ExpiresAt string   `json:"expires_at,omitempty"`
	Found     []string `json:"found"`
	Action    string   `json:"action"` // "deleted", "would-delete" (dry run) or "left"
	Reason    string   `json:"reason,omitempty"`
	Gone      []string `json:"verified_gone,omitempty"`
	Left      []string `json:"left,omitempty"`
}

// SweepReceipt is one sweep's whole record.
type SweepReceipt struct {
	Schema       string     `json:"schema"`
	At           string     `json:"at"`
	DryRun       bool       `json:"dry_run"`
	Subscription string     `json:"subscription"`
	Group        string     `json:"resource_group"`
	Runs         []SweptRun `json:"runs"`
	Untagged     []string   `json:"untagged_left"` // not the runner's: never touched
	Error        string     `json:"error,omitempty"`
}

// Acted reports whether the sweep found anything of the runner's, or failed: the cases a person must hear about.
func (r SweepReceipt) Acted() bool { return len(r.Runs) > 0 || r.Error != "" }

type sweepRow struct {
	ID, Type string
	Tags     map[string]string
	raw      json.RawMessage // the row exactly as listed, for the scope check
}

// Sweep lists the group once, decides per run tag, then deletes what may be deleted and re-lists to verify.
// live reports whether a run still has a controller; it must answer true when it cannot tell.
func Sweep(ctx context.Context, subscription, group string, now time.Time, dry bool, live func(run string) bool,
	call func(context.Context, string, ...string) ([]byte, error)) (out SweepReceipt) {
	out = SweepReceipt{Schema: sweepSchema, At: now.UTC().Format(time.RFC3339), DryRun: dry, Subscription: subscription, Group: group, Runs: []SweptRun{}, Untagged: []string{}}
	list := func() ([]sweepRow, error) {
		b, err := call(ctx, "az", "resource", "list", "--subscription", subscription, "--resource-group", group, "--output", "json", "--only-show-errors")
		if err != nil {
			return nil, err
		}
		if _, err = resourceIDs(b, "", true); err != nil { // the strict decision-key check, before any decision
			return nil, err
		}
		var raws []json.RawMessage
		if err = json.Unmarshal(b, &raws); err != nil {
			return nil, err
		}
		rows := make([]sweepRow, len(raws))
		for i, raw := range raws {
			if err = json.Unmarshal(raw, &rows[i]); err != nil {
				return nil, err
			}
			rows[i].raw = raw
		}
		return rows, nil
	}
	rows, err := list()
	if err != nil {
		out.Error = "list: " + err.Error()
		return out
	}
	byRun := map[string][]sweepRow{}
	for _, r := range rows {
		if run := r.Tags["seaweed_run"]; run != "" {
			byRun[run] = append(byRun[run], r)
		} else {
			out.Untagged = append(out.Untagged, r.ID)
		}
	}
	sort.Strings(out.Untagged)
	runs := []string{}
	for run := range byRun {
		runs = append(runs, run)
	}
	sort.Strings(runs)
	for _, run := range runs {
		s := SweptRun{Run: run, Found: []string{}, Action: "left"}
		// Every row must carry its own readable expires_at, all equal; one missing or unreadable row leaves the run.
		expires, seen := "", false
		var deadline time.Time
		for _, r := range byRun[run] {
			s.Found = append(s.Found, r.ID)
			v, ok := r.Tags["expires_at"]
			t, perr := time.Parse(time.RFC3339Nano, v)
			switch {
			case s.Reason != "":
			case r.Tags["managed_by"] != "sw-test-runner":
				s.Reason = "a resource with this run tag is not managed_by=sw-test-runner"
			case !ok || perr != nil:
				s.Reason = "expires_at missing or unreadable on " + r.ID
			case !seen:
				expires, deadline, seen = v, t, true
			case v != expires:
				s.Reason = "the run's resources disagree on expires_at"
			}
		}
		sort.Strings(s.Found)
		s.ExpiresAt = expires
		switch {
		case s.Reason != "":
		case !now.After(deadline):
			s.Reason = "not expired"
		case live(run):
			s.Reason = "controller live (or unknown)"
		}
		if s.Reason == "" {
			ids, e := orphanIDs(mustJSON(byRun[run]), subscription, group, run)
			switch {
			case e != nil:
				s.Reason = "out of scope: " + e.Error()
			case dry:
				s.Action, s.Left = "would-delete", ids
			default:
				gone, e := sweepOrphans(ctx, subscription, group, run, call)
				s.Action, s.Gone = "deleted", gone
				if e != nil {
					s.Action, s.Reason = "left", "delete: "+e.Error()
				}
			}
		}
		out.Runs = append(out.Runs, s)
	}
	if dry {
		return out
	}
	// Verify: nothing of a deleted run may remain on a fresh list.
	after, err := list()
	if err != nil {
		out.Error = "verify list: " + err.Error()
		return out
	}
	for i := range out.Runs {
		if out.Runs[i].Action != "deleted" {
			out.Runs[i].Left = out.Runs[i].Found
			continue
		}
		for _, r := range after {
			if r.Tags["seaweed_run"] == out.Runs[i].Run {
				out.Runs[i].Left = append(out.Runs[i].Left, r.ID)
			}
		}
		if len(out.Runs[i].Left) > 0 {
			out.Runs[i].Action, out.Runs[i].Reason = "left", "still listed after delete"
		}
	}
	return out
}

// mustJSON is the run's rows as listed, so the scope check sees exactly what Azure returned.
func mustJSON(rows []sweepRow) []byte {
	raws := make([]json.RawMessage, len(rows))
	for i, r := range rows {
		raws[i] = r.raw
	}
	b, _ := json.Marshal(raws)
	return b
}
