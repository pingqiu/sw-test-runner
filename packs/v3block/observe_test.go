package v3block

import (
	"errors"
	"reflect"
	"testing"
)

// The shape `ops chunk show` prints: the product's decoded HEAD with its Go
// field names, the snapshots map keyed by name.
const chunkShowTwoSnaps = `{"volume":{"Name":"v1"},"head":{"Gen":4,"VolumeID":"2ba321a7b77b064af31a86e5e1bbc096","Epoch":1,"Holder":"a-v1/r1","ClaimID":"c","Activity":0,"Current":null,"Position":9,"Snapshots":{"s2":{"Root":"r2","Position":9},"s1":{"Root":"r1","Position":5}},"State":""}}`

func TestObserveChunkShowRecordsSnapshotNamesSorted(t *testing.T) {
	res := map[string]any{}
	observeChunkShow(res, chunkShowTwoSnaps+"\n", 0, nil)
	if got := res["snapshots"]; !reflect.DeepEqual(got, []string{"s1", "s2"}) {
		t.Fatalf("snapshots %v", got)
	}
	want := map[string]any{"gen": uint64(4), "epoch": uint64(1), "holder": "a-v1/r1", "state": ""}
	if !reflect.DeepEqual(res["head"], want) {
		t.Fatalf("head %v", res["head"])
	}
	if _, ok := res["snapshots_error"]; ok {
		t.Fatalf("snapshots_error on a readable HEAD: %v", res)
	}
}

func TestObserveChunkShowNoSnapshotsIsAnEmptyList(t *testing.T) {
	res := map[string]any{}
	observeChunkShow(res, `{"head":{"Gen":2,"Epoch":1,"Holder":"a-v1/r1","Snapshots":null,"State":""}}`, 0, nil)
	if got, ok := res["snapshots"].([]string); !ok || len(got) != 0 {
		t.Fatalf("snapshots %#v, want an empty list", res["snapshots"])
	}
}

func TestObserveChunkShowWithoutHeadRecordsWhy(t *testing.T) {
	for name, c := range map[string]struct {
		out  string
		code int
		err  error
	}{
		"exit":       {`{"code":"volume_not_found"}`, 1, nil},
		"unreadable": {`not json`, 0, nil},
		"no head":    {`{"volume":{}}`, 0, nil},
		"transport":  {``, -1, errors.New("ssh: down")},
	} {
		res := map[string]any{}
		observeChunkShow(res, c.out, c.code, c.err)
		if _, ok := res["snapshots"]; ok {
			t.Fatalf("%s: snapshots recorded without a HEAD: %v", name, res)
		}
		if s, _ := res["snapshots_error"].(string); s == "" {
			t.Fatalf("%s: no snapshots_error: %v", name, res)
		}
	}
}
