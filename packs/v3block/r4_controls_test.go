package v3block

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
)

func r4File(t *testing.T, path, contents string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestR4ShortWriteAndMissingRoundFail(t *testing.T) {
	dev, ledger := r4Device(t)
	fail := r4File(t, filepath.Join(t.TempDir(), "dd"), "#!/bin/sh\nexit 1\n")
	if out, err := r4Env(t, []string{"R4_DD=" + fail}, "write", dev, ledger, "before", "1"); err == nil || !strings.Contains(out, "only 0 of 1") {
		t.Fatalf("short write passed: %v %s", err, out)
	}
	if out, err := r4(t, "write", dev, ledger, "before", "2"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if out, err := r4(t, "verify", dev, ledger, "before", "after-rejoin"); err == nil || !strings.Contains(out, "no acknowledged write of round") {
		t.Fatalf("missing round passed: %v %s", err, out)
	}
}

func TestR4NoAckRejectsToolErrorsAndAllowsOnlyRefusalOrPending(t *testing.T) {
	dev, ledger := r4Device(t)
	for _, tc := range []struct {
		name, script string
		good         bool
	}{
		{"io-refusal", "echo 'Input/output error' >&2; exit 1", true},
		{"tool-failure", "echo 'unrecognized option' >&2; exit 1", false},
		{"pending", "sleep 2; exit 1", true},
		{"acked", "exit 0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := r4File(t, filepath.Join(t.TempDir(), "dd"), "#!/bin/sh\n"+tc.script+"\n")
			out, err := r4Env(t, []string{"R4_DD=" + stub}, "no-ack", dev, ledger, "1")
			if (err == nil) != tc.good {
				t.Fatalf("good=%v: %v %s", tc.good, err, out)
			}
		})
	}
}

func TestR4DeviceDiscoveryRediscoversAndRejectsAmbiguity(t *testing.T) {
	dir := t.TempDir()
	sys := filepath.Join(dir, "sys")
	devices := filepath.Join(dir, "dev")
	r4File(t, filepath.Join(sys, "nvme-subsys9", "subsysnqn"), "other\n")
	ours := filepath.Join(sys, "nvme-subsys1")
	r4File(t, filepath.Join(ours, "subsysnqn"), "our-nqn\n")
	controller := filepath.Join(ours, "nvme7")
	r4File(t, filepath.Join(controller, "state"), "live\n")
	r4File(t, filepath.Join(controller, "address"), "traddr=10.2.3.4,trsvcid=4420\n")
	r4File(t, filepath.Join(ours, "nvme7n1"), "")
	r4File(t, filepath.Join(devices, "nvme7n1"), "")
	env := []string{"R4_SYS=" + sys, "R4_DEVDIR=" + devices}
	check := func(good bool, want string) {
		t.Helper()
		out, err := r4Env(t, env, "dev", "our-nqn", "10.2.3.4", "0")
		if (err == nil) != good || (good && strings.TrimSpace(out) != want) {
			t.Fatalf("good=%v: %v %s", good, err, out)
		}
	}
	check(true, filepath.Join(devices, "nvme7n1")) // unrelated subsystem sorts last
	os.Remove(filepath.Join(ours, "nvme7n1"))
	os.Remove(filepath.Join(devices, "nvme7n1"))
	r4File(t, filepath.Join(ours, "nvme12n1"), "")
	r4File(t, filepath.Join(devices, "nvme12n1"), "")
	check(true, filepath.Join(devices, "nvme12n1"))
	r4File(t, filepath.Join(controller, "state"), "dead\n")
	check(false, "")
	r4File(t, filepath.Join(controller, "state"), "live\n")
	r4File(t, filepath.Join(ours, "nvme12n2"), "")
	check(false, "")
	os.Remove(filepath.Join(ours, "nvme12n2"))
	r4File(t, filepath.Join(sys, "nvme-subsys2", "subsysnqn"), "our-nqn\n")
	check(false, "")
}

func TestR4OwnedUnitsFreshInstallAndFailedCleanupPreventPurge(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	units := filepath.Join(dir, "units")
	hosts := r4File(t, filepath.Join(dir, "hosts"), "127.0.0.1 localhost\n")
	os.MkdirAll(units, 0755)
	ctl := r4File(t, filepath.Join(dir, "systemctl"), "#!/bin/sh\nif [ \"$1\" = is-active ]; then echo \"${R4_TEST_ACTIVE:-inactive}\"; exit 0; fi\nexit 0\n")
	journal := r4File(t, filepath.Join(dir, "journalctl"), "#!/bin/sh\nexit 0\n")
	pgrep := r4File(t, filepath.Join(dir, "pgrep"), "#!/bin/sh\nexit ${R4_TEST_PGREP:-1}\n")
	env := []string{"R4_ROOT=" + root, "R4_UNITDIR=" + units, "R4_HOSTS=" + hosts, "R4_SYSTEMCTL=" + ctl, "R4_JOURNALCTL=" + journal, "R4_PGREP=" + pgrep}
	w := filepath.Join(root, "test")
	r4File(t, filepath.Join(w, ".r4-owner"), "test\n")
	// An existing root must be refused, even if no package is supplied.
	if out, err := r4Env(t, env, "install", "/missing-package", "test", "10.2.3.4"); err == nil || !strings.Contains(out, "not fresh") {
		t.Fatalf("%v %s", err, out)
	}
	if out, err := r4Env(t, env, "unit", "test", "volume", "--", "/bin/true", "unsafe argument"); err == nil {
		t.Fatalf("unsafe unit passed: %s", out)
	}
	if out, err := r4Env(t, env, "unit", "test", "volume", "--", "/bin/true"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	body := mustRead(t, filepath.Join(units, "r4-test-volume.service"))
	if !strings.Contains(body, "RestartPreventExitStatus=78") || strings.Contains(body, "[Install]") {
		t.Fatal(body)
	}
	if out, err := r4Env(t, env, "start", "test", "volume"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	for _, extra := range []string{"R4_TEST_ACTIVE=active", "R4_TEST_PGREP=0"} {
		bad := append(append([]string{}, env...), extra)
		if out, err := r4Env(t, bad, "cleanup", "test"); err == nil {
			t.Fatalf("failed absence passed: %s", out)
		}
		if out, err := r4Env(t, bad, "purge", "test"); err == nil {
			t.Fatalf("purged after failed cleanup: %s", out)
		}
	}
	if out, err := r4Env(t, env, "cleanup", "test"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if out, err := r4Env(t, env, "purge", "test"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if _, err := os.Stat(w); !os.IsNotExist(err) {
		t.Fatalf("work remains: %v", err)
	}
}

// Run the actual engine with inert handlers. This catches nested variables,
// wrong parameters, omitted cleanup hosts, and RF-specific pass-line drift.
func TestR4RendersBothModesWithoutNestedTemplates(t *testing.T) {
	for _, rf := range []string{"2", "3"} {
		t.Run(rf, func(t *testing.T) {
			s, err := tr.ParseFile("../../scenarios/v3block/r4-node-loss.yaml")
			if err != nil {
				t.Fatal(err)
			}
			s.Env["rf"] = rf
			s.Env["run_id"] = "test-run"
			for _, p := range s.Phases {
				if p.Name == "hold" && (!p.Parallel || len(p.Actions) != 2) {
					t.Fatal("survivors must be observed concurrently")
				}
			}
			if s.Phases[0].Name != "preflight" {
				t.Fatal("preflight must precede host work")
			}
			counts := map[string]int{}
			starts := 0
			connects := 0
			failover := false
			rejoin := false
			post := false
			handler := func(_ context.Context, _ *tr.ActionContext, a tr.Action) (map[string]string, error) {
				for k, v := range a.Params {
					if strings.Contains(v, "{{") {
						return nil, fmt.Errorf("unresolved %s=%s", k, v)
					}
				}
				cmd := a.Params["cmd"]
				if strings.Contains(cmd, "/bin/blockvolume ") {
					starts++
					if !strings.Contains(cmd, "--allow-external-nvme-bind") {
						return nil, fmt.Errorf("missing NVMe permission")
					}
				}
				if strings.Contains(cmd, "systemd-run") {
					return nil, fmt.Errorf("transient unit")
				}
				if strings.Contains(cmd, "r4.sh connect ") {
					connects++
				}
				if strings.Contains(cmd, "cleanup $T") {
					counts[a.Node]++
				}
				if strings.Contains(cmd, "ops failover ") {
					failover = strings.Contains(cmd, "--expect-current r1@1 --confirm-old-node-off v1")
				}
				if strings.Contains(cmd, "r4.sh start test-run-rf"+rf+" volume") && a.Node == "node-2" {
					rejoin = true
				}
				if strings.Contains(cmd, "verify \"$DEV\" $W/ledger before after-rejoin") {
					post = true
				}
				return nil, nil
			}
			reg := tr.NewRegistry()
			for _, n := range []string{"exec", "upload_path", "collect_path", "cloud_vm_power", "cloud_vm_power_restore"} {
				reg.RegisterFunc(n, tr.TierCore, handler)
			}
			result := tr.NewEngine(reg, nil).Run(context.Background(), s, &tr.ActionContext{Scenario: s, Vars: map[string]string{}, Log: func(string, ...interface{}) {}})
			if result.Status != tr.StatusPass || starts != 3 || connects != 2 || !failover || !rejoin || !post {
				t.Fatalf("%s %s starts=%d connects=%d failover=%v rejoin=%v post=%v", result.Status, result.Error, starts, connects, failover, rejoin, post)
			}
			for _, n := range []string{"node-1", "node-2", "node-3"} {
				if counts[n] != 1 {
					t.Fatalf("cleanup %s=%d", n, counts[n])
				}
			}
		})
	}
}
