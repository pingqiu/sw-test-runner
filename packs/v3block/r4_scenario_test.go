package v3block

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/actions"
	"github.com/pingqiu/sw-test-runner/packs/cloud"
)

// The R4 node-loss scenario parses, and every action it uses is registered
// (the engine would refuse an unknown one only at run time, on the VMs).
// It restores its power fault explicitly before the judge, and collects
// its evidence in an always phase.
func TestR4ScenarioUsesOnlyRegisteredActions(t *testing.T) {
	s, err := tr.ParseFile("../../scenarios/v3block/r4-node-loss.yaml")
	if err != nil {
		t.Fatal(err)
	}
	r := tr.NewRegistry()
	actions.RegisterCore(r)
	actions.RegisterBlockActions(r)
	actions.RegisterNVMeActions(r)
	RegisterPack(r)
	cloud.RegisterPack(r)
	r.AllowMutating = true // the run passes --allow-mutating (the power fault)
	var seen []string
	collectAlways := false
	for _, ph := range s.Phases {
		for _, a := range ph.Actions {
			if _, err := r.Get(a.Action); err != nil {
				t.Errorf("phase %s: action %q is not registered", ph.Name, a.Action)
			}
			seen = append(seen, a.Action)
		}
		if ph.Name == "collect" && ph.Always {
			collectAlways = true
		}
	}
	joined := " " + strings.Join(seen, " ") + " "
	off, on := strings.Index(joined, " cloud_vm_power "), strings.Index(joined, " cloud_vm_power_restore ")
	if off < 0 || on < 0 || on < off {
		t.Fatalf("the power fault is not applied and then restored explicitly: %v", seen)
	}
	if !collectAlways {
		t.Fatal("the evidence is not collected in an always phase")
	}
}

// r4 runs the scenario's check script on files (the controls): direct I/O
// flags off, polling at once.
func r4(t *testing.T, args ...string) (string, error) {
	return r4Env(t, nil, args...)
}

func r4Env(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Linux shell/device controls; no host resources")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	cmd := exec.Command("bash", append([]string{"../../scenarios/v3block/r4.sh"}, args...)...)
	cmd.Env = append(os.Environ(), "R4_DD_OFLAG=sync", "R4_DD_IFLAG=fullblock", "R4_POLL=0", "R4_ALLOW_FILE=1")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func r4Device(t *testing.T) (dev, ledger string) {
	t.Helper()
	dir := t.TempDir()
	dev, ledger = filepath.Join(dir, "dev"), filepath.Join(dir, "ledger")
	if err := os.WriteFile(dev, make([]byte, 64*4096), 0o600); err != nil {
		t.Fatal(err)
	}
	return dev, ledger
}

// P4: every acked write reads back; a block changed after its ack (an
// acknowledged write lost) fails the check.
func TestR4VerifyFailsOnALostAckedWrite(t *testing.T) {
	dev, ledger := r4Device(t)
	if out, err := r4(t, "write", dev, ledger, "before", "16"); err != nil {
		t.Fatalf("write: %v %s", err, out)
	}
	if out, err := r4(t, "verify", dev, ledger); err != nil || !strings.Contains(out, "mismatches=0") {
		t.Fatalf("verify on intact writes: %v %s", err, out)
	}
	// Lose one acknowledged write: overwrite its block.
	line := strings.Fields(strings.SplitN(mustRead(t, ledger), "\n", 2)[0])
	f, err := os.OpenFile(dev, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	var off int64
	for _, c := range line[3] {
		off = off*10 + int64(c-'0')
	}
	if _, err := f.WriteAt(make([]byte, 4096), off); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if out, err := r4(t, "verify", dev, ledger); err == nil || !strings.Contains(out, "MISMATCH") {
		t.Fatalf("verify passed with a lost acked write: %s", out)
	}
}

// The scenario's own red control: one dropped acked write must fail verify.
func TestR4DropControlDetectsADroppedWrite(t *testing.T) {
	dev, ledger := r4Device(t)
	if out, err := r4(t, "write", dev, ledger, "before", "8"); err != nil {
		t.Fatalf("write: %v %s", err, out)
	}
	if out, err := r4(t, "drop-control", dev, ledger); err != nil {
		t.Fatalf("drop-control: %v %s", err, out)
	}
	if out, err := r4(t, "verify", dev, ledger); err != nil {
		t.Fatalf("the control changed the run's ledger: %v %s", err, out)
	}
}

// P4b: an acknowledged write in the window is a finding (the check fails);
// a write that is not acknowledged passes.
func TestR4NoAckFailsOnAnAcknowledgedWrite(t *testing.T) {
	dev, ledger := r4Device(t)
	if out, err := r4(t, "no-ack", dev, ledger, "5"); err == nil || !strings.Contains(out, "ACKNOWLEDGED") {
		t.Fatalf("no-ack passed although the write was acknowledged: %s", out)
	}
	unwritable := filepath.Join(t.TempDir(), "missing-dir", "dev")
	if out, err := r4(t, "no-ack", unwritable, ledger, "5"); err == nil || !strings.Contains(out, "setup failure") {
		t.Fatalf("no-ack accepted a missing device: %v %s", err, out)
	}
}

func statusFile(t *testing.T, json string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "status.json")
	if err := os.WriteFile(p, []byte(json), 0o600); err != nil {
		t.Fatal(err)
	}
	return "cat " + p
}

// P2: a survivor that becomes primary, or leaves the epoch, fails the hold.
func TestR4HeldFailsOnAPromotion(t *testing.T) {
	quiet := statusFile(t, `{"AuthorityRole":"unknown","Epoch":1,"Healthy":false,"ReplicationRole":"replica_ready"}`)
	if out, err := r4(t, "held", quiet, "--", "0", "1"); err != nil {
		t.Fatalf("held on a quiet survivor: %v %s", err, out)
	}
	for name, s := range map[string]string{
		"promoted":  `{"AuthorityRole":"primary","Epoch":2,"Healthy":true,"ReplicationRole":"none"}`,
		"new epoch": `{"AuthorityRole":"unknown","Epoch":2,"Healthy":false,"ReplicationRole":"replica_ready"}`,
	} {
		if out, err := r4(t, "held", statusFile(t, s), "--", "0", "1"); err == nil {
			t.Fatalf("%s: held passed: %s", name, out)
		}
	}
}

// P3/P5: the role is read at the stated epoch; the wrong role or epoch fails.
func TestR4WaitRoleFailsOnTheWrongRoleOrEpoch(t *testing.T) {
	primary := statusFile(t, `{"AuthorityRole":"primary","Epoch":2,"Healthy":true,"ReplicationRole":"none"}`)
	replica := statusFile(t, `{"AuthorityRole":"superseded","Epoch":2,"Healthy":false,"ReplicationRole":"replica_ready"}`)
	if out, err := r4(t, "wait-role", primary, "primary", "2", "1"); err != nil {
		t.Fatalf("primary: %v %s", err, out)
	}
	if out, err := r4(t, "wait-role", replica, "replica", "2", "1"); err != nil {
		t.Fatalf("replica: %v %s", err, out)
	}
	for name, c := range map[string][]string{
		"primary asked of a replica": {replica, "primary", "2", "1"},
		"replica asked of a primary": {primary, "replica", "2", "1"},
		"wrong epoch":                {primary, "primary", "3", "1"},
	} {
		if out, err := r4(t, append([]string{"wait-role"}, c...)...); err == nil {
			t.Fatalf("%s: wait-role passed: %s", name, out)
		}
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
