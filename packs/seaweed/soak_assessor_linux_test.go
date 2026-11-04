//go:build linux

package seaweed

import (
	"github.com/pingqiu/sw-test-runner/inventory"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheAdmittedAssessorArgvRuns(t *testing.T) {
	helpers, _ := filepath.Abs("worker")
	root := t.TempDir()
	d := &inventory.Document{RunID: "run-1", Hosts: []inventory.Host{{ID: "m01", RunRoot: root}},
		Observation: &inventory.Observation{Hosts: []inventory.HostObservation{{HostID: "m01", BootID: "boot-1"}, {HostID: "m02", BootID: "boot-2"}}}}
	argv, err := SoakAssessArgs(d, SmokeSettings{HelperDir: helpers}, &SoakPlan{DurationS: 1800, ClientKillAtS: 1320, Layout: AssignLayout{Collection: "lmcache"}})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", argv...)
	cmd.Dir = t.TempDir()
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("admitted argv failed: %v", err)
	}
	v, err := decodeVerdict(captured(raw), SoakBinding{SoakVerdictSchema, "run-1", "boot-1", root}, soakCriteria)
	if err != nil || v.Status != "FAIL" || !strings.Contains(v.Error, "M01 boot") {
		t.Fatalf("want this run's FAIL row (empty evidence, foreign boot), got %+v %v", v, err)
	}
}

func TestTheAdmittedAssessorPassRowDecodes(t *testing.T) {
	helpers, _ := filepath.Abs("worker")
	root := t.TempDir()
	write := exec.Command("python3", "-B", "-c", "import sys, test_soak_assess as t; from pathlib import Path; t.write(Path(sys.argv[1]), t.evidence())", root)
	write.Dir = helpers
	if out, err := write.CombinedOutput(); err != nil {
		t.Fatalf("synthetic evidence: %v %s", err, out)
	}
	bootRaw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	boot := strings.TrimSpace(string(bootRaw))
	d := &inventory.Document{RunID: "run-1", Hosts: []inventory.Host{{ID: "m01", RunRoot: root}},
		Observation: &inventory.Observation{Hosts: []inventory.HostObservation{{HostID: "m01", BootID: boot}, {HostID: "m02", BootID: "m02-boot"}}}}
	argv, err := SoakAssessArgs(d, SmokeSettings{HelperDir: helpers}, &SoakPlan{DurationS: 1800, ClientKillAtS: 1320, Layout: AssignLayout{Collection: "lmcache"}})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", argv...)
	cmd.Dir = t.TempDir()
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("admitted argv failed: %v", err)
	}
	v, err := decodeVerdict(captured(raw), SoakBinding{SoakVerdictSchema, "run-1", boot, root}, soakCriteria)
	if err != nil || v.Status != "PASS" || !strings.Contains(v.MissesQualification.Method, "no parity claim") {
		t.Fatalf("want the real PASS row decoded, got %+v %v\n%s", v, err, raw)
	}
}
