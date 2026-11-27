package v3block

// The CLOSED VOCABULARY of the block description (product/actions.json).
// A description is tables, not a language: it may use
//
//   - substitution ({{...}} templates, words, a script named by its path);
//   - when: ONE field compared by equals or not_equals with a constant
//     (declared.Cond refuses anything else at load);
//   - each: one invocation per list element, each with its own record;
//   - the decoding of an answer: comparisons (holds, checks, judges, the
//     outcome table) and capture.
//
// This test walks the description and FAILS on any key that is not in the
// list below for its place, and on any place that is not in the list: a
// new key, or a new nesting, is a change of the vocabulary and needs its
// review before this list grows. Keys of the maps whose keys are the
// description's own names (args, fields, schema, derived, ...) are not
// vocabulary; their values are walked. The guest section is the product's
// own settings, read by the pack, not by the executor.
//
// The result keys ("result", "result_<S>") are allowed: each is the RESULT
// AT STAGE S, what is written into the history record's result at that
// stage (package declared states the stages), never a decision.
//
// Done: the "retry" of the delete and of the snapshot moved to the driver.
// Its declaration stays here (which answer, how many, how often, the page
// line), read by the driver (randomrun.Attempts); the executor runs one
// attempt per call and never loops.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/pingqiu/sw-test-runner/declared"
)

func declaredParse(b []byte) (*declared.File, error) { return declared.ParseWith(b, declaredScripts()) }

// freeMaps are keys whose values are maps keyed by the description's own
// names.
var freeMaps = map[string]bool{"derived": true, "lines": true, "fields": true, "schema": true, "json": true, "args": true,
	"match": true, "capture": true, "on_ok_set": true, "on_invoke_set": true, "on_ready_set": true, "on_outcome_set": true,
	"absent": true, "one": true, "map": true, "exits": true, "ok_length": true, "result": true, "result_ok": true,
	"result_failure": true, "result_not_run": true, "result_answered": true, "result_launched": true, "result_ready": true,
	"result_unanswered": true, "aliases": true, "device_resolvers": true, "failure_body": true, "on_unreadable": true,
	"on_failure": true, "on_not_run": true, "object": true, "files": true, "record_fields": true, "text_values": true}

// typesOnly are maps of names (to types, or to maps of names) with no
// vocabulary below them.
var typesOnly = map[string]bool{"schema": true, "on_outcome_set": true, "failure_body": true}

var when = []string{"field", "equals", "not_equals"}
var word = []string{"json", "raw", "script", "when", "words"}
var typed = []string{"template", "type", "optional"}

// vocabulary is every key the description may use, by place.
var vocabulary = map[string][]string{
	"": {"product", "read_at", "about", "config", "runner_io_dir", "run_values", "device_resolvers", "actions",
		"runner_setup", "exceptions", "guest"},
	".config":                  {"keys", "client"},
	".config.keys[]":           {"name", "required", "default", "computed"},
	".run_values[]":            {"name", "why"},
	".runner_setup[]":          {"action", "why"},
	".exceptions[]":            {"action", "why", "declarable_with", "runner_deadline_seconds", "plus_param"},
	".exceptions[].plus_param": {"name", "default"},
	".actions[]": {"action", "verb", "op", "kind", "runs_on", "as_root", "params", "record", "variants", "retry", "on_ok_set",
		"steps", "derived", "note", "runner_deadline_seconds", "plus_param", "each", "input", "device", "io",
		"on_invoke_set", "process", "on_outcome_set", "sequence"},
	".actions[].params[]":      {"name", "kind", "vars", "required", "default", "pattern", "values", "item_pattern", "aliases", "non_empty"},
	".actions[].record":        {"volume", "args"},
	".actions[].record.args.*": typed,
	".actions[].plus_param":    {"name", "default"},
	".actions[].derived.*":     {"kind", "from", "error", "map"},
	// Done: the driver re-issues (randomrun.Attempts) and reads this.
	".actions[].retry": {"while_code", "while_message_contains", "answers", "max_attempts", "every_seconds", "within_param",
		"request_arg", "page_cite", "expected_refusal", "note"},
	".actions[].retry.answers[]":        {"code", "message_contains"},
	".actions[].retry.expected_refusal": {"param", "why"},
	".actions[].variants[]":             {"name", "when", "command", "runner_deadline_seconds", "decode", "cite"},
	".actions[].variants[].when":        when,
	".actions[].variants[].command[]":   word,
	".actions[].variants[].decode": {"kind", "action", "path", "answer", "transport_failure", "fields", "outcome_field",
		"exits", "match", "ok_length", "result", "result_ok", "detail"},
	".actions[].steps[]":           {"name", "when", "command", "runner_deadline_seconds", "cite", "read"},
	".actions[].steps[].when":      when,
	".actions[].steps[].command[]": word,
	".actions[].steps[].read": {"kind", "schema", "require", "fields", "on_unreadable", "on_failure", "on_not_run",
		"failure_body", "into", "lines", "stderr_lines", "stderr_why", "error_into", "error"},
	".actions[].steps[].read.fields.*":              {"value", "int", "sorted_keys", "object"},
	".actions[].steps[].read.fields.*.object.*":     {"value", "int", "sorted_keys", "object"},
	".actions[].steps[].read.on_unreadable.*":       {"value", "int", "sorted_keys", "object"},
	".actions[].steps[].read.on_failure.*":          {"value", "int", "sorted_keys", "object"},
	".actions[].steps[].read.on_failure.*.object.*": {"value", "int", "sorted_keys", "object"},
	".actions[].steps[].read.on_not_run.*":          {"value", "int", "sorted_keys", "object"},
	".actions[].steps[].read.on_not_run.*.object.*": {"value", "int", "sorted_keys", "object"},
	".actions[].input":                              {"kind", "text", "bytes", "path", "command", "cite"},
	".actions[].input.command[]":                    word,
	".actions[].device":                             {"resolver", "subsystem", "within_seconds", "cite"},
	".actions[].io": {"command", "as_root", "runner_deadline_seconds", "cite", "ok_stdout", "failure", "result",
		"result_unanswered"},
	".actions[].io.command[]": word,
	".actions[].process": {"op", "subject", "name", "cite", "argv", "log", "before", "owned", "ready", "after",
		"result_launched", "result_ready", "on_ready_set", "within_param"},
	".actions[].process.argv[]":                   word,
	".actions[].process.argv[].when":              when,
	".actions[].process.before[]":                 probe,
	".actions[].process.before[].when":            when,
	".actions[].process.before[].command[]":       word,
	".actions[].process.before[].command[].when":  when,
	".actions[].process.before[].decode":          {"kind", "fields", "outcome_field", "exits", "match", "ok_length", "detail"},
	".actions[].process.owned[]":                  probe,
	".actions[].process.owned[].when":             when,
	".actions[].process.owned[].command[]":        word,
	".actions[].process.ready":                    {"within_param", "every_ms", "probes", "not_ready", "not_ready_by_deadline"},
	".actions[].process.ready.probes[]":           probe,
	".actions[].process.ready.probes[].command[]": word,
	".actions[].process.ready.probes[].holds[]":   {"path", "equals", "above", "at_least"},
	".actions[].process.after[]":                  probe,
	".actions[].process.result_launched.*":        typed,
	".actions[].process.result_ready.*":           typed,
	".actions[].sequence":                         {"steps_of", "steps", "changes_from", "result"},
	".actions[].sequence.steps_of":                {"action", "detail_prefix"},
	".actions[].sequence.result.*":                typed,
	".actions[].sequence.steps[]": {"name", "kind", "cite", "command", "as_root", "judge", "answer", "failure", "not_run",
		"not_run_drops_result", "result_failure", "result_not_run", "result_answered", "result", "on_ok_set", "resolver",
		"observe", "subsystem", "pid", "start", "within_seconds", "pattern", "every_ms", "capture"},
	".actions[].sequence.steps[].command[]":               word,
	".actions[].sequence.steps[].judge":                   {"kind", "action", "path"},
	".actions[].sequence.steps[].observe":                 {"verb", "subsystem", "removed"},
	".actions[].sequence.steps[].answer":                  {"kind", "schema", "unreadable", "checks", "pattern", "match", "failure", "capture"},
	".actions[].sequence.steps[].answer.checks[]":         {"holds", "stderr_equals", "failure"},
	".actions[].sequence.steps[].answer.checks[].holds[]": {"path", "equals", "above", "at_least"},
	".actions[].sequence.steps[].result.*":                typed,
	".actions[].sequence.steps[].result_failure.*":        typed,
	".actions[].sequence.steps[].result_not_run.*":        typed,
	".actions[].sequence.steps[].result_answered.*":       typed,
	".device_resolvers.*":                                 {"cite", "scan", "connect", "disconnect", "readback"},
	".device_resolvers.*.scan": {"command", "as_root", "end", "lines", "absent", "one", "device_line", "device",
		"unreadable", "bad_line", "incomplete", "no_device", "no_device_by_deadline", "every_ms"},
	".device_resolvers.*.scan.command[]":    word,
	".device_resolvers.*.connect":           {"command", "as_root", "require", "failure"},
	".device_resolvers.*.connect.command[]": word,
	".device_resolvers.*.connect.require[]": {"value", "pattern", "failure"},
	".device_resolvers.*.disconnect": {"command", "as_root", "count_pattern", "failure", "confirm_within_seconds",
		"confirm_every_ms", "not_confirmed", "remains", "remains_by_deadline"},
	".device_resolvers.*.disconnect.command[]": word,
	".device_resolvers.*.readback": {"command", "as_root", "unit_line", "value_line", "not_shown", "text_values",
		"unreadable", "not_one", "unit_key", "mismatch", "mismatch_item"},
	".device_resolvers.*.readback.command[]": word,
}

var probe = []string{"name", "kind", "when", "command", "as_root", "decode", "failure", "on_ok_set", "addresses", "schema",
	"holds", "capture", "address", "subsystem", "options", "unless_present", "resolver", "within_seconds", "into", "cite",
	"expect", "files", "fail_run", "record_verb", "record_fields"}

// vocabularyErrors lists every key of the description d outside the
// vocabulary.
func vocabularyErrors(d any) []string {
	var bad []string
	var walk func(o any, place, key string)
	walk = func(o any, place, key string) {
		switch o := o.(type) {
		case map[string]any:
			if typesOnly[key] {
				return // names to types or templates, nothing nested of the vocabulary
			}
			if freeMaps[key] {
				for _, v := range o {
					walk(v, place+".*", "")
				}
				return
			}
			if place == ".actions[].derived.*" && o["kind"] != "map" {
				bad = append(bad, fmt.Sprintf("%s: kind %v; a derived value is a map (a lookup table)", place, o["kind"]))
			}
			allowed, known := vocabulary[place]
			if !known && place != ".guest" {
				bad = append(bad, fmt.Sprintf("%s: a place the vocabulary does not have", place))
				return
			}
			for k, v := range o {
				if place == "" && k == "guest" {
					continue // the product's own settings
				}
				if !contains(allowed, k) {
					bad = append(bad, fmt.Sprintf("%s: key %q is not in the vocabulary", place, k))
					continue
				}
				walk(v, place+"."+k, k)
			}
		case []any:
			for _, v := range o {
				walk(v, place+"[]", key)
			}
		}
	}
	walk(d, "", "")
	sort.Strings(bad)
	return bad
}

func TestClosedVocabulary(t *testing.T) {
	var d any
	if err := json.Unmarshal(actionsJSON, &d); err != nil {
		t.Fatal(err)
	}
	if bad := vocabularyErrors(d); len(bad) != 0 {
		t.Fatalf("outside the closed vocabulary:\n%s", strings.Join(bad, "\n"))
	}
	// Red controls: a new key seeded where an action, a step, a word and a
	// condition can hold one, and a new nesting, are each caught; and a
	// condition beyond one constant comparison does not load.
	for name, c := range map[string][2]string{
		"a loop on an action":     {`"action": "v3_op_create",`, `"action": "v3_op_create", "repeat": {"times": 3},`},
		"an expression in a step": {`{"name": "detach", "kind": "command",`, `{"name": "detach", "kind": "command", "if": "{{x}} > 1",`},
		"a new word form":         {`{"raw": "&&"}`, `{"raw": "&&", "eval": "x"}`},
		"a compound condition":    {`"when": {"field": "volume.holder_alive", "equals": "true"}`, `"when": {"field": "volume.holder_alive", "equals": "true", "or": {"field": "x", "equals": "y"}}`},
		"a new nesting":           {`"plus_param": {"name": "ready_seconds", "default": 60}`, `"plus_param": {"name": "ready_seconds", "default": 60, "scale": {"by": 2}}`},
	} {
		seeded := strings.Replace(string(actionsJSON), c[0], c[1], 1)
		if seeded == string(actionsJSON) {
			t.Fatalf("%s: the seed did not apply", name)
		}
		var sd any
		if err := json.Unmarshal([]byte(seeded), &sd); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(vocabularyErrors(sd)) == 0 {
			t.Errorf("red control %s: not caught", name)
		}
	}
	for name, c := range map[string][2]string{
		"a template in a condition": {`"when": {"field": "volume.holder_alive", "equals": "true"}`, `"when": {"field": "volume.holder_alive", "equals": "{{volume.name}}"}`},
		"two comparisons":           {`"when": {"field": "volume.holder_alive", "equals": "true"}`, `"when": {"field": "volume.holder_alive", "equals": "true", "not_equals": "x"}`},
	} {
		seeded := strings.Replace(string(actionsJSON), c[0], c[1], 1)
		if seeded == string(actionsJSON) {
			t.Fatalf("%s: the seed did not apply", name)
		}
		if vocabularyErrors(mustJSON(t, seeded)) != nil {
			continue // caught by the walk already
		}
		if _, err := declaredParse([]byte(seeded)); err == nil {
			t.Errorf("red control %s: loaded", name)
		}
	}
}

// A derived value is a lookup table: the walk takes kind "map" only, and a
// function kind does not load (red control).
func TestDerivedIsAMapOnly(t *testing.T) {
	seeded := strings.Replace(string(actionsJSON), `"kind": "map", "from": "{{flush}}", "map": {"true": "fsync,notrunc"`,
		`"kind": "prefix", "from": "{{flush}}", "length": 2, "map": {"true": "fsync,notrunc"`, 1)
	if seeded == string(actionsJSON) {
		t.Fatal("the seed did not apply")
	}
	if len(vocabularyErrors(mustJSON(t, seeded))) == 0 {
		t.Error("red control: a function kind passes the walk")
	}
	if _, err := declaredParse([]byte(seeded)); err == nil {
		t.Error("red control: a function kind loaded")
	}
	if _, err := declaredParse([]byte(strings.Replace(seeded, `"length": 2, `, "", 1))); err == nil {
		t.Error("red control: a function kind with map keys only loaded")
	}
}

func mustJSON(t *testing.T, s string) any {
	t.Helper()
	var d any
	if err := json.Unmarshal([]byte(s), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// placementVocabulary is the closed vocabulary of checks.json's placement
// rows (declared.Placement): the key "placement", each row's keys, the one
// "by", the four cases and the four outcomes.
var placementVocabulary = map[string][]string{
	".placement[]":          {"verb", "by", "field", "arg", "outcomes", "cite", "what"},
	".placement[].by":       {declared.PlaceByListMember},
	".placement[].outcomes": {"present_before", "others_lost", "present", "absent"},
	".placement[].outcome":  declared.PlacementOutcomes,
}

// The placement table loads (one row: the snapshot, by list_member), its
// keys are the vocabulary's, and a row outside it does not load (red
// controls: another by, another outcome, a case missing or added, another
// key).
func TestPlacementVocabulary(t *testing.T) {
	rows, err := declared.ParsePlacements(checksJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows["snapshot"] == nil || rows["snapshot"].By != declared.PlaceByListMember {
		t.Fatalf("placement rows %+v", rows)
	}
	var d struct {
		Placement []map[string]any `json:"placement"`
	}
	if err := json.Unmarshal(checksJSON, &d); err != nil {
		t.Fatal(err)
	}
	for _, row := range d.Placement {
		for k := range row {
			if !contains(placementVocabulary[".placement[]"], k) {
				t.Errorf("placement key %q is not in the vocabulary", k)
			}
		}
		if !contains(placementVocabulary[".placement[].by"], fmt.Sprint(row["by"])) {
			t.Errorf("placement by %v", row["by"])
		}
		for c, o := range row["outcomes"].(map[string]any) {
			if !contains(placementVocabulary[".placement[].outcomes"], c) || !contains(placementVocabulary[".placement[].outcome"], fmt.Sprint(o)) {
				t.Errorf("placement case %q -> %v", c, o)
			}
		}
	}
	for name, c := range map[string][2]string{
		"another by":      {`"by": "list_member"`, `"by": "snapshot_names"`},
		"another outcome": {`"absent": "did_not"`, `"absent": "maybe"`},
		"a case missing":  {`, "absent": "did_not"`, ``},
		"a case added":    {`"absent": "did_not"`, `"absent": "did_not", "gone": "third"`},
		"another key":     {`"arg": "name",`, `"arg": "name", "when": "always",`},
	} {
		seeded := strings.Replace(string(checksJSON), c[0], c[1], 1)
		if seeded == string(checksJSON) {
			t.Fatalf("%s: the seed did not apply", name)
		}
		if _, err := declared.ParsePlacements([]byte(seeded)); err == nil {
			t.Errorf("red control %s: loaded", name)
		}
	}
}
