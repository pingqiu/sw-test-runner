package smoke

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pingqiu/sw-test-runner/packs/seaweed"
	"golang.org/x/crypto/ssh"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	source, e := filepath.Abs("../..")
	if e != nil {
		t.Fatal(e)
	}
	_, pin, e := Sources(source)
	if e != nil {
		t.Fatal(e)
	}
	hard := time.Now().UTC().Add(50 * time.Minute).Truncate(time.Minute)
	vars, _ := json.Marshal(map[string]any{"subscription_id": "11111111-1111-1111-1111-111111111111", "resource_group_name": "dedicated-test", "location": "westus2", "vm_size": "Standard_D2s_v5", "run_declaration": map[string]any{"work_deadline": hard.Add(-20 * time.Minute), "hard_deadline": hard, "cloud_cleanup_reserve_seconds": 1200, "budget_usd": 3, "abort_estimate_usd": 2.5}})
	p := filepath.Join(root, "vars.json")
	if e = os.WriteFile(p, vars, 0600); e != nil {
		t.Fatal(e)
	}
	cloud := seaweed.CloudConfig{SSHKey: filepath.Join(root, "key"), Files: map[string]seaweed.CloudFile{}}
	for _, name := range []string{"weed", "weed-volume", "native.tar"} {
		p := filepath.Join(root, name)
		os.WriteFile(p, []byte(name), 0600)
		cloud.Files[name] = seaweed.CloudFile{Path: p, SHA256: digest([]byte(name))}
	}
	return Config{Source: source, ModuleSHA256: pin, Variables: p, VariablesSHA256: digest(vars), Output: filepath.Join(root, "output"), Cloud: cloud, EstimatedUSD: .12}
}

type fake struct {
	events       []string
	fail         string
	lists        int
	live         bool
	cancel       context.CancelFunc
	cleanContext bool
}

func (f *fake) command(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
	label := tool + " " + args[0]
	f.events = append(f.events, label)
	if tool == "az" {
		if args[0] == "tag" {
			return []byte(`{}`), nil
		}
		if !strings.Contains(strings.Join(args, " "), "--resource-group") {
			return []byte(`[]`), nil
		}
		f.lists++
		if f.fail == "list" && f.lists > 1 {
			return nil, errors.New("list unknown")
		}
		if f.live && f.lists > 1 {
			return []byte(`[{"id":"/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/dedicated-test/providers/Microsoft.Compute/virtualMachines/vm","type":"Microsoft.Compute/virtualMachines"}]`), nil
		}
		return []byte(`[]`), nil
	}
	if args[0] == "destroy" {
		f.cleanContext = ctx.Err() == nil
	}
	if args[0] == f.fail {
		return nil, errors.New("failed/unknown " + f.fail)
	}
	if args[0] == "output" {
		if args[len(args)-1] == "os_disk_ids" {
			ids := []string{}
			for i := 0; i < 10; i++ {
				ids = append(ids, "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/dedicated-test/providers/Microsoft.Compute/disks/os"+fmt.Sprint(i))
			}
			return json.Marshal(ids)
		}
		return []byte(`{"exact":"module export"}`), nil
	}
	return []byte("ok"), nil
}
func (f *fake) pack(ctx context.Context, b []byte, cloud seaweed.CloudConfig) (string, error) {
	f.events = append(f.events, "pack")
	if string(b) != `{"exact":"module export"}` {
		return "", errors.New("export drift")
	}
	if f.cancel != nil {
		f.cancel()
	}
	if f.fail == "pack" {
		return "bundle", errors.New("workload FAIL")
	}
	return "bundle", nil
}
func controller(f *fake) Controller { return Controller{Command: f.command, Pack: f.pack} }

func TestPlanHasNoCommandsOrOutput(t *testing.T) {
	c := fixture(t)
	f := &fake{}
	r := controller(f).Execute(context.Background(), "plan", c)
	if r.Code != "PLAN_ONLY" || len(f.events) != 0 || len(r.Effects) != 0 || r.Plan["vm_count"] != 1 {
		t.Fatalf("%+v %+v", r, f)
	}
	if _, e := os.Stat(c.Output); !os.IsNotExist(e) {
		t.Fatal("plan wrote output")
	}
}
func TestCompleteLifecycle(t *testing.T) {
	c := fixture(t)
	f := &fake{}
	r := controller(f).Execute(context.Background(), "run", c)
	want := "az resource,terraform init,terraform plan,terraform apply,terraform output,pack,terraform destroy,az resource"
	if r.Code != "COMPLETE" || !r.RemainingObserved || strings.Join(f.events, ",") != want {
		t.Fatalf("%+v %+v", r, f.events)
	}
}

func TestGeneratedServerKeyIsInPrivateCloudInitBeforeApply(t *testing.T) {
	c := fixture(t)
	f := &fake{}
	checked := false
	ctl := controller(f)
	ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
		if tool == "terraform" && args[0] == "plan" {
			b, e := os.ReadFile(filepath.Join(dir, "host.auto.tfvars.json"))
			if e != nil {
				t.Fatal(e)
			}
			var v struct {
				HostKeys map[string]string `json:"host_keys"`
			}
			if json.Unmarshal(b, &v) != nil {
				t.Fatal("bad key JSON")
			}
			signer, e := ssh.ParsePrivateKey([]byte(v.HostKeys["ed25519_private"]))
			if e != nil {
				t.Fatal(e)
			}
			if strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) != v.HostKeys["ed25519_public"] {
				t.Fatal("server key mismatch")
			}
			checked = true
		}
		return f.command(ctx, dir, tool, args...)
	}
	ctl.Pack = func(ctx context.Context, b []byte, cloud seaweed.CloudConfig) (string, error) {
		if !checked || cloud.HostKey == "" || cloud.Output != c.Output {
			t.Fatal("key or output missing before pack")
		}
		return f.pack(ctx, b, cloud)
	}
	r := ctl.Execute(context.Background(), "run", c)
	if r.Code != "COMPLETE" || !checked {
		t.Fatal(r)
	}
}

func TestMissingArtifactsRefuseBeforeCloudCalls(t *testing.T) {
	c := fixture(t)
	c.Cloud.Files = nil
	f := &fake{}
	r := (Controller{Command: f.command}).Execute(context.Background(), "run", c)
	if r.Code != "INPUT_REFUSED" || len(f.events) != 0 {
		t.Fatalf("%+v %v", r, f.events)
	}
}
func TestFailuresAlwaysCleanupAndStayFailed(t *testing.T) {
	for _, fail := range []string{"apply", "output", "pack"} {
		t.Run(fail, func(t *testing.T) {
			c := fixture(t)
			f := &fake{fail: fail}
			r := controller(f).Execute(context.Background(), "run", c)
			if r.Code == "COMPLETE" || !r.RemainingObserved || !f.cleanContext || f.events[len(f.events)-2] != "terraform destroy" || f.lists != 2 {
				t.Fatalf("%+v %+v", r, f)
			}
			count := 0
			for _, e := range f.events {
				if e == "terraform apply" {
					count++
				}
			}
			if count != 1 {
				t.Fatal("apply retried")
			}
		})
	}
}
func TestCleanupFailureAndResidualNeverComplete(t *testing.T) {
	for _, kind := range []string{"destroy", "list", "residual"} {
		t.Run(kind, func(t *testing.T) {
			c := fixture(t)
			f := &fake{fail: kind, live: kind == "residual"}
			r := controller(f).Execute(context.Background(), "run", c)
			if r.Code != "CLEANUP_INCOMPLETE" {
				t.Fatalf("%+v", r)
			}
			if kind == "residual" && len(r.Remaining) != 1 {
				t.Fatal("missing remaining ID")
			}
			if kind == "list" && r.RemainingObserved {
				t.Fatal("unknown list certified")
			}
		})
	}
}
func TestCancellationStillDestroys(t *testing.T) {
	c := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fake{cancel: cancel, fail: "pack"}
	r := controller(f).Execute(ctx, "run", c)
	if r.Code != "PACK_FAILED" || !f.cleanContext {
		t.Fatalf("%+v", r)
	}
}
func TestPinAndDeadlineRefuseBeforeContact(t *testing.T) {
	for _, kind := range []string{"module", "variables", "expired", "over60", "cost"} {
		t.Run(kind, func(t *testing.T) {
			c := fixture(t)
			switch kind {
			case "module":
				c.ModuleSHA256 = strings.Repeat("0", 64)
			case "variables":
				c.VariablesSHA256 = strings.Repeat("0", 64)
			case "cost":
				c.EstimatedUSD = 3.1
			default:
				b, _ := os.ReadFile(c.Variables)
				var v map[string]any
				_ = json.Unmarshal(b, &v)
				r := v["run_declaration"].(map[string]any)
				hard := time.Now().Add(-time.Hour).UTC().Truncate(time.Minute)
				if kind == "over60" {
					hard = time.Now().Add(2 * time.Hour).UTC().Truncate(time.Minute)
				}
				r["hard_deadline"] = hard
				r["work_deadline"] = hard.Add(-20 * time.Minute)
				b, _ = json.Marshal(v)
				_ = os.WriteFile(c.Variables, b, 0600)
				c.VariablesSHA256 = digest(b)
			}
			f := &fake{}
			r := controller(f).Execute(context.Background(), "run", c)
			if r.Code == "COMPLETE" || len(f.events) != 0 {
				t.Fatalf("%+v %+v", r, f)
			}
		})
	}
}

// Claude01's run 7 ruling: hard end may be up to 75 min away (60 work + 15
// cleanup reserve); the reserve is not cut. More than 75 refuses.
func TestHardDeadlineUpTo75Minutes(t *testing.T) {
	for _, tc := range []struct {
		away time.Duration
		ok   bool
	}{{74 * time.Minute, true}, {76 * time.Minute, false}} {
		c := fixture(t)
		b, _ := os.ReadFile(c.Variables)
		var v map[string]any
		_ = json.Unmarshal(b, &v)
		r := v["run_declaration"].(map[string]any)
		hard := time.Now().Add(tc.away).UTC().Truncate(time.Minute)
		r["hard_deadline"], r["work_deadline"] = hard, hard.Add(-20*time.Minute) // fixture reserve 1200 s
		b, _ = json.Marshal(v)
		_ = os.WriteFile(c.Variables, b, 0600)
		c.VariablesSHA256 = digest(b)
		out := controller(&fake{}).Execute(context.Background(), "run", c)
		if tc.ok == (out.Code == "DEADLINE_REFUSED" || out.Code == "INPUT_REFUSED") || !tc.ok && out.Code != "DEADLINE_REFUSED" {
			t.Fatalf("hard %v away: %+v", tc.away, out)
		}
	}
}

// The mount soak class has its own ceilings ($10 per run, $100 cumulative, 150 min hard); every other class keeps
// $3, $10 and 75 min, unchanged.
func TestMountSoakClassHasItsOwnCapsAndNoOtherClassDoes(t *testing.T) {
	set := func(c *Config, budget, abort float64, away time.Duration) {
		b, _ := os.ReadFile(c.Variables)
		var v map[string]any
		_ = json.Unmarshal(b, &v)
		if c.Cloud.MountSoak {
			v["fuse_clients"], v["cluster"] = true, true
		}
		r := v["run_declaration"].(map[string]any)
		hard := time.Now().Add(away).UTC().Truncate(time.Minute)
		r["hard_deadline"], r["work_deadline"], r["budget_usd"], r["abort_estimate_usd"] = hard, hard.Add(-20*time.Minute), budget, abort
		b, _ = json.Marshal(v)
		_ = os.WriteFile(c.Variables, b, 0600)
		c.VariablesSHA256 = digest(b)
	}
	refused := func(code string) bool { return code == "DEADLINE_REFUSED" || code == "INPUT_REFUSED" }
	for _, tc := range []struct {
		name               string
		mount              bool
		budget, abort, est float64
		prior              float64
		away               time.Duration
		ok                 bool
	}{
		{"mount soak at its ceilings", true, 10, 10, 9, 90, 149 * time.Minute, true},
		{"mount soak over per run", true, 10.5, 10, 9, 0, 120 * time.Minute, false},
		{"mount soak over cumulative", true, 10, 10, 9, 91.5, 120 * time.Minute, false},
		{"mount soak over the window", true, 10, 10, 9, 0, 151 * time.Minute, false},
		{"other class over $3", false, 5, 4, 3.5, 0, 50 * time.Minute, false},
		{"other class over $10 cumulative", false, 3, 2.5, 2, 8.5, 50 * time.Minute, false},
		{"other class over 75 min", false, 3, 2.5, 2, 0, 100 * time.Minute, false},
		{"other class inside its caps", false, 3, 2.5, 2, 7, 74 * time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fixture(t)
			c.Cloud.MountSoak, c.EstimatedUSD, c.PriorUSD = tc.mount, tc.est, tc.prior
			set(&c, tc.budget, tc.abort, tc.away)
			f := &fake{}
			out := controller(f).Execute(context.Background(), "run", c)
			if tc.ok == refused(out.Code) {
				t.Fatalf("%+v", out)
			}
			if !tc.ok && len(f.events) != 0 {
				t.Fatalf("a refused run reached the cloud: %v", f.events)
			}
		})
	}
}

// FUSE is set on the clients exactly for a mount soak: a mount soak without it, or any other run with it, is refused
// before any cloud call.
func TestFuseClientsGoExactlyWithTheMountSoak(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mount bool
		fuse  any
		ok    bool
	}{
		{"mount soak with fuse", true, true, true},
		{"mount soak without fuse", true, nil, false},
		{"mount soak with fuse false", true, false, false},
		{"other run with fuse", false, true, false},
		{"other run with fuse false", false, false, true},
		{"fuse not a boolean", false, "yes", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fixture(t)
			c.Cloud.MountSoak = tc.mount
			b, _ := os.ReadFile(c.Variables)
			var v map[string]any
			_ = json.Unmarshal(b, &v)
			if tc.fuse != nil {
				v["fuse_clients"] = tc.fuse
			}
			if tc.mount {
				v["cluster"] = true
			}
			b, _ = json.Marshal(v)
			_ = os.WriteFile(c.Variables, b, 0600)
			c.VariablesSHA256 = digest(b)
			f := &fake{}
			out := controller(f).Execute(context.Background(), "run", c)
			if refused := out.Code == "INPUT_REFUSED"; refused == tc.ok || !tc.ok && len(f.events) != 0 {
				t.Fatalf("%+v %v", out, f.events)
			}
		})
	}
}

// The mount soak's widened caps are chosen only after its class is proven closed: a mount soak mixed with another
// phase or class, or without the ten-node layout, is refused before any command, even with an estimate only the
// mount soak's caps would allow.
func TestAMixedOrPartialMountSoakNeverReachesTheCloud(t *testing.T) {
	for name, mutate := range map[string]func(*Config, map[string]any){
		"with chaos":         func(c *Config, v map[string]any) { c.Cloud.Chaos = true },
		"with replication":   func(c *Config, v map[string]any) { c.Cloud.Replication = "001" },
		"with chaos set":     func(c *Config, v map[string]any) { c.Cloud.ChaosSet = "A" },
		"with kv soak":       func(c *Config, v map[string]any) { c.Cloud.KVSoak = true },
		"with a read case":   func(c *Config, v map[string]any) { c.Cloud.ReadCase = "mount-soak" },
		"single VM layout":   func(c *Config, v map[string]any) { delete(v, "cluster") },
		"cluster false":      func(c *Config, v map[string]any) { v["cluster"] = false },
		"cluster not a bool": func(c *Config, v map[string]any) { v["cluster"] = "yes" },
		"with ec stage1":     func(c *Config, v map[string]any) { c.Cloud.ECStage1 = true },
		"with master writes": func(c *Config, v map[string]any) { c.Cloud.MasterWrites = true },
	} {
		t.Run(name, func(t *testing.T) {
			c := fixture(t)
			c.Cloud.MountSoak, c.EstimatedUSD, c.PriorUSD = true, 9, 90
			b, _ := os.ReadFile(c.Variables)
			var v map[string]any
			_ = json.Unmarshal(b, &v)
			v["fuse_clients"], v["cluster"] = true, true
			r := v["run_declaration"].(map[string]any)
			r["budget_usd"], r["abort_estimate_usd"] = 10, 10
			mutate(&c, v)
			b, _ = json.Marshal(v)
			_ = os.WriteFile(c.Variables, b, 0600)
			c.VariablesSHA256 = digest(b)
			f := &fake{}
			out := controller(f).Execute(context.Background(), "run", c)
			if out.Code != "INPUT_REFUSED" || len(f.events) != 0 {
				t.Fatalf("code %s, events %v", out.Code, f.events)
			}
		})
	}
}

func TestRecoveryUsesSameSnapshotAndDoesNotCreate(t *testing.T) {
	c := fixture(t)
	f := &fake{fail: "destroy"}
	r := controller(f).Execute(context.Background(), "run", c)
	if r.Code != "CLEANUP_INCOMPLETE" {
		t.Fatal(r)
	}
	next := &fake{}
	r = controller(next).Execute(context.Background(), "destroy", c)
	if r.Code != "COMPLETE" || strings.Join(next.events, ",") != "terraform destroy,az resource" {
		t.Fatalf("%+v %+v", r, next)
	}
	_ = os.WriteFile(filepath.Join(c.Output, "provision", "terraform", "azure-validation", "run.auto.tfvars.json"), []byte(`{}`), 0600)
	next = &fake{}
	r = controller(next).Execute(context.Background(), "destroy", c)
	if r.Code != "INPUT_REFUSED" || len(next.events) > 0 {
		t.Fatal("changed recovery accepted")
	}
}
func TestCloudPackRefusesInvalidInventory(t *testing.T) {
	c := fixture(t)
	_, e := seaweed.RunCloudHTTP(context.Background(), []byte(`{}`), c.Cloud)
	if e == nil {
		t.Fatal("malformed cloud inventory accepted")
	}
}
func TestStrictJSONRefusesAliasesDuplicatesAndTrailing(t *testing.T) {
	for _, raw := range []string{`{"Source":"x"}`, `{"source":"x","source":"y"}`, `{} {}`, `{"surprise":1}`} {
		var c Config
		if StrictJSON([]byte(raw), &c) == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestUnknownApplyStaysUnknownAcrossEmptyRecoveryAndVerify(t *testing.T) {
	c := fixture(t)
	first := controller(&fake{fail: "apply"}).Execute(context.Background(), "run", c)
	if first.Code != "APPLY_FAILED_OR_UNKNOWN" || !first.CreateUnresolved {
		t.Fatal(first)
	}
	for _, mode := range []string{"destroy", "verify", "verify"} {
		r := controller(&fake{}).Execute(context.Background(), mode, c)
		if r.Code != "CREATE_UNRESOLVED" || !r.CreateUnresolved || !r.RemainingObserved || len(r.Remaining) != 0 {
			t.Fatalf("%s: %+v", mode, r)
		}
	}
}

func TestMissingOrMalformedApplyReceiptNeverCertifiesClean(t *testing.T) {
	for _, kind := range []string{"missing", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			c := fixture(t)
			if r := controller(&fake{}).Execute(context.Background(), "run", c); r.Code != "COMPLETE" {
				t.Fatal(r)
			}
			p := filepath.Join(c.Output, "apply-succeeded")
			var e error
			if kind == "missing" {
				e = os.Rename(p, p+".saved")
			} else {
				e = os.WriteFile(p, []byte("partial"), 0600)
			}
			if e != nil {
				t.Fatal(e)
			}
			for _, mode := range []string{"destroy", "verify"} {
				r := controller(&fake{}).Execute(context.Background(), mode, c)
				if r.Code != "CREATE_UNRESOLVED" || !r.CreateUnresolved {
					t.Fatal(r)
				}
			}
		})
	}
}

func TestVerifyTimeoutCannotCertifyAbsence(t *testing.T) {
	c := fixture(t)
	if r := controller(&fake{}).Execute(context.Background(), "run", c); r.Code != "COMPLETE" {
		t.Fatal(r)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	ctl := Controller{Command: func(cx context.Context, _ string, tool string, _ ...string) ([]byte, error) {
		if tool != "az" {
			t.Fatal("verify mutated")
		}
		<-cx.Done()
		return nil, cx.Err()
	}}
	r := ctl.Execute(ctx, "verify", c)
	if r.Code != "RESOURCES_REMAIN" || r.RemainingObserved {
		t.Fatal(r)
	}
}
