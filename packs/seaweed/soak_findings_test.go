package seaweed

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// r26 F5 (plan v4 A, v5 carried): the assessor's #1219 findings and F5's late-completion count are Go-decoded,
// strict, published and read back unchanged, on PASS and on FAIL; Go never drops or reshapes them.

var f5Want = SoakBinding{SoakVerdictSchema, "run-1", "boot-1", "/r"}

func f5Row(status string, outcomes map[string]string, findings string) string {
	row := strings.Replace(rowVerdict(status, outcomes, findings), `"row":"F3"`, `"row":"F5"`, 1)
	return row
}

const oneFinding = `"findings":[{"issue":1219,"kind":"generic_error_read_new","op":"964","key":"k@ab","error":"RuntimeError('put_cpu_many failed')"}],`

func TestF5FindingsSurviveTheStrictPublishRoundtrip(t *testing.T) {
	for name, c := range map[string]struct {
		status   string
		outcomes map[string]string
		findings string
		want     int
	}{
		"PASS, no finding":  {"PASS", nil, `"findings":[],`, 0},
		"PASS, one finding": {"PASS", nil, oneFinding, 1},
		"FAIL, one finding": {"FAIL", map[string]string{"bytes": "FAIL"}, oneFinding, 1},
	} {
		v, err := decodeVerdict(captured([]byte(f5Row(c.status, c.outcomes, c.findings))), f5Want, rowCriteria)
		if err != nil || v.Findings == nil || len(*v.Findings) != c.want || v.LateCompletions == nil {
			t.Fatalf("%s: decode %v %+v", name, err, v)
		}
		path := filepath.Join(t.TempDir(), "soak-verdict.json")
		body, _ := json.MarshalIndent(v, "", " ")
		if err = os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		if err = readBackVerdict(path, v); err != nil {
			t.Fatalf("%s: readback %v\n%s", name, err, body)
		}
		if c.want == 0 && !strings.Contains(string(body), `"findings": []`) {
			t.Fatalf("%s: an empty findings list must be published present, not dropped:\n%s", name, body)
		}
		back, err := decodeVerdict(captured(body), f5Want, rowCriteria) // the published bytes decode strictly again
		if err != nil || !reflect.DeepEqual(back.Findings, v.Findings) || *back.LateCompletions != *v.LateCompletions {
			t.Fatalf("%s: re-decode %v %+v", name, err, back)
		}
	}
}

func TestF5FindingsDecodeRules(t *testing.T) {
	long := `"error":"` + strings.Repeat("E", 401) + `"`
	for name, c := range map[string]struct {
		row string
		ok  bool
	}{
		"F5 PASS with findings":                           {f5Row("PASS", nil, `"findings":[],`), true},
		"F5 PASS without findings":                        {f5Row("PASS", nil, ""), false},
		"F5 NOT_COVERED without findings":                 {f5Row("NOT_COVERED", map[string]string{"causal": "NOT_COVERED"}, ""), false},
		"F5 PASS without its late count":                  {strings.Replace(f5Row("PASS", nil, `"findings":[],`), `"late_completions":0,`, "", 1), false},
		"findings on F3":                                  {rowVerdict("PASS", nil, `"findings":[],`), false},
		"finding of another issue":                        {f5Row("PASS", nil, strings.Replace(oneFinding, "1219", "1218", 1)), false},
		"finding of an unknown kind":                      {f5Row("PASS", nil, strings.Replace(oneFinding, "generic_error_read_new", "refused", 1)), false},
		"finding without its op":                          {f5Row("PASS", nil, strings.Replace(oneFinding, `"op":"964",`, "", 1)), false},
		"finding without its key":                         {f5Row("PASS", nil, strings.Replace(oneFinding, `"key":"k@ab",`, "", 1)), false},
		"finding error over 400":                          {f5Row("PASS", nil, strings.Replace(oneFinding, `"error":"RuntimeError('put_cpu_many failed')"`, long, 1)), false},
		"finding with an unknown member":                  {f5Row("PASS", nil, strings.Replace(oneFinding, `"issue":1219,`, `"issue":1219,"severity":1,`, 1)), false},
		"typed contradiction on a passing write contract": {f5Row("PASS", nil, strings.Replace(oneFinding, "generic_error_read_new", "typed_contradiction", 1)), false},
		"typed contradiction on a failing write contract": {f5Row("FAIL", map[string]string{"write_contract": "FAIL"}, strings.Replace(oneFinding, "generic_error_read_new", "typed_contradiction", 1)), true},
		"alias Findings":                                  {f5Row("PASS", nil, `"Findings":[],`), false},
		"alias Late_Completions":                          {strings.Replace(f5Row("PASS", nil, `"findings":[],`), `"late_completions"`, `"Late_Completions"`, 1), false},
		"alias inside a finding":                          {f5Row("PASS", nil, strings.Replace(oneFinding, `"op":`, `"OP":`, 1)), false},
	} {
		_, err := decodeVerdict(captured([]byte(c.row)), f5Want, rowCriteria)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%v", name, err, c.ok)
		}
	}
	if _, err := decodeVerdict(captured([]byte(passRow(t, func(r map[string]any) { r["findings"] = []any{} }))), testBinding, soakCriteria); err == nil {
		t.Error("findings on a non-row (soak) verdict must not decode")
	}
}

// r26 correction (source review of c0a826ae): the error cap is 400 characters, as the assessor truncates the
// raw text, not 400 UTF-8 bytes; and a findings member is refused by its raw shape, null included, before decoding
// (a null decodes to an absent pointer that publication would drop).
func TestF5FindingErrorCapIsCharactersAndNullFindingsAreRefused(t *testing.T) {
	withError := func(s string) string {
		return f5Row("PASS", nil, strings.Replace(oneFinding, `"error":"RuntimeError('put_cpu_many failed')"`, `"error":"`+s+`"`, 1))
	}
	for name, c := range map[string]struct {
		row string
		ok  bool
	}{
		"400 multibyte characters (800 bytes)": {withError(strings.Repeat("é", 400)), true},
		"201 multibyte characters (402 bytes)": {withError(strings.Repeat("é", 201)), true},
		"401 multibyte characters":             {withError(strings.Repeat("é", 401)), false},
		"F5 findings null":                     {f5Row("PASS", nil, `"findings":null,`), false},
		"F3 findings null":                     {rowVerdict("PASS", nil, `"findings":null,`), false},
		"F5 findings an object":                {f5Row("PASS", nil, `"findings":{},`), false},
	} {
		_, err := decodeVerdict(captured([]byte(c.row)), f5Want, rowCriteria)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%v", name, err, c.ok)
		}
	}
	for name, v := range map[string]any{"null": nil, "array": []any{}} {
		row := passRow(t, func(r map[string]any) { r["findings"] = v })
		if _, err := decodeVerdict(captured([]byte(row)), testBinding, soakCriteria); err == nil {
			t.Errorf("findings %s on a non-row (soak) verdict must not decode", name)
		}
	}
}

func TestReadBackVerdictRefusesWhatWasNotPublished(t *testing.T) {
	v, err := decodeVerdict(captured([]byte(f5Row("PASS", nil, oneFinding))), f5Want, rowCriteria)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.MarshalIndent(v, "", " ")
	dir := t.TempDir()
	for name, edit := range map[string]func(string) string{
		"findings dropped":   func(s string) string { return strings.Replace(s, `"findings"`, `"findings_dropped"`, 1) },
		"finding changed":    func(s string) string { return strings.Replace(s, `"964"`, `"965"`, 1) },
		"late count changed": func(s string) string { return strings.Replace(s, `"late_completions": 0`, `"late_completions": 1`, 1) },
		"a duplicated key": func(s string) string {
			return strings.Replace(s, `"status": "PASS",`, `"status": "PASS", "status": "PASS",`, 1)
		},
		"a case-folded alias": func(s string) string { return strings.Replace(s, `"findings"`, `"Findings"`, 1) },
		"two values":          func(s string) string { return s + "\n{}" },
	} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json")
		if err := os.WriteFile(path, []byte(edit(string(body))), 0600); err != nil {
			t.Fatal(err)
		}
		if readBackVerdict(path, v) == nil {
			t.Errorf("%s: the readback must refuse it", name)
		}
	}
	if readBackVerdict(filepath.Join(dir, "absent.json"), v) == nil {
		t.Error("an absent published verdict must be refused")
	}
}
