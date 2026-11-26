package testrunner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func blockBinding(t *testing.T) BlockCloudBinding {
	t.Helper()
	dir := t.TempDir()
	ctxPath := filepath.ToSlash(filepath.Join(dir, "context.json"))
	if err := os.WriteFile(ctxPath, []byte(`{"schema":"seaweed.cloud-pack-context/v1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if filepath.VolumeName(ctxPath) != "" { // path.IsAbs wants a slash-rooted path
		ctxPath = ctxPath[len(filepath.VolumeName(ctxPath)):]
		t.Chdir(filepath.VolumeName(dir) + `\`)
	}
	return BlockCloudBinding{RunID: "r4-1", ContextPath: ctxPath, OutputDir: "/run/r4-1",
		WorkDeadline: time.Now().Add(time.Hour), HardDeadline: time.Now().Add(time.Hour + 20*time.Minute), RestoreReserve: 6 * time.Minute}
}

func agrees(context.Context, BlockCloudBinding) error { return nil }

func TestBlockScopeRefusesAMismatchedContext(t *testing.T) {
	b := blockBinding(t)
	mismatch := func(_ context.Context, got BlockCloudBinding) error {
		return errors.New("inventory run_id r4-2 is not the context's r4-1")
	}
	if s, err := NewBlockCloudRunScope(t.Context(), b, mismatch); err == nil || s != nil {
		t.Fatalf("a context the verifier refuses opened a scope: %v", err)
	}
	for name, mut := range map[string]func(*BlockCloudBinding){
		"no run id":                      func(b *BlockCloudBinding) { b.RunID = "" },
		"a relative context":             func(b *BlockCloudBinding) { b.ContextPath = "context.json" },
		"a missing context file":         func(b *BlockCloudBinding) { b.ContextPath = "/no/such/context.json" },
		"a reserve below a VM start":     func(b *BlockCloudBinding) { b.RestoreReserve = time.Minute },
		"a reserve past the hard bound":  func(b *BlockCloudBinding) { b.HardDeadline = b.WorkDeadline.Add(time.Minute) },
		"a work deadline already passed": func(b *BlockCloudBinding) { b.WorkDeadline = time.Now().Add(-time.Second) },
	} {
		c := b
		mut(&c)
		if s, err := NewBlockCloudRunScope(t.Context(), c, agrees); err == nil || s != nil {
			t.Errorf("%s: opened", name)
		}
	}
	if _, err := NewBlockCloudRunScope(t.Context(), b, nil); err == nil {
		t.Error("no verifier: opened")
	}
}

func TestBlockScopeRefusesEveryJob(t *testing.T) {
	s, err := NewBlockCloudRunScope(t.Context(), blockBinding(t), agrees)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// A well-formed job, so the refusal is the backend's own, not a spec check.
	_, err = s.Start(JobSpec{Node: "n1", Executable: "/bin/true", SHA256: strings.Repeat("a", 64)})
	if err == nil {
		t.Fatal("a block cloud scope started a job")
	}
	if !errors.Is(err, errNoJobs) {
		t.Fatalf("refused, but not by the block backend: %v", err)
	}
}

// The power fault in a block scope: applied, then restored by Close, with the
// history holding two Runner operations, each invoke before complete.
func TestBlockScopeRestoresThePowerFaultAndRecordsIt(t *testing.T) {
	s, err := NewBlockCloudRunScope(t.Context(), blockBinding(t), agrees)
	if err != nil {
		t.Fatal(err)
	}
	hp := filepath.Join(t.TempDir(), "history.jsonl")
	h, err := OpenHistory(hp)
	if err != nil {
		t.Fatal(err)
	}
	v := newFakeVM()
	p := v.plan()
	p.History, p.Client = h, "runner"
	if err := s.ApplyWitnessedPowerFault(t.Context(), p.Key(), p); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if v.ons != 1 {
		t.Fatalf("not restored at close: ons=%d", v.ons)
	}
	_ = h.Close()
	recs := readRecords(t, hp)
	type step struct{ kind, verb, op, outcome string }
	var got []step
	for _, r := range recs {
		got = append(got, step{r.Kind, r.Verb, r.Op, r.Outcome})
	}
	want := []step{
		{HistoryInvoke, "fault_apply", "Runner.VmPowerOff", ""},
		{HistoryComplete, "", "", OutcomeOK},
		{HistoryInvoke, "fault_restore", "Runner.VmPowerOn", ""},
		{HistoryComplete, "", "", OutcomeOK},
	}
	if len(got) < len(want) {
		t.Fatalf("history %+v", got)
	}
	for i, w := range want {
		g := got[i]
		if g.kind != w.kind || (w.verb != "" && (g.verb != w.verb || g.op != w.op)) || g.outcome != w.outcome {
			t.Fatalf("record %d = %+v, want %+v (all %+v)", i, g, w, got)
		}
	}
	if recs[1].Of != recs[0].Seq || recs[3].Of != recs[2].Seq {
		t.Fatalf("completes not bound to their invokes: %+v", recs)
	}
	if recs[0].Args["fault_key"] != p.Key() || recs[0].Args["vm_id"] != "vmid-1" || recs[0].Args["resource"] != powerVM {
		t.Fatalf("apply args %+v", recs[0].Args)
	}
	if recs[3].Result["power"] != "running" || recs[3].Result["boot_changed"] != true {
		t.Fatalf("restore result %+v", recs[3].Result)
	}
}

// A restore that cannot be proven completes UNKNOWN, never ok.
func TestPowerFaultUnprovenRestoreIsUnknownInHistory(t *testing.T) {
	s, err := NewBlockCloudRunScope(t.Context(), blockBinding(t), agrees)
	if err != nil {
		t.Fatal(err)
	}
	hp := filepath.Join(t.TempDir(), "history.jsonl")
	h, _ := OpenHistory(hp)
	v := newFakeVM()
	v.afterOn = func(v *fakeVM) { v.boot = "boot-1" } // back with the old boot
	p := v.plan()
	p.History = h
	if err := s.ApplyWitnessedPowerFault(t.Context(), p.Key(), p); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	_ = h.Close()
	recs := readRecords(t, hp)
	if len(recs) < 4 || recs[2].Verb != "fault_restore" || recs[3].Outcome != OutcomeUnknown {
		t.Fatalf("an unproven restore recorded as %+v", recs)
	}
}

func readRecords(t *testing.T, p string) []HistoryRecord {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []HistoryRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r HistoryRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		if r.Kind != HistoryEnd {
			out = append(out, r)
		}
	}
	return out
}
