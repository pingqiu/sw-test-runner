package hostloss

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
)

const raceKillNs = "1795408847399928288" // the raced fixture's kill receipt time

// stage copies judge fixtures into a fresh directory (receipts are written next to the inputs).
func stage(t *testing.T, names ...string) string {
	t.Helper()
	d := t.TempDir()
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join("..", "..", "internal", "hostloss", "testdata", n))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, n), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func run(t *testing.T, name string, params map[string]string) (map[string]string, error) {
	t.Helper()
	r := tr.NewRegistry()
	RegisterPack(r)
	h, err := r.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	return h.Execute(context.Background(), &tr.ActionContext{}, tr.Action{Action: name, Params: params})
}

func receipt(t *testing.T, path string) Receipt {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r Receipt
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRow1ReceiptAndOutputs(t *testing.T) {
	d := stage(t, "torn-live.tsv")
	out, err := run(t, "hostloss_row1", map[string]string{"ledger": filepath.Join(d, "torn-live.tsv")})
	if err != nil {
		t.Fatal(err)
	}
	if out["verdict"] != "PASS" || out["counted"] != "true" || out["acked"] != "199" {
		t.Fatalf("outputs: %v", out)
	}
	rc := receipt(t, out["receipt"])
	if rc.Row != "d1-lsn-exact" || rc.Verdict != "PASS" || !rc.Counted || len(rc.Evidence) != 1 || len(rc.Evidence[0].SHA256) != 64 || rc.Values["acked"] != "199" {
		t.Fatalf("receipt: %+v", rc)
	}
}

func writeRow1(t *testing.T, d string, acked string) string {
	t.Helper()
	p := filepath.Join(d, "row1.receipt.json")
	b, _ := json.Marshal(Receipt{Row: "d1-lsn-exact", Verdict: "PASS", Counted: true, Values: map[string]string{"acked": acked}})
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFinalRaceInputs(t *testing.T) {
	d := stage(t, "raced-kill.tsv")
	led := filepath.Join(d, "raced-kill.tsv")
	r1 := writeRow1(t, d, "150")
	out, err := run(t, "hostloss_final", map[string]string{"ledger": led, "row1_receipt": r1, "kill_ns": raceKillNs})
	if err != nil || out["verdict"] != "PASS" || out["raced"] != "true" {
		t.Fatalf("kill_ns race: %v %v", out, err)
	}
	if rc := receipt(t, out["receipt"]); len(rc.Evidence) != 2 {
		t.Fatalf("both inputs are evidence: %+v", rc.Evidence)
	}
	if out, err := run(t, "hostloss_final", map[string]string{"ledger": led, "row1_receipt": r1}); err == nil || out["verdict"] != "FAIL" || out["counted"] != "false" {
		t.Fatalf("no race instant must FAIL: %v %v", out, err)
	}
	if _, err := run(t, "hostloss_final", map[string]string{"ledger": led, "row1_receipt": r1, "kill_ns": raceKillNs, "fence_sec": "1795408847"}); err == nil || !strings.Contains(err.Error(), "exclusive") {
		t.Fatalf("kill_ns + fence_sec: %v", err)
	}
	if _, err := run(t, "hostloss_final", map[string]string{"ledger": led, "row1_receipt": led}); err == nil {
		t.Fatal("a non-receipt row1_receipt must be refused")
	}
	bad := writeRow1(t, d, "x")
	if _, err := run(t, "hostloss_final", map[string]string{"ledger": led, "row1_receipt": bad}); err == nil {
		t.Fatal("a receipt without an acked count must be refused")
	}
}

func TestNotCountedNeedsTheRehearsalFlag(t *testing.T) {
	d := t.TempDir()
	sha := strings.Repeat("a", 64)
	led := filepath.Join(d, "lsnless.tsv")
	// one acked write without an LSN that is NOT the last ack -> the image check cannot count the rep
	body := "I\t0\t0\t" + sha + "\t1\t1\t1\nA\t0\t0\t" + sha + "\t1\tNA\t1\t1\n" +
		"I\t1\t1\t" + sha + "\t5\t1\t1\nA\t1\t1\t" + sha + "\t5\t6\t1\t1\n"
	if err := os.WriteFile(led, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(d, "img")
	if err := os.WriteFile(img, make([]byte, 4096*4), 0o644); err != nil {
		t.Fatal(err)
	}
	p := map[string]string{"ledger": led, "image": img, "L": "3", "blocks": "4"}
	out, err := run(t, "hostloss_image", p)
	if err == nil || out["verdict"] != "NOT_COUNTED" || out["counted"] != "false" {
		t.Fatalf("NOT_COUNTED without the flag must fail the action: %v %v", out, err)
	}
	p["allow_not_counted"] = "true"
	out, err = run(t, "hostloss_image", p)
	if err != nil || out["verdict"] != "NOT_COUNTED" || out["counted"] != "false" {
		t.Fatalf("rehearsal flag: %v %v", out, err)
	}
	if rc := receipt(t, out["receipt"]); rc.Verdict != "NOT_COUNTED" || rc.Counted {
		t.Fatalf("receipt: %+v", rc)
	}
}

func TestLostRangeAndStopBound(t *testing.T) {
	d := stage(t, "grant.txt", "range.txt", "partition-ledger.tsv", "partition-journal.txt")
	out, err := run(t, "hostloss_lostrange", map[string]string{"grant": filepath.Join(d, "grant.txt"), "range": filepath.Join(d, "range.txt"), "request": "rep-fm1"})
	if err != nil || out["L"] != "52791" || out["new_epoch"] != "2" {
		t.Fatalf("lostrange: %v %v", out, err)
	}
	out, err = run(t, "hostloss_stopbound", map[string]string{"ledger": filepath.Join(d, "partition-ledger.tsv"), "journal": filepath.Join(d, "partition-journal.txt")})
	if err != nil || out["verdict"] != "PASS" {
		t.Fatalf("stopbound: %v %v", out, err)
	}
}

func TestMissingInputsRefused(t *testing.T) {
	for name, p := range map[string]map[string]string{
		"hostloss_row1":      {},
		"hostloss_final":     {"ledger": "nope"},
		"hostloss_image":     {"ledger": "nope", "image": "nope", "L": "1", "blocks": "1"},
		"hostloss_lostrange": {"grant": "nope"},
		"hostloss_stopbound": {"ledger": "nope", "journal": "nope"},
	} {
		if out, err := run(t, name, p); err == nil || out != nil {
			t.Errorf("%s: missing input accepted: %v", name, out)
		}
	}
}
