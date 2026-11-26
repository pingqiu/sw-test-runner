package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	tr "github.com/pingqiu/sw-test-runner"
)

// slashRun writes a pack context whose path is slash-rooted (the controller
// is Linux; on Windows the test runs from the volume root).
func slashRun(t *testing.T, invMut, ctxMut func(string) string) string {
	t.Helper()
	dir := t.TempDir()
	work := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	hard := time.Now().Add(time.Hour + 20*time.Minute).UTC().Format(time.RFC3339)
	inv := `{"schema":"seaweed.inventory/v1","target":"azure","run_id":"r4-1","deadlines":{"work":"` + work + `","hard":"` + hard + `"},
	 "managed_resource_ids":["` + goodVM + `"],
	 "hosts":[{"id":"node-1","resource_id":"` + goodVM + `","management":{"address":"20.1.2.3","port":22,"user":"seaweed"}}]}`
	if invMut != nil {
		inv = invMut(inv)
	}
	invPath := filepath.Join(dir, "inventory.json")
	if err := os.WriteFile(invPath, []byte(inv), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := `{"schema":"seaweed.cloud-pack-context/v1","run_id":"r4-1","inventory":"` + filepath.ToSlash(invPath) + `","ssh_key":"/k/id","host_keys":{"node-1":"ssh-ed25519 AAAAC3Nz"}}`
	if ctxMut != nil {
		ctx = ctxMut(ctx)
	}
	p := filepath.Join(dir, "context.json")
	if err := os.WriteFile(p, []byte(ctx), 0o600); err != nil {
		t.Fatal(err)
	}
	p = filepath.ToSlash(p)
	if v := filepath.VolumeName(dir); v != "" {
		t.Chdir(v + `\`)
		p = p[len(v):]
	}
	return p
}

func slashDir(t *testing.T) string {
	d := filepath.ToSlash(t.TempDir())
	if v := filepath.VolumeName(d); v != "" {
		d = d[len(v):]
	}
	return d
}

func TestOpenScopeRefusesAMismatchedContext(t *testing.T) {
	if s, err := OpenScope(t.Context(), slashRun(t, nil, nil), slashDir(t)); err != nil {
		t.Fatalf("a matching context: %v", err)
	} else {
		_ = s.Close()
	}
	for name, c := range map[string]struct{ inv, ctx func(string) string }{
		"another run id in the context": {nil, func(s string) string { return strings.Replace(s, `"run_id":"r4-1"`, `"run_id":"r4-2"`, 1) }},
		"a host key for a foreign host": {nil, func(s string) string { return strings.Replace(s, `"node-1":`, `"node-9":"ssh-ed25519 AAAAC3Nz","node-1":`, 1) }},
		"no time to restore before the hard deadline": {func(s string) string {
			return regexp.MustCompile(`"hard":"[^"]+"`).ReplaceAllString(s, `"hard":"`+time.Now().Add(time.Hour+2*time.Minute).UTC().Format(time.RFC3339)+`"`)
		}, nil},
	} {
		if s, err := OpenScope(t.Context(), slashRun(t, c.inv, c.ctx), slashDir(t)); err == nil {
			_ = s.Close()
			t.Errorf("%s: opened", name)
		}
	}
}

func TestPowerActionRefusesWithoutCustody(t *testing.T) {
	if _, err := vmPowerOff(t.Context(), &tr.ActionContext{}, tr.Action{Params: map[string]string{"node": "node-1"}}); err == nil || !strings.Contains(err.Error(), "no fault custody") {
		t.Fatalf("no scope: %v", err)
	}
}

func TestUsesFaults(t *testing.T) {
	s := &tr.Scenario{Phases: []tr.Phase{{Actions: []tr.Action{{Action: "exec"}}}}}
	if UsesFaults(s) {
		t.Fatal("exec only")
	}
	s.Phases[0].Actions = append(s.Phases[0].Actions, tr.Action{Action: "cloud_vm_power"})
	if !UsesFaults(s) || UsesFaults(nil) {
		t.Fatal("cloud_vm_power")
	}
}

// fakeCloud is one VM behind the az and ssh seams.
type fakeCloud struct {
	mu    sync.Mutex
	power string
	boot  int
	argv  [][]string
}

func (c *fakeCloud) install(t *testing.T) {
	oldAz, oldSSH := Az, SSH
	t.Cleanup(func() { Az, SSH = oldAz, oldSSH })
	Az = func(_ context.Context, argv []string) (string, string, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.argv = append(c.argv, argv)
		switch argv[1] {
		case "stop":
			c.power = "stopped"
		case "start":
			c.power, c.boot = "running", c.boot+1
		case "get-instance-view":
			return `{"vmId":"vmid-1","instanceView":{"statuses":[{"code":"PowerState/` + c.power + `"}]}}`, "", nil
		}
		return "", "", nil
	}
	SSH = func(context.Context, Host, string, string, string) (string, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.power != "running" {
			return "", errors.New("connect timed out")
		}
		return []string{"1f0e2d3c-4b5a-6978-8f9e-0d1c2b3a4f50", "1f0e2d3c-4b5a-6978-8f9e-0d1c2b3a4f51"}[c.boot], nil
	}
}

func TestPowerActionAppliesRestoresAndLeavesTheReceiptInTheBundle(t *testing.T) {
	ctxPath := slashRun(t, nil, nil)
	t.Setenv(ContextEnv, ctxPath)
	bundleDir := t.TempDir()
	s, err := OpenScope(t.Context(), ctxPath, slashDir(t))
	if err != nil {
		t.Fatal(err)
	}
	h, err := tr.OpenHistory(filepath.Join(bundleDir, "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	c := &fakeCloud{power: "running"}
	c.install(t)
	actx := &tr.ActionContext{Safety: s, History: h, Bundle: &tr.RunBundle{Dir: bundleDir}}
	act := tr.Action{Action: "cloud_vm_power", Params: map[string]string{"node": "node-1"}}
	out, err := vmPowerOff(t.Context(), actx, act)
	if err != nil || out["fault_key"] != "node-1/"+goodVM {
		t.Fatalf("apply: %v %v", out, err)
	}
	if c.power != "stopped" {
		t.Fatalf("not powered off: %s", c.power)
	}
	if _, err := vmPowerRestore(t.Context(), actx, act); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if c.power != "running" || c.boot != 1 {
		t.Fatalf("not restored: %s boot %d", c.power, c.boot)
	}
	raw, err := os.ReadFile(filepath.Join(bundleDir, "faults", "receipts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var receipts map[string]tr.FaultReceipt
	if err := json.Unmarshal(raw, &receipts); err != nil {
		t.Fatal(err)
	}
	r := receipts["node-1/"+goodVM]
	if r.Kind != "vm_power" || r.Applied.State["node"] != "unreachable" || r.Restored.State["power"] != "running" || r.Restored.BootID == r.Before.BootID {
		t.Fatalf("receipt %+v", r)
	}
	for _, a := range c.argv {
		if err := checkArgv(goodVM, a); err != nil {
			t.Fatalf("an unchecked argv reached az: %v", a)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close after an explicit restore: %v", err)
	}
	_ = h.Close()
}
