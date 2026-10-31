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
