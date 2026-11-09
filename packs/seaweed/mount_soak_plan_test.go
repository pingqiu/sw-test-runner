package seaweed

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pingqiu/sw-test-runner/inventory"
)

func mountSoakInventory() *inventory.Document {
	d := &inventory.Document{RunID: "run1"}
	d.Hosts = []inventory.Host{
		{ID: "m01", Roles: []string{"client"}, RunRoot: "/opt/rdma-lab-ci/work/run1"},
		{ID: "m02", Roles: []string{"master", "filer", "volume"}, Services: []inventory.Service{{Name: "filer_http", Port: 19105}, {Name: "volume_http", Port: 19601}}},
	}
	d.Hosts[1].Data.Address = "10.0.2.3"
	return d
}

func TestMountSoakArgsComeOnlyFromTheInventory(t *testing.T) {
	args, err := MountSoakArgs(mountSoakInventory(), SmokeSettings{HelperDir: "/rel/worker"})
	if err != nil {
		t.Fatal(err)
	}
	if args[1] != "/rel/worker/mount_soak.py" || args[2] != "--config" {
		t.Fatalf("argv %v", args)
	}
	var c mountSoakConfig
	if err = json.Unmarshal([]byte(args[3]), &c); err != nil {
		t.Fatal(err)
	}
	if c.VolumeHost != "10.0.2.3" || c.VolumePort != 19601 || c.Filer != "10.0.2.3:19105" || c.Weed != "/rel/bin/weed" || c.Root != "/opt/rdma-lab-ci/work/run1" {
		t.Fatalf("config %+v", c)
	}
	if len(c.PhasesS) != 3 || c.PhasesS[0] != 900 || c.EpisodesS[0] != 60 || c.EpisodesS[1] != 180 || c.Mounts != 4 || c.DualS != 300 {
		t.Fatalf("not the r4 schedule: %+v", c)
	}
}

func TestMountSoakRefusesAnInventoryOutsideTheFrozenPath(t *testing.T) {
	cases := map[string]func(*inventory.Document){
		"two volume servers": func(d *inventory.Document) { d.Hosts[0].Roles = append(d.Hosts[0].Roles, "volume") },
		"filer on the client": func(d *inventory.Document) {
			d.Hosts[0].Roles, d.Hosts[1].Roles = []string{"client", "filer"}, []string{"volume"}
		},
		"no volume_http": func(d *inventory.Document) { d.Hosts[1].Services = d.Hosts[1].Services[:1] },
	}
	for name, mutate := range cases {
		d := mountSoakInventory()
		mutate(d)
		if _, err := MountSoakArgs(d, SmokeSettings{HelperDir: "/rel/worker"}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestMountVerdictDecodeRefusesForeignOutput(t *testing.T) {
	if _, err := decodeMountVerdict([]byte(`{"schema":"seaweed-mount-verdict/1","status":"PASS","cleanup":"PROVEN"}`)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"schema":"other","status":"PASS"}`, `{"schema":"seaweed-mount-verdict/1","status":"MAYBE"}`, `not json`} {
		if _, err := decodeMountVerdict([]byte(raw)); err == nil || strings.Contains(raw, "\"status\":\"PASS\",\"cleanup") {
			t.Errorf("%s accepted", raw)
		}
	}
}
