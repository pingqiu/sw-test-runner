package v3block

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/declared"
)

// What `ops chunk show` prints for a volume with an open snapshot session,
// a committed image and two snapshots, one with a restore target: the
// product's own encoding of its layout.Head (encoding/json over the Go
// types, cmd/sw-block/chunk_b.go:219-233, cmd/sw-block/chunk.go:317), made
// by marshalling a layout.Head with the product's types (the same lines at
// the read_at pin and at v1.0.0-rc.3). VolumeID and every hash are byte
// arrays, so they print as lists of numbers; Activity alone has JSON tags.
const chunkShowWholeHead = `{"volume":null,"head":{"Gen":12,"VolumeID":[16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,31],"Epoch":3,"Holder":"a-v06/r1","ClaimID":"c-7f3a","Activity":{"kind":"snap","session":"s-42"},"Current":{"Hash":[0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,31]},"Position":1187,"Snapshots":{"v06-s1":{"Root":{"Hash":[255,254,253,252,251,250,249,248,247,246,245,244,243,242,241,240,239,238,237,236,235,234,233,232,231,230,229,228,227,226,225,224]},"Position":640,"Meta":{"CreatedNs":1700000000000000000,"Class":"semantic","ExpiresNs":0,"Labels":null},"Dependents":[{"Target":"v06-r1","TargetID":[160,161,162,163,164,165,166,167,168,169,170,171,172,173,174,175],"Root":{"Hash":[255,254,253,252,251,250,249,248,247,246,245,244,243,242,241,240,239,238,237,236,235,234,233,232,231,230,229,228,227,226,225,224]}}]},"v06-s2":{"Root":{"Hash":[0,1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,31]},"Position":1187,"Meta":{"CreatedNs":0,"Class":"","ExpiresNs":0,"Labels":null},"Dependents":null}},"State":""}}`

// The same encoding of an unclaimed HEAD: idle (no session), no image.
const chunkShowIdleHead = `{"volume":null,"head":{"Gen":1,"VolumeID":[16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,31],"Epoch":0,"Holder":"","ClaimID":"","Activity":{"kind":"idle"},"Current":null,"Position":0,"Snapshots":null,"State":""}}`

// wantHeadDiag is the head_diag a readable answer records, decoded here
// with the product's field names (core/frontend/chunkstore/layout/head.go:
// 143-154, :29-32), not by the description, and read back as a history
// record holds it.
func wantHeadDiag(t *testing.T, out string) any {
	t.Helper()
	var body struct {
		Head *struct {
			ClaimID  string
			Activity *struct {
				Kind    string `json:"kind"`
				Session string `json:"session"`
			}
			Current   map[string]json.RawMessage
			Position  uint64
			Snapshots map[string]json.RawMessage
		} `json:"head"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &body); err != nil || body.Head == nil {
		t.Fatalf("not a readable HEAD: %q", out)
	}
	h := body.Head
	b, err := json.Marshal(map[string]any{"claim_id": h.ClaimID, "activity": h.Activity, "position": h.Position, "current": h.Current, "snapshots": h.Snapshots})
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// wholeHead is the control: the observation of chunkShowWholeHead records
// every field of HEAD a generation move can be named by, with its value.
func wholeHead(rec tr.HistoryRecord) error {
	d, ok := rec.Result["head_diag"].(map[string]any)
	if !ok {
		return fmt.Errorf("no head_diag in %v", rec.Result)
	}
	if d["claim_id"] != "c-7f3a" {
		return fmt.Errorf("claim_id %v", d["claim_id"])
	}
	if !reflect.DeepEqual(d["activity"], map[string]any{"kind": "snap", "session": "s-42"}) {
		return fmt.Errorf("activity %v", d["activity"])
	}
	if d["position"] != float64(1187) {
		return fmt.Errorf("position %v", d["position"])
	}
	cur, _ := d["current"].(map[string]any)
	if h, _ := cur["Hash"].([]any); len(h) != 32 || h[1] != float64(1) {
		return fmt.Errorf("current %v", d["current"])
	}
	snaps, _ := d["snapshots"].(map[string]any)
	s1, _ := snaps["v06-s1"].(map[string]any)
	deps, _ := s1["Dependents"].([]any)
	if len(snaps) != 2 || len(deps) != 1 || deps[0].(map[string]any)["Target"] != "v06-r1" {
		return fmt.Errorf("snapshots' dependents %v", d["snapshots"])
	}
	if s2, _ := snaps["v06-s2"].(map[string]any); s2 == nil || s2["Dependents"] != nil {
		return fmt.Errorf("a snapshot with no dependents %v", snaps["v06-s2"])
	}
	return nil
}

func observeOnce(t *testing.T, h tr.ActionHandlerFunc, show string) tr.HistoryRecord {
	t.Helper()
	ws := eqAnswer{out: strings.Repeat("key: value\n", 15)}
	c := eqCase{name: "whole head", params: map[string]string{"volume": "v1"}, rules: []eqRule{
		{"ops chunk show", []eqAnswer{{out: show + "\n"}}}, {"ops workspace show", []eqAnswer{ws}},
		{"namePattern", []eqAnswer{{out: `["v1-01234567"]`}}}, {"python3", []eqAnswer{{out: `["HEAD","VOLUME"]`}}}}}
	r := runEq(t, h, c, true)
	if r.err != nil || len(r.records) != 2 || r.records[0].Kind != tr.HistoryObserve {
		t.Fatalf("observe: %v %v", r.err, r.records)
	}
	return r.records[0]
}

// oldDecode is the observation as it was before head_diag: HEAD's
// generation, epoch, holder and state, and the snapshot names.
func oldDecode(t *testing.T) tr.ActionHandlerFunc {
	t.Helper()
	var d map[string]any
	if err := json.Unmarshal(actionsJSON, &d); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range d["actions"].([]any) {
		a := a.(map[string]any)
		if a["action"] != "v3_op_observe" {
			continue
		}
		for _, st := range a["steps"].([]any) {
			read := st.(map[string]any)["read"].(map[string]any)
			if f, ok := read["fields"].(map[string]any); ok && f["head_diag"] != nil {
				delete(f, "head_diag")
				head := read["schema"].(map[string]any)["head"].(map[string]any)
				for _, k := range []string{"ClaimID", "Activity", "Current", "Position"} {
					delete(head, k)
				}
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the observe step that decodes HEAD is not in the description")
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	f, err := declared.ParseWith(b, declaredScripts())
	if err != nil {
		t.Fatal(err)
	}
	x := *blockActions
	x.File = f
	return x.Handler("v3_op_observe")
}

// The observation records the whole HEAD beside head (head_diag), from a
// realistic answer; head itself is the tuple C5 compares, unchanged. Red:
// the old decode, which kept only epoch, gen, holder and state, fails the
// same control.
func TestObserveRecordsTheWholeHead(t *testing.T) {
	rec := observeOnce(t, blockActions.Handler("v3_op_observe"), chunkShowWholeHead)
	if err := wholeHead(rec); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rec.Result["head_diag"], wantHeadDiag(t, chunkShowWholeHead)) {
		t.Fatalf("head_diag %v", rec.Result["head_diag"])
	}
	if want := map[string]any{"gen": float64(12), "epoch": float64(3), "holder": "a-v06/r1", "state": ""}; !reflect.DeepEqual(rec.Result["head"], want) {
		t.Fatalf("head %v, want %v: the compared tuple changed", rec.Result["head"], want)
	}
	if !reflect.DeepEqual(rec.Result["snapshots"], []any{"v06-s1", "v06-s2"}) {
		t.Fatalf("snapshots %v", rec.Result["snapshots"])
	}
	// An idle, unclaimed HEAD: no session, no image, no snapshots.
	idle := observeOnce(t, blockActions.Handler("v3_op_observe"), chunkShowIdleHead)
	want := map[string]any{"claim_id": "", "activity": map[string]any{"kind": "idle", "session": ""}, "position": float64(0), "current": nil, "snapshots": nil}
	if !reflect.DeepEqual(idle.Result["head_diag"], want) {
		t.Fatalf("idle head_diag %v, want %v", idle.Result["head_diag"], want)
	}

	old := observeOnce(t, oldDecode(t), chunkShowWholeHead)
	if err := wholeHead(old); err == nil {
		t.Fatal("red control: the old decode passed the whole-HEAD control")
	}
	// The old decode and the new one agree on every field but head_diag.
	got := map[string]any{}
	for k, v := range rec.Result {
		if k != "head_diag" {
			got[k] = v
		}
	}
	if !reflect.DeepEqual(got, old.Result) {
		t.Fatalf("beyond head_diag the observation changed:\nnew %v\nold %v", got, old.Result)
	}
}
