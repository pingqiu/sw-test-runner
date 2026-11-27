package v3block

import (
	"context"
	"crypto/sha256"
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
	ctl := r4File(t, filepath.Join(dir, "systemctl"), "#!/bin/sh\nif [ \"$1\" = is-active ]; then [ -z \"${R4_TEST_CTL_ERROR:-}\" ] || exit 1; echo \"${R4_TEST_ACTIVE:-inactive}\"; exit 0; fi\nexit 0\n")
	journal := r4File(t, filepath.Join(dir, "journalctl"), "#!/bin/sh\nexit 0\n")
	pgrep := r4File(t, filepath.Join(dir, "pgrep"), "#!/bin/sh\nexit ${R4_TEST_PGREP:-1}\n")
	env := []string{"R4_ROOT=" + root, "R4_TMP=" + dir, "R4_UNITDIR=" + units, "R4_HOSTS=" + hosts, "R4_SYSTEMCTL=" + ctl, "R4_JOURNALCTL=" + journal, "R4_PGREP=" + pgrep}
	w := filepath.Join(root, "test")
	pkg := filepath.Join(dir, "pkg")
	r4File(t, filepath.Join(pkg, "payload"), "package\n")
	r4File(t, filepath.Join(pkg, "SHA256SUMS"), fmt.Sprintf("%x  payload\n", sha256.Sum256([]byte("package\n"))))
	// The cloud controller builds under umask 077, so its uploaded manifest
	// remains private when root installs and later copies it into evidence.
	if err := os.Chmod(filepath.Join(pkg, "SHA256SUMS"), 0600); err != nil {
		t.Fatal(err)
	}
	privateKey := r4File(t, filepath.Join(pkg, "tls", "node.key"), "test-only private material\n")
	if err := os.Chmod(privateKey, 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := r4Env(t, env, "install", pkg, "test", "10.2.3.4"); err != nil {
		t.Fatalf("fresh install: %v %s", err, out)
	}
	if strings.TrimSpace(mustRead(t, filepath.Join(w, ".r4-owner"))) != "test" {
		t.Fatal("missing ownership marker")
	}
	if info, err := os.Stat(filepath.Join(w, "master")); err != nil || !info.IsDir() {
		t.Fatalf("fresh install has no audit-log parent: %v", err)
	}
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
	if out, err := r4Env(t, env, "unit", "test", "writer", "--", "/bin/true"); err != nil {
		t.Fatalf("writer unit: %v %s", err, out)
	}
	writerBody := mustRead(t, filepath.Join(units, "r4-test-writer.service"))
	if !strings.Contains(writerBody, "TimeoutStopSec=210s") || !strings.Contains(writerBody, "Restart=no") {
		t.Fatal("writer must drain documented controller lifetime without restart: " + writerBody)
	}
	if out, err := r4Env(t, env, "start", "test", "volume"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	for _, extra := range []string{"R4_TEST_ACTIVE=active", "R4_TEST_PGREP=0", "R4_TEST_PGREP=2", "R4_TEST_CTL_ERROR=1"} {
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
	for _, path := range []string{filepath.Join(w, "evidence"), filepath.Join(w, "evidence", "SHA256SUMS")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0004 == 0 || (info.IsDir() && info.Mode().Perm()&0001 == 0) {
			t.Fatalf("unprivileged collector cannot read evidence: %s mode=%o", path, info.Mode().Perm())
		}
	}
	if _, err := os.Stat(filepath.Join(w, "evidence", "tls")); !os.IsNotExist(err) {
		t.Fatalf("private TLS material copied into evidence: %v", err)
	}
	if info, err := os.Stat(filepath.Join(w, "tls", "node.key")); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private TLS permissions changed: info=%v err=%v", info, err)
	}
	if out, err := r4Env(t, env, "purge", "test"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if _, err := os.Stat(w); !os.IsNotExist(err) {
		t.Fatalf("work remains: %v", err)
	}
}

func TestR4FailedPreflightPreventsHostWork(t *testing.T) {
	s, err := tr.ParseFile("../../scenarios/v3block/r4-node-loss.yaml")
	if err != nil {
		t.Fatal(err)
	}
	normal := 0
	handler := func(_ context.Context, _ *tr.ActionContext, a tr.Action) (map[string]string, error) {
		// The only normal action reached must be the source preflight. Always
		// actions still settle; their host cleanup must independently fail closed.
		if strings.Contains(a.Params["cmd"], "PREFLIGHT PASS") {
			normal++
			return nil, fmt.Errorf("source flag removed")
		}
		if a.Action == "upload_path" || a.Action == "cloud_vm_power" || strings.Contains(a.Params["cmd"], " unit $T") {
			normal++
		}
		return nil, nil
	}
	r := tr.NewRegistry()
	for _, n := range []string{"exec", "upload_path", "collect_path", "cloud_vm_power", "cloud_vm_power_restore"} {
		r.RegisterFunc(n, tr.TierCore, handler)
	}
	result := tr.NewEngine(r, nil).Run(context.Background(), s, &tr.ActionContext{Scenario: s, Vars: map[string]string{}, Log: func(string, ...interface{}) {}})
	if result.Status != tr.StatusFail || normal != 1 {
		t.Fatalf("%s normal=%d %s", result.Status, normal, result.Error)
	}
}

func TestR4EvidenceIncludesInactiveAndMissingUnits(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	w := filepath.Join(root, "test")
	r4File(t, filepath.Join(w, ".r4-owner"), "test\n")
	ctl := r4File(t, filepath.Join(dir, "systemctl"), "#!/bin/sh\n[ \"$1\" = status ] || exit 99\necho inactive\nexit 3\n")
	journal := r4File(t, filepath.Join(dir, "journalctl"), "#!/bin/sh\necho journal-for-$2\nexit ${R4_TEST_JOURNAL_ERROR:-0}\n")
	env := []string{"R4_ROOT=" + root, "R4_SYSTEMCTL=" + ctl, "R4_JOURNALCTL=" + journal}
	if out, err := r4Env(t, env, "evidence", "test"); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	for _, role := range []string{"master", "volume", "writer"} {
		p := filepath.Join(w, "evidence", "r4-test-"+role+".journal")
		if !strings.Contains(mustRead(t, p), "journal-for-r4-test-"+role) {
			t.Fatal(p)
		}
	}
	bad := append(append([]string{}, env...), "R4_TEST_JOURNAL_ERROR=1")
	if out, err := r4Env(t, bad, "evidence", "test"); err == nil || !strings.Contains(out, "incomplete journal capture") {
		t.Fatalf("journal failure hidden: %v %s", err, out)
	}
	if _, err := os.Stat(w); err != nil {
		t.Fatalf("failed capture removed evidence: %v", err)
	}
}

// Exercise the real always phase after failure at each normal action. Evidence
// must reach the collector before any host stops/removes its units.
func TestR4EveryExitCollectsJournalsBeforeCleanup(t *testing.T) {
	for failAt := -1; ; failAt++ {
		s, err := tr.ParseFile("../../scenarios/v3block/r4-node-loss.yaml")
		if err != nil {
			t.Fatal(err)
		}
		normal, failed := 0, false
		snap, collected, cleaned := map[string]bool{}, map[string]bool{}, map[string]bool{}
		h := func(_ context.Context, _ *tr.ActionContext, a tr.Action) (map[string]string, error) {
			cmd := a.Params["cmd"]
			if strings.Contains(cmd, " evidence $T") {
				snap[a.Node] = true
				return nil, nil
			}
			if a.Action == "collect_path" && a.Node != "controller" {
				if !snap[a.Node] || cleaned[a.Node] {
					return nil, fmt.Errorf("journal collection order on %s", a.Node)
				}
				collected[a.Node] = true
				if failAt == -1 && a.Node == "node-1" {
					failed = true
					return nil, fmt.Errorf("injected archive failure")
				}
				return nil, nil
			}
			if strings.Contains(cmd, " cleanup $T") {
				if !collected[a.Node] {
					return nil, fmt.Errorf("cleanup before journal collection on %s", a.Node)
				}
				cleaned[a.Node] = true
				return nil, nil
			}
			if strings.Contains(cmd, " purge $T") {
				return nil, fmt.Errorf("purge can erase a failed collection")
			}
			if a.Action == "collect_path" || strings.Contains(cmd, "L=/tmp/r4-") {
				return nil, nil
			}
			normal++
			if normal-1 == failAt {
				failed = true
				return nil, fmt.Errorf("injected normal failure")
			}
			if strings.Contains(cmd, "auto-p2.py wait ") {
				return map[string]string{"value": "test-promotion-token"}, nil
			}
			return nil, nil
		}
		r := tr.NewRegistry()
		for _, n := range []string{"exec", "upload_path", "collect_path", "cloud_vm_power", "cloud_vm_power_restore"} {
			r.RegisterFunc(n, tr.TierCore, h)
		}
		result := tr.NewEngine(r, nil).Run(context.Background(), s, &tr.ActionContext{Scenario: s, Vars: map[string]string{}, Log: func(string, ...interface{}) {}})
		for _, node := range []string{"node-1", "node-2", "node-3"} {
			if !snap[node] || !collected[node] || !cleaned[node] {
				t.Fatalf("failure %d: %s missing evidence/collection/cleanup: %s", failAt, node, result.Error)
			}
		}
		if !failed {
			if result.Status != tr.StatusPass {
				t.Fatal(result.Error)
			}
			break
		}
		if result.Status != tr.StatusFail {
			t.Fatalf("injected failure %d passed", failAt)
		}
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
			controller := s.Topology.Nodes["controller"]
			if !controller.IsNative || controller.IsLocal {
				t.Fatal("Linux cloud controller must execute natively, not via wsl")
			}
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
			masters := 0
			connects := 0
			writer := false
			writerStopped := false
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
				if strings.Contains(cmd, "/bin/blockmaster ") {
					masters++
					// Documented replicated standalone command, product PR #644:
					// a8f58eb4a6e4cf64e8206e93be85bde1c5607997,
					// docs/standalone-deployment.md step 2. Run-local paths and
					// listen address substitute for the example's paths/address.
					for _, arg := range []string{
						"--authority-store $W/master/authority",
						"--lifecycle-store $W/master/lifecycle",
						"--cluster-spec $W/cluster-spec.yaml",
						"--expected-slots-per-volume " + rf,
						"--lifecycle-product-loop-interval 100ms",
						"--failback-runtime-rpc",
						"--tls-cert $W/tls/master.crt",
						"--tls-key $W/tls/master.key",
						"--tls-client-ca $W/tls/ca.crt",
						"--role-file $W/tls/roles",
						"--audit-log $W/master/audit.jsonl",
						"--listen 0.0.0.0:19600",
					} {
						if !strings.Contains(cmd, arg) {
							return nil, fmt.Errorf("documented master command missing %s", arg)
						}
					}
				}
				if strings.Contains(cmd, "r4.sh unit $T writer") {
					writer = true
				}
				if a.Action == "cloud_vm_power" && !writer {
					return nil, fmt.Errorf("power-off precedes continuous writer")
				}
				if strings.Contains(cmd, "r4.sh writer-stop $T") {
					writerStopped = true
				}
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
					if connects > 1 && !writerStopped {
						return nil, fmt.Errorf("reconnect before stopping the fault-boundary writer")
					}
				}
				if strings.Contains(cmd, "cleanup $T") {
					counts[a.Node]++
				}
				if strings.Contains(cmd, "ops failover ") {
					failover = strings.Contains(cmd, "--expect-current r1@1 --confirm-old-node-off v1")
					if !strings.Contains(cmd, "[ 'automatic' = operator ] || exit 0") {
						return nil, fmt.Errorf("automatic mode must skip the operator command")
					}
				}
				if strings.Contains(cmd, "r4.sh start test-run-rf"+rf+" volume") && a.Node == "node-2" {
					rejoin = true
				}
				if strings.Contains(cmd, "verify \"$DEV\" $W/ledger before during-loss after-rejoin") {
					post = true
				}
				if strings.Contains(cmd, "auto-p2.py wait ") {
					return map[string]string{"value": "test-promotion-token"}, nil
				}
				return nil, nil
			}
			reg := tr.NewRegistry()
			for _, n := range []string{"exec", "upload_path", "collect_path", "cloud_vm_power", "cloud_vm_power_restore"} {
				reg.RegisterFunc(n, tr.TierCore, handler)
			}
			result := tr.NewEngine(reg, nil).Run(context.Background(), s, &tr.ActionContext{Scenario: s, Vars: map[string]string{}, Log: func(string, ...interface{}) {}})
			if result.Status != tr.StatusPass || masters != 1 || starts != 3 || connects != 2 || !failover || !rejoin || !post || !writer || !writerStopped {
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
