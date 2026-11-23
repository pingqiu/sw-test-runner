package hostloss

import (
	"fmt"
	"strings"
	"testing"
)

func TestLostRangeReal(t *testing.T) {
	r, lr := JudgeLostRange(string(fixture(t, "grant.txt")), string(fixture(t, "range.txt")), "rep-fm1")
	if r.Outcome != Pass || lr == nil || lr.L != 52791 || lr.OldEpoch != 1 || lr.NewEpoch != 2 || lr.Gen != 5 {
		t.Fatalf("%s %+v", r, lr)
	}
}

func TestLostRangeFailures(t *testing.T) {
	g, rg := string(fixture(t, "grant.txt")), string(fixture(t, "range.txt"))
	for _, c := range []struct {
		name, grant, ranges, req, want string
	}{
		{"other-request", g, rg, "rep-fm2", "grant request rep-fm1 != rep-fm2"},
		{"lost-after-recomputed", g, strings.Replace(rg, "lost after LSN 52791", "lost after LSN 52790", 1), "", "stored LostAfterLSN 52790 != grant kept remote LSN 52791"},
		{"epochs-differ", g, strings.Replace(rg, "r1@1 -> epoch 2", "r1@1 -> epoch 3", 1), "", "range epochs 1->3 != grant 1->2"},
		{"epoch-not-above", strings.Replace(g, "r1@2 on h1", "r1@1 on h1", 1), strings.Replace(rg, "-> epoch 2", "-> epoch 1", 1), "", "new epoch 1 not above old 1"},
		{"two-ranges-same-request", g, rg + strings.SplitN(rg, "\n", 2)[0] + "\n", "", "2 ranges for request rep-fm1"},
		{"no-range-for-request", g, strings.Replace(rg, "request rep-fm1", "request other", 1), "", "0 ranges for request rep-fm1"},
		{"two-grants", g + strings.SplitN(g, "\n", 2)[0] + "\n", rg, "", "unparsable: 2 grants"},
		{"no-range", g, "", "", "unparsable: 1 grants, 0 ranges"},
	} {
		r, _ := JudgeLostRange(c.grant, c.ranges, c.req)
		if r.Outcome != Fail || !strings.Contains(r.Text, c.want) {
			t.Errorf("%s: %s (want FAIL with %q)", c.name, r, c.want)
		}
	}
}

func TestStopBoundReal(t *testing.T) {
	l, err := ParseLedger(fixture(t, "partition-ledger.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	r := JudgeStopBound(l, string(fixture(t, "partition-journal.txt")))
	if r.Outcome != Pass || !strings.Contains(r.Text, "bound=1795405559588203707") || !strings.Contains(r.Text, "margin_ms=2") {
		t.Fatalf("%s", r)
	}
}

func TestStopBoundFailures(t *testing.T) {
	led := string(fixture(t, "partition-ledger.tsv"))
	j := string(fixture(t, "partition-journal.txt"))
	const U = 1795405559588203707
	sha := strings.Repeat("c", 64)
	late := led + fmt.Sprintf("I\t990000\t990000\t%s\t7\t1\t%d\nA\t990000\t990000\t%s\t7\t8\t1\t%d\n", sha, U, sha, U+1_000_000)
	noFence := dropLines(j, "workspace lease: fenced")
	for _, c := range []struct {
		name, led, journal, want string
	}{
		{"ack-after-the-bound", late, j, "1 acknowledged write(s) after the lease bound (first seq 990000, +1 ms)"},
		{"no-fence-line", led, noFence, "no fence line"},
		{"revoked-not-partitioned", led, strings.Replace(j, "lease expired at", "revoked by the master; lease expired at", 1), "partition was not effective"},
		{"no-fenced-stop", led, dropLines(j, fencedStop), "blockvolume did not stop"},
		{"no-bound-logged", led, dropLines(strings.Replace(j, "lease expired at", "lease ended at", 1), "lease valid until"), "no lease bound logged"},
	} {
		l, err := ParseLedger([]byte(c.led))
		if err != nil {
			t.Fatal(err)
		}
		if r := JudgeStopBound(l, c.journal); r.Outcome != Fail || !strings.Contains(r.Text, c.want) {
			t.Errorf("%s: %s (want FAIL with %q)", c.name, r, c.want)
		}
	}
	// the bound falls back to the last "lease valid until" when the fence line names no expiry instant
	l, _ := ParseLedger([]byte(led))
	if r := JudgeStopBound(l, strings.Replace(j, "lease expired at", "lease ended at", 1)); r.Outcome != Pass {
		t.Errorf("valid-until fallback: %s", r)
	}
}

func dropLines(s, sub string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if !strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
