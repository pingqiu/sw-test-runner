package seaweed

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pingqiu/sw-test-runner/inventory"
)

// fakeMountNode answers one client's steps. Every way a client can go wrong is one field.
type fakeMountNode struct {
	t        *testing.T
	root     string
	boot     string // the boot id the first step reads
	listBoot string // the boot id the listing reports (differs = the client rebooted)
	members  []member
	listing  func(string) string // rewrites the listing text
	stream   func(io.Writer) error
	worker   int // rc of the worker step
	mu       sync.Mutex
	cmds     []string
}

func (f *fakeMountNode) Run(ctx context.Context, cmd string) (string, string, int, error) {
	f.mu.Lock()
	f.cmds = append(f.cmds, cmd)
	f.mu.Unlock()
	switch {
	case strings.Contains(cmd, "boot_id") && !strings.Contains(cmd, "python3 -I -c"):
		return f.boot + "\n", "", 0, nil
	case strings.Contains(cmd, "mount_soak.py --config"):
		return "", "", f.worker, nil
	case strings.Contains(cmd, "python3 -I -c"):
		b := f.listBoot
		if b == "" {
			b = f.boot
		}
		l := listingFor(f.members, b, f.root)
		if f.listing != nil {
			l = f.listing(l)
		}
		return l, "", 0, nil
	}
	return "", "", 0, nil
}

func (f *fakeMountNode) Upload(string, string) error { return nil }

func (f *fakeMountNode) StreamRun(ctx context.Context, cmd string, w io.Writer) error {
	if f.stream != nil {
		return f.stream(w)
	}
	_, e := w.Write(archive(f.t, f.members, tar.FormatUSTAR))
	return e
}

func verdictJSON(status, cleanup string) []byte {
	return []byte(fmt.Sprintf(`{"schema":"seaweed-mount-verdict/1","status":%q,"cleanup":%q,"gate_reasons":[],"rows":{}}`, status, cleanup))
}

type mountFixture struct {
	d      inventory.Document
	c      CloudConfig
	groups map[string][]inventory.Host
	nodes  map[string]*fakeMountNode
	// verdicts by client id; the stand-in assessor answers from the admitted directory's client.
	verdicts map[string][]byte
}

func newMountFixture(t *testing.T) *mountFixture {
	t.Helper()
	f := &mountFixture{groups: map[string][]inventory.Host{}, nodes: map[string]*fakeMountNode{}, verdicts: map[string][]byte{}}
	f.d.RunID = "r1"
	weed := filepath.Join(t.TempDir(), "weed")
	os.WriteFile(weed, []byte("weed"), 0600)
	f.c = CloudConfig{Output: t.TempDir(), Files: map[string]CloudFile{"weed": {Path: weed, SHA256: hash([]byte("weed"))}}}
	mk := func(id, role, addr string, svc ...inventory.Service) inventory.Host {
		h := inventory.Host{ID: id, Roles: []string{role}, RunRoot: "/data/seaweed-runs/r1", Services: svc}
		h.Data.Address = addr
		return h
	}
	f.groups["master"] = []inventory.Host{mk("meta", "master", "10.84.1.4", inventory.Service{Name: "filer_http", Port: 8888})}
	for i := 1; i <= 6; i++ {
		f.groups["volume"] = append(f.groups["volume"], mk(fmt.Sprintf("vol%d", i), "volume", fmt.Sprintf("10.84.1.%d", 10+i), inventory.Service{Name: "volume_http", Port: 8080}))
	}
	for i, id := range mountClients {
		f.groups["client"] = append(f.groups["client"], mk(id, "client", fmt.Sprintf("10.84.1.%d", 20+i)))
		f.nodes[id] = &fakeMountNode{t: t, root: "/data/seaweed-runs/r1/mountsoak", boot: fmt.Sprintf("boot-%s", id), members: records()}
		f.verdicts[id] = verdictJSON("PASS", "PROVEN")
	}
	orig := mountAssess
	t.Cleanup(func() { mountAssess = orig })
	mountAssess = func(ctx context.Context, script, dir string) ([]byte, error) {
		return f.verdicts[filepath.Base(filepath.Dir(dir))], nil // dir is <out>/mount/<client>/run-root
	}
	return f
}

func (f *mountFixture) run() (clusterMountResult, error) {
	_, err := runClusterMount(context.Background(), f.d, f.c, f.groups, func(h inventory.Host) (mountNode, func(), error) {
		return f.nodes[h.ID], func() {}, nil
	})
	var r clusterMountResult
	raw, e := os.ReadFile(filepath.Join(f.c.Output, "mount-verdicts.json"))
	if e == nil {
		json.Unmarshal(raw, &r)
	}
	return r, err
}

func client(r clusterMountResult, id string) clientMount {
	for _, c := range r.Clients {
		if c.Client == id {
			return c
		}
	}
	return clientMount{}
}

func TestClusterMountAllThreePassIsPass(t *testing.T) {
	f := newMountFixture(t)
	r, err := f.run()
	if err != nil || r.Status != "PASS" || r.Cleanup != "PROVEN" || len(r.Clients) != 3 || r.Scope != mountClusterScope || r.Schema != "seaweed-mount-cluster/1" {
		t.Fatalf("%+v %v", r, err)
	}
	for _, id := range mountClients {
		if c := client(r, id); c.Missing != "" || c.Boot != "boot-"+id || len(c.ListingSHA256) != 64 || c.Worker != "ok" {
			t.Fatalf("%s: %+v", id, c)
		}
	}
}

func TestClusterMountAValidFailIsNeverHidden(t *testing.T) {
	f := newMountFixture(t)
	f.verdicts["client1"] = verdictJSON("FAIL", "PROVEN")
	f.verdicts["client2"] = verdictJSON("INVALID", "PROVEN")
	f.nodes["client3"].stream = func(io.Writer) error { return errors.New("connection reset") }
	r, err := f.run()
	if r.Status != "FAIL" || err == nil || !strings.Contains(err.Error(), "verdict FAIL") {
		t.Fatalf("a valid FAIL was hidden: %+v %v", r, err)
	}
	if c := client(r, "client1"); !strings.Contains(string(c.Verdict), `"FAIL"`) {
		t.Fatalf("client1's FAIL verdict was not kept whole: %s", c.Verdict)
	}
	if c := client(r, "client3"); !strings.Contains(c.Missing, "connection reset") || c.Verdict != nil {
		t.Fatalf("client3 should be missing with the reason: %+v", c)
	}
	if r.Cleanup != "UNKNOWN" {
		t.Fatalf("cleanup with a missing client must not be PROVEN: %s", r.Cleanup)
	}
}

func TestClusterMountFailWithoutProvenCleanupIsNotAValidFail(t *testing.T) {
	f := newMountFixture(t)
	f.verdicts["client2"] = verdictJSON("FAIL", "UNKNOWN")
	r, _ := f.run()
	if r.Status != "INVALID" || r.Cleanup != "UNKNOWN" || !strings.Contains(string(client(r, "client2").Verdict), `"FAIL"`) {
		t.Fatalf("%+v", r)
	}
}

func TestClusterMountEachBadClientIsMissingAndTheOthersStand(t *testing.T) {
	bomb := append(records(), member{name: "./z", body: make([]byte, 600<<20)}) // over the per-member cap
	cases := map[string]func(*fakeMountNode){
		"listing malformed": func(n *fakeMountNode) { n.listing = func(string) string { return "nonsense\n" } },
		"listing names a link": func(n *fakeMountNode) {
			n.listing = func(s string) string { return strings.Replace(s, "end\t", "other\tlink\nend\t", 1) }
		},
		"rebooted":        func(n *fakeMountNode) { n.listBoot = "another-boot" },
		"wrong root":      func(n *fakeMountNode) { n.root = "/elsewhere" },
		"stream error":    func(n *fakeMountNode) { n.stream = func(io.Writer) error { return errors.New("broken pipe") } },
		"stream not gzip": func(n *fakeMountNode) { n.stream = func(w io.Writer) error { w.Write([]byte("hello")); return nil } },
		"unlisted member": func(n *fakeMountNode) {
			ms := append(records(), member{name: "./extra", body: []byte("x")})
			n.stream = func(w io.Writer) error { _, e := w.Write(archive(n.t, ms, tar.FormatUSTAR)); return e }
		},
		"trailing data": func(n *fakeMountNode) {
			n.stream = func(w io.Writer) error {
				w.Write(archive(n.t, n.members, tar.FormatUSTAR))
				_, e := w.Write(make([]byte, 1<<20+10))
				return e
			}
		},
		"member over its cap": func(n *fakeMountNode) {
			n.members = bomb
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			f := newMountFixture(t)
			breakIt(f.nodes["client2"])
			r, err := f.run()
			c2 := client(r, "client2")
			if c2.Missing == "" || c2.Verdict != nil {
				t.Fatalf("client2 should be missing: %+v", c2)
			}
			for _, id := range []string{"client1", "client3"} {
				if c := client(r, id); c.Missing != "" || c.status != "" && c.status != "PASS" {
					t.Fatalf("%s was affected: %+v", id, c)
				}
			}
			if r.Status != "INVALID" || err == nil {
				t.Fatalf("a missing client with two passes must be INVALID: %+v %v", r.Status, err)
			}
		})
	}
}

func TestClusterMountAHangingStreamIsCutAtItsBound(t *testing.T) {
	orig := mountFetchBound
	origGrace := mountStreamGrace
	mountFetchBound, mountStreamGrace = 300*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { mountFetchBound, mountStreamGrace = orig, origGrace })
	f := newMountFixture(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f.nodes["client1"].stream = func(w io.Writer) error { <-release; return nil }
	f.nodes["client1"].stream = func(w io.Writer) error {
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		return context.DeadlineExceeded
	}
	start := time.Now()
	r, _ := f.run()
	if c := client(r, "client1"); c.Missing == "" {
		t.Fatalf("a hanging stream was not cut: %+v", c)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatalf("the hang held the phase for %s", time.Since(start))
	}
}

func TestClusterMountForeignAssessorOutputIsMissing(t *testing.T) {
	f := newMountFixture(t)
	f.verdicts["client3"] = []byte(`{"schema":"something-else","status":"PASS"}`)
	r, _ := f.run()
	if c := client(r, "client3"); !strings.Contains(c.Missing, "assessor") {
		t.Fatalf("%+v", c)
	}
}

func TestClusterMountWorkerConfigCutsEveryVolumeAndOwnsItsPath(t *testing.T) {
	f := newMountFixture(t)
	vols, err := mountVolumes(f.groups["volume"])
	if err != nil {
		t.Fatal(err)
	}
	raw, err := clientMountConfig(f.d, f.groups["client"][1], f.groups["master"][0], vols)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	json.Unmarshal(raw, &cfg)
	if v := cfg["volumes"].([]any); len(v) != 6 || fmt.Sprint(v[0]) != "[10.84.1.11 8080]" {
		t.Fatalf("volumes %v", cfg["volumes"])
	}
	if cfg["filer_path"] != "/testops/r1/mountsoak/client2" || cfg["root"] != "/data/seaweed-runs/r1/mountsoak" || cfg["filer"] != "10.84.1.4:8888" {
		t.Fatalf("config %s", raw)
	}
	if cfg["phases_s"].([]any)[0].(float64) != float64(scheduleFor(MountSoakCase).PhasesS[0]) {
		t.Fatalf("not the full schedule: %v", cfg["phases_s"])
	}
}

func TestClusterMountRosterIsFrozen(t *testing.T) {
	f := newMountFixture(t)
	f.groups["client"] = f.groups["client"][:2]
	if _, err := f.run(); err == nil || !strings.Contains(err.Error(), "client1-3") {
		t.Fatalf("a two-client roster ran: %v", err)
	}
	f = newMountFixture(t)
	f.groups["client"][2].ID = "client9"
	if _, err := f.run(); err == nil {
		t.Fatal("a foreign client ran")
	}
}

func TestMountSoakRefusesEveryOtherPhaseAndTarget(t *testing.T) {
	ten := func(target string) inventory.Document {
		d := inventory.Document{Target: target}
		for _, id := range []string{"meta", "vol1", "vol2", "vol3", "vol4", "vol5", "vol6", "client1", "client2", "client3"} {
			d.Hosts = append(d.Hosts, inventory.Host{ID: id})
		}
		return d
	}
	for name, c := range map[string]CloudConfig{
		"with chaos": {MountSoak: true, Chaos: true}, "with replication": {MountSoak: true, Replication: "001"},
		"with chaos_set": {MountSoak: true, ChaosSet: "A"}, "with read_case": {MountSoak: true, ReadCase: MountSoakCase},
		"with kv_soak": {MountSoak: true, KVSoak: true}, "with kv_load": {MountSoak: true, KVLoad: true},
		"with ec": {MountSoak: true, ECStage1: true}, "with master writes": {MountSoak: true, MasterWrites: true},
		"with cluster chaos": {MountSoak: true, ClusterChaosCases: []string{"x"}}, "with swctl": {MountSoak: true, SWCtl: &SWCtlConfig{}},
	} {
		if err := mountSoakRefusal(ten("azure"), c); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	for _, target := range []string{"onprem", "aws", "gcp"} {
		if err := mountSoakRefusal(ten(target), CloudConfig{MountSoak: true}); err == nil {
			t.Fatalf("target %s was accepted", target)
		}
	}
	short := ten("azure")
	short.Hosts = short.Hosts[:9]
	if err := mountSoakRefusal(short, CloudConfig{MountSoak: true}); err == nil {
		t.Fatal("nine hosts were accepted")
	}
	for _, target := range []string{"azure", "docker"} {
		if err := mountSoakRefusal(ten(target), CloudConfig{MountSoak: true}); err != nil {
			t.Fatalf("%s: %v", target, err)
		}
	}
	if err := mountSoakRefusal(ten("onprem"), CloudConfig{Chaos: true}); err != nil {
		t.Fatalf("a run without mount_soak was refused: %v", err)
	}
}
