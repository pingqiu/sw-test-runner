package seaweed

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"reflect"
	"slices"
	"strings"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/inventory"
)

// SoakVerdictSchema names the assessor's config and row contract.
const SoakVerdictSchema = "seaweed-soak-verdict/1"

// soakCriteria is the exact mandatory set (soak_assess.MANDATORY): a PASS
// carries every one of them and nothing else.
var soakCriteria = []string{"wrong_reads", "failed_sets", "get_outcomes", "terminal", "fault_errors", "misses", "coverage",
	"body", "client_kill", "rotation", "max_vid_advanced", "routes"}

// kvLoadCriteria is kv_load's set (soak_assess.KV_LOAD): the pattern's
// write_once and load_as_declared replace the soak's coverage, rotation and
// master-fault rows, and misses are judged outside the fault windows only.
var kvLoadCriteria = []string{"wrong_reads", "failed_sets", "get_outcomes", "terminal", "fault_errors", "misses", "write_once",
	"body", "client_kill", "load_as_declared", "routes"}

// rowCriteria is a stage-3 row's set (soak_assess.ROW_CRITERIA): the plan's five acceptance items, with the
// write contract and its mandatory audit apart from bytes.
var rowCriteria = []string{"bytes", "write_contract", "audit", "failure_mode", "recovery", "resources", "causal"}

// criteria is the mandatory set the plan's assessor must return.
func (x *SoakPlan) criteria() []string {
	if x.rowMode() {
		return rowCriteria
	}
	if x != nil && x.Pattern != nil {
		return kvLoadCriteria
	}
	return soakCriteria
}

// SoakBinding is what the assessor was told to judge, echoed in its row.
type SoakBinding struct {
	Schema string `json:"schema"`
	RunID  string `json:"run_id"`
	BootID string `json:"boot_id"`
	Root   string `json:"root"`
}

// observedBoot is the admitted boot of a host from the resolved observation.
func observedBoot(d *inventory.Document, host string) string {
	if d.Observation != nil {
		for _, h := range d.Observation.Hosts {
			if h.HostID == host {
				return h.BootID
			}
		}
	}
	return ""
}

func soakBinding(d *inventory.Document) (SoakBinding, error) {
	b := SoakBinding{Schema: SoakVerdictSchema, RunID: d.RunID, BootID: observedBoot(d, "m01"), Root: runRoot(d, "m01")}
	if b.RunID == "" || b.BootID == "" || b.Root == "" {
		return b, errors.New("soak assessor needs the run id, the admitted M01 boot and the M01 run root")
	}
	return b, nil
}

// SoakAssessArgs is the exact soak_assess.py argv for the M01 verdict job.
func SoakAssessArgs(d *inventory.Document, s SmokeSettings, x *SoakPlan) ([]string, error) {
	b, err := soakBinding(d)
	if err != nil {
		return nil, err
	}
	m02 := observedBoot(d, "m02")
	if m02 == "" {
		return nil, errors.New("soak assessor needs the admitted M02 boot")
	}
	config := map[string]any{"schema": b.Schema, "root": b.Root, "run_id": b.RunID, "boot_id": b.BootID, "m02_boot_id": m02,
		"duration_s": x.DurationS, "quiescence_s": int(QuiescenceReserve.Seconds()), "client_kill_at_s": x.ClientKillAtS,
		"collection": x.Layout.Collection}
	if x.Pattern != nil {
		config["pattern"], config["faults"] = x.Pattern, x.Faults // kv_load: judged by its own rows; absent for the soak
	}
	if x.rowMode() { // stage 3: the row's frozen values; absent otherwise
		f, r := x.Faults[0], x.Row
		row := map[string]any{"kind": f.kind(), "episode": f.Episode, "recovery_s": r.RecoveryS,
			"pin_ranges_bytes": r.PinBound.bytes(), "pin_pool_bytes": r.PinBound.PoolBytes, "pin_cache_bytes": r.PinBound.CacheBytes, "retained_budget_bytes": r.RetainedBudgetBytes, "free_below_bytes": r.FreeBelowBytes,
			"client_fallback_counters": append([]string{}, r.ClientFallbackCounters...)}
		if f.kind() == KindLinkFlap { // F3: the attribution receipt's target VF; absent otherwise
			row["netdev"], row["rdma_device"], row["netdev_class"] = r.Netdev, r.RDMADevice, r.NetdevClass
		}
		config["fault_row"] = row
	}
	if x.rc() {
		config["read_transport"] = "rc" // the RC soak's routes and client kill; absent is DC
	}
	raw, err := json.Marshal(config)
	return []string{"-I", path.Join(s.HelperDir, "soak_assess.py"), "--config", string(raw)}, err
}

// soakAssessHelpers are the pinned sources the verdict job imports.
var soakAssessHelpers = []string{"soak_assess.py", "soak_load.py", "soak_native_worker.py", "soak_oracle.py", "soak_stats.py"}

// SoakVerdict is the assessor's one bounded row. Go decodes and integrates
// it; the replay itself lives only in soak_assess.py.
type SoakVerdict struct {
	Status   string `json:"status"`
	Criteria map[string]struct {
		Outcome string `json:"outcome"`
		Reason  string `json:"reason"`
	} `json:"criteria"`
	K7        []map[string]any `json:"k7"`
	Resources string           `json:"resources"`
	Binding   SoakBinding      `json:"binding"`
	Error     string           `json:"error,omitempty"`
	// What an excused miss means (decision 3 revision): carried on every row.
	MissesQualification struct {
		Method string `json:"method"`
		Clock  string `json:"clock"`
		Limit  string `json:"limit"`
	} `json:"misses_qualification"`
	// Rule A (Ping, 2026-11-04): misses inside the clock bracket around B, reported per op; they do not fail the row.
	MissesUndecidable []MissUndecidable `json:"misses_undecidable,omitempty"`
	// Stage 3 rows only: the row id, scoped claims (F4's QP claim), the attributed UNKNOWN count and each
	// resource view's quiescence class.
	Row               string            `json:"row,omitempty"`
	Claims            map[string]string `json:"claims,omitempty"`
	UnknownAttributed *int              `json:"unknown_attributed,omitempty"`
	ResourceClasses   map[string]string `json:"resource_classes,omitempty"`
	LayoutScope       string            `json:"layout_scope,omitempty"`
	// F3 and F5 (r12; r26): late completions of futures abandoned at their deadline, reported for the reader; no criterion.
	LateCompletions *int `json:"late_completions,omitempty"`
	// F5 only (r26): every quarantined PUT the fresh audit read as NEW, a visible #1219 finding whatever the status.
	// A pointer, so an F5 row's empty list survives the publish (present and empty is not absent).
	Findings *[]SoakFinding `json:"findings,omitempty"`
}

// SoakFinding is one #1219 finding: exactly these members, each required.
type SoakFinding struct {
	Issue int    `json:"issue"`
	Kind  string `json:"kind"`
	Op    string `json:"op"`
	Key   string `json:"key"`
	Error string `json:"error"`
}

// findingKinds: generic_error_read_new (a generic error, then an audit read of NEW: ambiguous, the row may pass) and
// typed_contradiction (a TYPED nothing-written claim, then NEW: the write contract fails).
var findingKinds = []string{"generic_error_read_new", "typed_contradiction"}

// rowLayoutScope is the layout every stage-3 row result is limited to (soak_assess.LAYOUT_SCOPE): one volume
// process, on M02, which F1's PUT attribution needs. A row verdict without exactly this scope does not decode.
const rowLayoutScope = "single volume process, on M02: multi-volume layouts are not covered"

// MissUndecidable is one rule-A miss: exactly these four members, each required and typed.
type MissUndecidable struct {
	Op                 string   `json:"op"`
	Vid                *int     `json:"vid"`
	SubmitMinusEarlyBS *float64 `json:"submit_minus_early_b_s"`
	SubmitMinusLateBS  *float64 `json:"submit_minus_late_b_s"`
}

// verdictFields are the decoded field names. encoding/json matches struct
// fields case-insensitively, so a key that folds to one of them must be it.
var verdictFields = []string{"status", "criteria", "k7", "resources", "binding", "error", "outcome", "reason", "schema", "run_id", "boot_id", "root",
	"clock", "entry_epoch", "exit_epoch", "entry_mono", "exit_mono", // the clock row is decoded by encoding/json too
	"misses_qualification", "method", "limit",
	"misses_undecidable", "op", "vid", "submit_minus_early_b_s", "submit_minus_late_b_s",
	"file", "bytes", "sha256", // the M02 evidence reference row (#1002)
	"row", "claims", "unknown_attributed", "resource_classes", "layout_scope", // stage 3 rows
	"late_completions", "findings", "issue", "kind", "key"} // F3/F5 late completions; r26 F5 #1219 findings

// oneStrictValue admits exactly one JSON value with no duplicate object keys
// and no case-folded alias of a decoded field.
func oneStrictValue(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := uniqueKeys(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("more than one JSON value")
	}
	return nil
}

func uniqueKeys(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil || (tok != json.Delim('{') && tok != json.Delim('[')) {
		return err
	}
	seen := map[string]bool{}
	for dec.More() {
		if tok == json.Delim('{') {
			k, err := dec.Token()
			if err != nil {
				return err
			}
			key := k.(string)
			if seen[key] {
				return fmt.Errorf("duplicate JSON key %q", key)
			}
			seen[key] = true
			for _, f := range verdictFields {
				if key != f && strings.EqualFold(key, f) {
					return fmt.Errorf("JSON key %q aliases %q", key, f)
				}
			}
		}
		if err = uniqueKeys(dec); err != nil {
			return err
		}
	}
	_, err = dec.Token()
	return err
}

// decodeVerdict accepts exactly one strict row for the expected run; a PASS
// must carry exactly the mandatory criteria, all PASS. Anything else fails closed.
func decodeVerdict(e tr.ExitInfo, want SoakBinding, criteria []string) (SoakVerdict, error) {
	var v SoakVerdict
	out, _, err := e.Output.Decode()
	if err != nil || e.Code != 0 {
		return v, errors.New("soak verdict job failed or its output is incomplete")
	}
	if err = oneStrictValue(out); err != nil {
		return v, fmt.Errorf("soak verdict is not one strict row: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.DisallowUnknownFields()
	row := slices.Equal(criteria, rowCriteria)
	if err = dec.Decode(&v); err != nil {
		return v, errors.New("soak verdict malformed")
	}
	if row {
		return v, decodeRowVerdict(v, want, criteria)
	}
	if (v.Status != "PASS" && v.Status != "FAIL") || v.Resources != "NOT_OBSERVED" || v.Row != "" || v.Claims != nil || v.UnknownAttributed != nil || v.ResourceClasses != nil || v.LayoutScope != "" || v.LateCompletions != nil || v.Findings != nil {
		return v, errors.New("soak verdict malformed")
	}
	if q := v.MissesQualification; q.Method == "" || q.Clock == "" || q.Limit == "" {
		return v, errors.New("soak verdict without its misses qualification")
	}
	// Rule A rows: every member present, typed and finite. The offsets are rounded diagnostics: the Python
	// assessor alone classifies, so the sign is not judged again here (codex01 on 66da7699).
	for _, u := range v.MissesUndecidable {
		if u.Op == "" || u.Vid == nil || *u.Vid <= 0 || u.SubmitMinusEarlyBS == nil || u.SubmitMinusLateBS == nil ||
			math.IsNaN(*u.SubmitMinusEarlyBS) || math.IsInf(*u.SubmitMinusEarlyBS, 0) || math.IsNaN(*u.SubmitMinusLateBS) ||
			math.IsInf(*u.SubmitMinusLateBS, 0) {
			return v, errors.New("soak verdict undecidable miss malformed")
		}
	}
	if v.Binding != want {
		return v, fmt.Errorf("soak verdict is bound to %+v, not this run", v.Binding)
	}
	for name, c := range v.Criteria {
		if !contains(criteria, name) || (c.Outcome != "PASS" && c.Outcome != "FAIL") {
			return v, fmt.Errorf("soak verdict criterion %q unknown or malformed", name)
		}
		if c.Outcome != "PASS" && v.Status == "PASS" {
			return v, errors.New("soak verdict PASS with a failed criterion")
		}
	}
	if v.Status == "PASS" && (len(v.Criteria) != len(criteria) || v.Error != "") {
		return v, errors.New("soak verdict PASS without every mandatory criterion")
	}
	return v, nil
}

// decodeRowVerdict integrates a stage-3 row verdict: PASS, FAIL or NOT_COVERED, each criterion one of those,
// and the row's status exactly the worst of them (FAIL over NOT_COVERED over PASS), so a missing witness can
// never be read as PASS. A PASS also carries every criterion, the attributed UNKNOWN count and the classes.
func decodeRowVerdict(v SoakVerdict, want SoakBinding, criteria []string) error {
	outcomes := []string{"PASS", "FAIL", "NOT_COVERED"}
	if !slices.Contains(outcomes, v.Status) || v.Resources != "ACCOUNTED" || v.LayoutScope != rowLayoutScope || v.MissesQualification.Method != "" || len(v.MissesUndecidable) != 0 {
		return errors.New("soak row verdict malformed")
	}
	if v.Binding != want {
		return fmt.Errorf("soak verdict is bound to %+v, not this run", v.Binding)
	}
	worst := "PASS"
	for name, c := range v.Criteria {
		if !contains(criteria, name) || !slices.Contains(outcomes, c.Outcome) {
			return fmt.Errorf("soak row criterion %q unknown or malformed", name)
		}
		if c.Outcome == "FAIL" || c.Outcome == "NOT_COVERED" && worst == "PASS" {
			worst = c.Outcome
		}
	}
	if v.Error != "" && v.Status != "FAIL" || v.Error == "" && v.Status != worst {
		return errors.New("soak row verdict status is not the worst of its criteria")
	}
	for k, c := range v.Claims {
		if k != "qp" || c != "NOT_COVERED" {
			return errors.New("soak row verdict claim unknown")
		}
	}
	if v.Status == "PASS" && (len(v.Criteria) != len(criteria) || v.UnknownAttributed == nil || len(v.ResourceClasses) == 0 || v.Row == "") {
		return errors.New("soak row verdict PASS without every criterion, its UNKNOWN count and resource classes")
	}
	late := v.Row == "F3" || v.Row == "F5"
	if v.LateCompletions != nil && (*v.LateCompletions < 0 || !late) || v.Status == "PASS" && late && v.LateCompletions == nil {
		return errors.New("soak row verdict late-completion count missing, negative or outside F3/F5")
	}
	return checkFindings(v)
}

// checkFindings (r26): findings only on an F5 row, present on every F5 row the assessor judged (empty included, any
// status), each one well formed; a typed contradiction is never on a row whose write contract passed.
func checkFindings(v SoakVerdict) error {
	if v.Findings == nil {
		if v.Row == "F5" && v.Error == "" {
			return errors.New("soak row verdict F5 without its findings")
		}
		return nil
	}
	if v.Row != "F5" {
		return errors.New("soak row verdict findings outside F5")
	}
	for _, f := range *v.Findings {
		if f.Issue != 1219 || !slices.Contains(findingKinds, f.Kind) || f.Op == "" || f.Key == "" || len(f.Error) > 400 {
			return errors.New("soak row verdict finding malformed")
		}
		if f.Kind == "typed_contradiction" && v.Criteria["write_contract"].Outcome != "FAIL" {
			return errors.New("soak row verdict typed contradiction on a passing write contract")
		}
	}
	return nil
}

// readBackVerdict (r26) re-reads the published soak-verdict.json: exactly the bytes of the decoded verdict, one strict
// value (no duplicate key, no case-folded alias), a strict decode with no unknown field whose own publish is those same
// bytes, and the findings and late count equal to the decoded verdict's. A field the publish dropped or changed (an F5
// row's findings, empty or not) fails instead of passing silently.
func readBackVerdict(path string, v SoakVerdict) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("soak verdict readback: %w", err)
	}
	want, _ := json.MarshalIndent(v, "", " ")
	if !bytes.Equal(raw, want) {
		return errors.New("soak verdict readback is not the verdict that was decoded")
	}
	if err = oneStrictValue(raw); err != nil {
		return fmt.Errorf("soak verdict readback is not one strict value: %w", err)
	}
	var back SoakVerdict
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&back); err != nil {
		return fmt.Errorf("soak verdict readback malformed: %w", err)
	}
	again, _ := json.MarshalIndent(back, "", " ")
	if !bytes.Equal(again, raw) || !reflect.DeepEqual(back.Findings, v.Findings) || !reflect.DeepEqual(back.LateCompletions, v.LateCompletions) {
		return errors.New("soak verdict readback differs from the decoded verdict")
	}
	return nil
}
