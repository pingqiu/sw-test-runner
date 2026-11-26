package v3block

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
)

const oneDevice = "ctrl nvme3\nns nvme3c3n1\nend\n"

// historyRecords reads back every record of a history file.
func historyRecords(t *testing.T, path string) []tr.HistoryRecord {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rs []tr.HistoryRecord
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var r tr.HistoryRecord
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatal(err)
		}
		rs = append(rs, r)
	}
	return rs
}

// A flushed write is today's command (conv=fsync) and today's record (no
// flush key); an unflushed one drops only the fsync and says "flush": false.
func TestWriteFlushVariant(t *testing.T) {
	flushed := writeCmd("/r/.block-v1-7", "/dev/nvme3n1", 7, true)
	if flushed != "dd if=/r/.block-v1-7 of=/dev/nvme3n1 bs=4096 seek=7 count=1 oflag=direct conv=fsync,notrunc status=none" {
		t.Errorf("flushed write changed: %s", flushed)
	}
	unflushed := writeCmd("/r/.block-v1-7", "/dev/nvme3n1", 7, false)
	if strings.Contains(unflushed, "fsync") || !strings.Contains(unflushed, "oflag=direct") || !strings.Contains(unflushed, "conv=notrunc") {
		t.Errorf("unflushed write: %s", unflushed)
	}
	if _, has := writeArgs(7, "s", "g1", true)["flush"]; has {
		t.Error("a flushed write's record carries a flush key; today's records must stay as they are")
	}
	if f, has := writeArgs(7, "s", "g1", false)["flush"]; !has || f != false {
		t.Errorf("an unflushed write's record has flush %v", f)
	}
}

// The unflushed write through record(): its invocation on disk carries
// "flush": false and the device command issued has no fsync.
func TestUnflushedWriteIsRecordedAsSuch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	h, err := tr.OpenHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	actx := &tr.ActionContext{History: h, Log: func(string, ...interface{}) {}}
	v, _, e := testVolume()
	f := (&fakeNode{}).on("subsysnqn", ok(oneDevice)).on("dd if=", ok(""))
	out, err := record(actx, e, "write", v.Name, writeArgs(3, "abc", "g1", false), func() (string, map[string]any) {
		return writeBlock(context.Background(), f, v, "/r/.block-v1-3", 3, false)
	})
	if err != nil || out["value"] != tr.OutcomeOK {
		t.Fatalf("%v %v", out, err)
	}
	if f.issued("fsync") {
		t.Error("the unflushed write issued an fsync")
	}
	rs := historyRecords(t, path)
	if len(rs) != 2 || rs[0].Verb != "write" || rs[0].Args["flush"] != false {
		t.Fatalf("records %+v", rs)
	}
}

// v3_op_flush: fsync of the device; ok only on exit 0. Red control: a flush
// whose fsync fails, or whose device cannot be established, is unknown, never ok.
func TestFlushOutcome(t *testing.T) {
	ctx := context.Background()
	v, _, _ := testVolume()
	f := (&fakeNode{}).on("subsysnqn", ok(oneDevice)).on("os.fsync", ok(""))
	if out, res := flushDevice(ctx, f, v); out != tr.OutcomeOK || res["device"] != "/dev/nvme3n1" {
		t.Fatalf("good flush: %s %v", out, res)
	}
	if !f.issued(flushCmd("/dev/nvme3n1")) {
		t.Error("the flush command was not issued on the volume's device")
	}
	for name, n := range map[string]*fakeNode{
		"fsync fails": (&fakeNode{}).on("subsysnqn", ok(oneDevice)).on("os.fsync", fakeAnswer{errOut: "OSError: [Errno 5] Input/output error", code: 1}),
		"no device":   (&fakeNode{}).on("subsysnqn", fakeAnswer{errOut: "boom", code: 3}),
	} {
		if out, res := flushDevice(ctx, n, v); out != tr.OutcomeUnknown || res["detail"] == "" {
			t.Errorf("%s: %s %v; want unknown", name, out, res)
		}
	}
}

func TestFlushIsRegistered(t *testing.T) {
	if _, ok := genHandlers["v3_op_flush"]; !ok {
		t.Error("v3_op_flush is not in genHandlers")
	}
	if !strings.Contains(opIDs["flush"], ".") {
		t.Errorf("flush op id %q", opIDs["flush"])
	}
}
