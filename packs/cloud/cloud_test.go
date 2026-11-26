package cloud

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodVM = "/subscriptions/0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0/resourceGroups/rg-seaweed_1/providers/Microsoft.Compute/virtualMachines/node-1"

func TestArgvCarriesOnlyAVMIDOfTheRightShape(t *testing.T) {
	for _, argv := range [][]string{powerOffArgv(goodVM), powerOnArgv(goodVM), viewArgv(goodVM)} {
		if err := checkArgv(goodVM, argv); err != nil {
			t.Errorf("%v: %v", argv, err)
		}
	}
	if a := powerOffArgv(goodVM); !contains(a, "--skip-shutdown") {
		t.Errorf("power off is not a power cut: %v", a)
	}
	for name, id := range map[string]string{
		"a disk":                strings.Replace(goodVM, "virtualMachines/node-1", "disks/node-1-os", 1),
		"another provider":      strings.Replace(goodVM, "Microsoft.Compute", "Microsoft.Network", 1),
		"no subscription uuid":  strings.Replace(goodVM, "0b1c2d3e-4f50-6172-8394-a5b6c7d8e9f0", "prod", 1),
		"a shell character":     goodVM + "&calc",
		"a quote":               goodVM + "\"",
		"a trailing child path": goodVM + "/extensions/x",
	} {
		if err := checkArgv(id, powerOffArgv(id)); err == nil {
			t.Errorf("%s accepted: %s", name, id)
		}
	}
}

func TestAzRunsOnlyCheckedArgv(t *testing.T) {
	var called [][]string
	old := Az
	defer func() { Az = old }()
	Az = func(_ context.Context, argv []string) (string, string, error) {
		called = append(called, argv)
		return "", "", nil
	}
	if err := PowerOff(context.Background(), goodVM+"|x"); err == nil || len(called) != 0 {
		t.Fatalf("an unchecked id reached az: %v %v", err, called)
	}
	if err := PowerOff(context.Background(), goodVM); err != nil || len(called) != 1 || called[0][1] != "stop" {
		t.Fatalf("power off: %v %v", err, called)
	}
	Az = func(context.Context, []string) (string, string, error) {
		return "", "ERROR: (AuthorizationFailed) no", errors.New("exit 1")
	}
	if err := PowerOn(context.Background(), goodVM); err == nil || !strings.Contains(err.Error(), "AuthorizationFailed") {
		t.Fatalf("a failed az call: %v", err)
	}
}

func TestParseViewReadsIdentityAndOnePowerState(t *testing.T) {
	ok := `{"vmId":"6b2a-1","instanceView":{"statuses":[{"code":"ProvisioningState/succeeded"},{"code":"PowerState/deallocated"}]}}`
	v, err := parseView([]byte(ok))
	if err != nil || v.VMID != "6b2a-1" || v.PowerState != "deallocated" {
		t.Fatalf("%+v %v", v, err)
	}
	// get-instance-view may also answer with the statuses at the top level.
	if v, err := parseView([]byte(`{"vmId":"x","statuses":[{"code":"PowerState/running"}]}`)); err != nil || v.PowerState != "running" {
		t.Fatalf("top-level statuses: %+v %v", v, err)
	}
	for name, raw := range map[string]string{
		"no vmId":             `{"instanceView":{"statuses":[{"code":"PowerState/running"}]}}`,
		"no power state":      `{"vmId":"x","instanceView":{"statuses":[{"code":"ProvisioningState/succeeded"}]}}`,
		"two power states":    `{"vmId":"x","instanceView":{"statuses":[{"code":"PowerState/running"},{"code":"PowerState/stopped"}]}}`,
		"an empty power code": `{"vmId":"x","instanceView":{"statuses":[{"code":"PowerState/"}]}}`,
		"not json":            `ERROR: x`,
	} {
		if _, err := parseView([]byte(raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func writeRun(t *testing.T, invMut func(string) string) string {
	t.Helper()
	dir := t.TempDir()
	inv := `{"schema":"seaweed.inventory/v1","target":"azure","run_id":"r4-1","deadlines":{"work":"2099-01-01T01:00:00Z","hard":"2099-01-01T01:20:00Z","cleanup_reserve_ms":15000},"managed_resource_ids":["` + goodVM + `"],
	 "hosts":[{"id":"node-1","resource_id":"` + goodVM + `","management":{"address":"20.1.2.3","port":22,"user":"seaweed","host_key":""}}]}`
	if invMut != nil {
		inv = invMut(inv)
	}
	invPath := filepath.Join(dir, "inventory.json")
	_ = os.WriteFile(invPath, []byte(inv), 0o600)
	ctx := `{"schema":"seaweed.cloud-pack-context/v1","inventory":"` + filepath.ToSlash(invPath) + `","ssh_key":"/k/id","host_keys":{"node-1":"ssh-ed25519 AAAAC3Nz"}}`
	p := filepath.Join(dir, "context.json")
	_ = os.WriteFile(p, []byte(ctx), 0o600)
	return p
}

func TestLoadRunBindsHostsToTheirVMAndKey(t *testing.T) {
	r, err := LoadRun(writeRun(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	h := r.Hosts["node-1"]
	if r.RunID != "r4-1" || h.ResourceID != goodVM || h.HostKey != "ssh-ed25519 AAAAC3Nz" || h.Port != 22 || len(r.Owned) != 1 {
		t.Fatalf("%+v", r)
	}
	for name, mut := range map[string]func(string) string{
		"no managed resources": func(s string) string { return strings.Replace(s, `["`+goodVM+`"]`, `[]`, 1) },
		"not azure":            func(s string) string { return strings.Replace(s, `"azure"`, `"onprem"`, 1) },
		"no resource id":       func(s string) string { return strings.Replace(s, `"resource_id":"`+goodVM+`"`, `"resource_id":""`, 1) },
		"no deadlines":         func(s string) string { return strings.Replace(s, `"work":"2099-01-01T01:00:00Z",`, ``, 1) },
		"a host twice": func(s string) string {
			return strings.Replace(s, `}]}`, `},{"id":"node-1","resource_id":"x","management":{"address":"a","user":"u"}}]}`, 1)
		},
	} {
		if _, err := LoadRun(writeRun(t, mut)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestBootIDPinsTheHostKeyAndReadsOnlyABootID(t *testing.T) {
	old := SSH
	defer func() { SSH = old }()
	h := Host{ID: "node-1", Address: "20.1.2.3", Port: 22, User: "seaweed", HostKey: "ssh-ed25519 AAAAC3Nz"}
	var known string
	SSH = func(_ context.Context, _ Host, _, knownHosts, cmd string) (string, error) {
		b, _ := os.ReadFile(knownHosts)
		known = string(b)
		return "1f0e2d3c-4b5a-6978-8f9e-0d1c2b3a4f5e\n", nil
	}
	id, err := BootID(context.Background(), h, "/k/id", t.TempDir())
	if err != nil || id != "1f0e2d3c-4b5a-6978-8f9e-0d1c2b3a4f5e" || known != "20.1.2.3 ssh-ed25519 AAAAC3Nz\n" {
		t.Fatalf("%q %v known=%q", id, err, known)
	}
	for name, answer := range map[string]struct {
		out string
		err error
	}{
		"unreachable":       {"", errors.New("ssh: connect timed out")},
		"not a boot id":     {"Permission denied\n", nil},
		"an empty answer":   {"", nil},
		"two lines of junk": {"1f0e2d3c-4b5a-6978-8f9e-0d1c2b3a4f5e\nmore\n", nil},
	} {
		SSH = func(context.Context, Host, string, string, string) (string, error) { return answer.out, answer.err }
		if id, err := BootID(context.Background(), h, "/k/id", t.TempDir()); err == nil {
			t.Errorf("%s: read %q", name, id)
		}
	}
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
