package v3block

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/declared"
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

// deviceRun runs one declared device action on a simulated host over f and
// returns its records.
func deviceRun(t *testing.T, action string, f *fakeNode, params map[string]string) (map[string]string, []tr.HistoryRecord) {
	t.Helper()
	actx, p := deadlineCtx(t, &declared.SimHost{Inner: f})
	out, err := blockActions.Handler(action)(context.Background(), actx, tr.Action{Params: params})
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
	_ = actx.History.Close()
	return out, historyRecords(t, p)
}

// A flushed write is today's command (conv=fsync) and today's record (no
// flush key); an unflushed one drops only the fsync and says "flush": false.
func TestWriteFlushVariant(t *testing.T) {
	gen := fakeAnswer{out: blockSHAFrozen("v1|g1|7|") + "\n"}
	f := (&fakeNode{}).on("python3 -c", gen).on("subsysnqn", ok(oneDevice)).on("dd if=", ok(""))
	_, rs := deviceRun(t, "v3_op_write", f, map[string]string{"volume": "v1", "round": "g1", "lbas": "7"})
	if !f.issued("dd if=/w/root/.block-v1-7 of=/dev/nvme3n1 bs=4096 seek=7 count=1 oflag=direct conv=fsync,notrunc status=none") {
		t.Errorf("flushed write changed: %q", f.calls)
	}
	if _, has := rs[0].Args["flush"]; has {
		t.Error("a flushed write's record carries a flush key; today's records must stay as they are")
	}
	f = (&fakeNode{}).on("python3 -c", gen).on("subsysnqn", ok(oneDevice)).on("dd if=", ok(""))
	out, rs := deviceRun(t, "v3_op_write", f, map[string]string{"volume": "v1", "round": "g1", "lbas": "7", "flush": "false"})
	if f.issued("fsync") || !f.issued("oflag=direct") || !f.issued("conv=notrunc") {
		t.Errorf("unflushed write: %q", f.calls)
	}
	if out["value"] != tr.OutcomeOK || len(rs) != 3 || rs[0].Verb != "write" || rs[0].Args["flush"] != false {
		t.Fatalf("records %+v", rs)
	}
}

// v3_op_flush: fsync of the device; ok only on exit 0. Red control: a flush
// whose fsync fails, or whose device cannot be established, is unknown, never ok.
func TestFlushOutcome(t *testing.T) {
	f := (&fakeNode{}).on("subsysnqn", ok(oneDevice)).on("os.fsync", ok(""))
	out, rs := deviceRun(t, "v3_op_flush", f, map[string]string{"volume": "v1"})
	if out["value"] != tr.OutcomeOK || rs[1].Result["device"] != "/dev/nvme3n1" {
		t.Fatalf("good flush: %v %+v", out, rs)
	}
	if !f.issued(flushCmdFrozen("/dev/nvme3n1")) {
		t.Error("the flush command was not issued on the volume's device")
	}
	for name, n := range map[string]*fakeNode{
		"fsync fails": (&fakeNode{}).on("subsysnqn", ok(oneDevice)).on("os.fsync", fakeAnswer{errOut: "OSError: [Errno 5] Input/output error", code: 1}),
		"no device":   (&fakeNode{}).on("subsysnqn", fakeAnswer{errOut: "boom", code: 3}),
	} {
		if out, rs := deviceRun(t, "v3_op_flush", n, map[string]string{"volume": "v1"}); out["value"] != tr.OutcomeUnknown || rs[1].Result["detail"] == "" {
			t.Errorf("%s: %v %+v; want unknown", name, out, rs)
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
