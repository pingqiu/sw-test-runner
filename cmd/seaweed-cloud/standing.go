package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/pingqiu/sw-test-runner/provision/smoke"
)

// standingResult is the one JSON line every `standing` action prints.
type standingResult struct {
	Schema     string   `json:"schema"`
	Action     string   `json:"action"`
	Code       string   `json:"code"`
	Name       string   `json:"name"`
	PowerState string   `json:"power_state,omitempty"`
	Billing    string   `json:"compute_billing,omitempty"`
	Resources  []string `json:"resources,omitempty"`
	Reason     string   `json:"reason"`
}

// standingIdentity is pinned at plan time: every later action must name the same box, RG and subscription, and
// every Azure call is scoped to them.
type standingIdentity struct {
	Name           string `json:"name"`
	ResourceGroup  string `json:"resource_group_name"`
	SubscriptionID string `json:"subscription_id"`
	VarsSHA256     string `json:"vars_sha256"`
}

// standingApply is the durable apply receipt: "started" is written before terraform apply runs and is replaced by
// "applied" only on success, so a failed, killed or timed-out create stays UNRESOLVED across controller restarts.
type standingApply struct {
	State string `json:"state"`
	At    string `json:"at"`
}

var (
	standingName  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)
	standingRG    = regexp.MustCompile(`^rg-[a-z0-9]+-standing-[0-9]{8}$`)
	standingToken = regexp.MustCompile(`^[0-9a-f]{24}$`)
	standingUUID  = regexp.MustCompile(`^[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$`)

	// test seams: the external command boundary, the OS gate and the state root
	standingTool = smoke.Native
	standingGOOS = runtime.GOOS
	standingHome = os.UserHomeDir
)

const (
	standingVarsFile  = "box.tfvars.json"
	standingIdentFile = "identity.json"
	standingApplyFile = "apply.json"
	standingPlanFile  = "standing.tfplan"
)

// tfArgv puts every option before the positional arguments (Terraform's parser takes options first).
func tfArgv(sub string, opts []string, pos ...string) []string {
	argv := append([]string{sub, "-input=false", "-no-color"}, opts...)
	return append(argv, pos...)
}

// runStanding drives the standing test box (provision/standing). plan pins the inputs, the identity and a reviewed
// plan file; apply applies exactly that file; start/stop/status act on the VM; destroy uses the same pinned inputs
// and verifies the pinned RG. apply and destroy need the manager's launch token.
func runStanding(args []string, w io.Writer) int {
	r := standingResult{Schema: "seaweed.cloud-standing/v1", Code: "USAGE"}
	if len(args) > 0 {
		r.Action = args[0]
	}
	fs := flag.NewFlagSet("standing", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&r.Name, "name", "", "box name (VM name)")
	rg := fs.String("resource-group", "", "the box's own RG, rg-<seat>-standing-<yyyymmdd>")
	module := fs.String("module", "", "path of provision/standing (plan only)")
	vars := fs.String("vars", "", "tfvars JSON for the box (plan only)")
	token := fs.String("launch-token", "", "manager's launch hub id (apply, destroy)")
	ok := len(args) > 0 && fs.Parse(args[1:]) == nil && fs.NArg() == 0 && standingName.MatchString(r.Name) && standingRG.MatchString(*rg)
	needToken := r.Action == "apply" || r.Action == "destroy"
	switch {
	case !ok || !strings.Contains(" plan apply start stop status destroy ", " "+r.Action+" "):
		r.Reason = "use standing plan|apply|start|stop|status|destroy --name NAME --resource-group RG [--module DIR --vars FILE] [--launch-token ID]"
	case r.Action == "plan" && (*module == "" || *vars == ""):
		r.Reason = "plan needs --module and --vars"
	case needToken && !standingToken.MatchString(*token):
		r.Code, r.Reason = "REFUSED", r.Action+" needs --launch-token (24 lowercase hex)"
	case standingGOOS != "linux":
		r.Reason = "standing requires the Linux controller"
	default:
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()
		standingDo(ctx, &r, *rg, *module, *vars)
	}
	if json.NewEncoder(w).Encode(r) != nil {
		return 2
	}
	if r.Code == "OK" {
		return 0
	}
	return 2
}

func standingDo(ctx context.Context, r *standingResult, rg, module, vars string) {
	home, err := standingHome()
	if err != nil {
		r.Code, r.Reason = "UNKNOWN", "no home directory"
		return
	}
	work := filepath.Join(home, ".local", "state", "seaweed-cloud", "standing", r.Name)
	tf := func(sub string, opts []string, pos ...string) ([]byte, error) {
		return standingTool(ctx, work, "terraform", tfArgv(sub, opts, pos...)...)
	}
	fail := func(what string, out []byte) { r.Code, r.Reason = "FAILED", what+": "+lastLine(out) }
	if r.Action == "plan" {
		standingPlan(ctx, r, rg, module, vars, work, tf, fail)
		return
	}
	id, err := readIdentity(work)
	if err != nil {
		r.Code, r.Reason = "REFUSED", "no pinned identity for this box (run plan first): "+err.Error()
		return
	}
	if id.Name != r.Name || id.ResourceGroup != rg {
		r.Code, r.Reason = "REFUSED", fmt.Sprintf("target %s/%s does not match the pinned box %s/%s", rg, r.Name, id.ResourceGroup, id.Name)
		return
	}
	az := func(args ...string) ([]byte, error) {
		return standingTool(ctx, "", "az", append(args, "--subscription", id.SubscriptionID, "--output", "json", "--only-show-errors")...)
	}
	varsOpt := []string{"-var-file=" + filepath.Join(work, standingVarsFile)}
	prior, _ := readApply(work)
	switch r.Action {
	case "apply":
		if prior.State == "started" || prior.State == "unresolved" {
			r.Code, r.Reason = "UNRESOLVED", "a previous apply did not complete ("+prior.State+" "+prior.At+"): resolve it from the provider's operations before any new create; no blind retry"
			return
		}
		if writeApply(work, "started") != nil {
			r.Code, r.Reason = "UNKNOWN", "cannot write the apply receipt; nothing applied"
			return
		}
		if out, err := tf("apply", nil, standingPlanFile); err != nil {
			_ = writeApply(work, "unresolved")
			r.Code, r.Reason = "UNRESOLVED", "terraform apply failed; the create stays UNRESOLVED until resolved: "+lastLine(out)
			return
		}
		if writeApply(work, "applied") != nil {
			r.Code, r.Reason = "UNKNOWN", "applied, but the receipt could not be written"
			return
		}
		r.Code, r.Reason = "OK", "applied the reviewed plan"
	case "destroy":
		if out, err := tf("destroy", append(varsOpt, "-auto-approve")); err != nil {
			fail("terraform destroy", out)
			return
		}
		code, reason := standingVerifyGone(az, id)
		if code == "OK" && (prior.State == "started" || prior.State == "unresolved") {
			r.Code, r.Reason = "UNRESOLVED", "destroyed and "+reason+", but a previous apply is unresolved: an empty observation does not close it"
			return
		}
		if code == "OK" {
			_ = writeApply(work, "destroyed")
		}
		r.Code, r.Reason = code, reason
	case "start", "stop", "status":
		verb := map[string]string{"start": "start", "stop": "deallocate"}[r.Action]
		if verb != "" {
			if out, err := az("vm", verb, "--resource-group", id.ResourceGroup, "--name", id.Name); err != nil {
				fail("az vm "+verb, out)
				return
			}
		}
		out, err := az("vm", "get-instance-view", "--resource-group", id.ResourceGroup, "--name", id.Name)
		if err != nil {
			fail("az vm get-instance-view", out)
			return
		}
		r.PowerState, r.Billing = classifyPower(out)
		r.Code, r.Reason = "OK", "power state read after "+r.Action
		if r.PowerState == "UNKNOWN" {
			r.Code, r.Reason = "UNKNOWN", "no PowerState in the instance view"
		}
	}
}

func standingPlan(ctx context.Context, r *standingResult, rg, module, vars, work string,
	tf func(string, []string, ...string) ([]byte, error), fail func(string, []byte)) {
	b, err := os.ReadFile(vars)
	var v struct {
		ResourceGroup  string `json:"resource_group_name"`
		Name           string `json:"name"`
		SubscriptionID string `json:"subscription_id"`
	}
	if err != nil || json.Unmarshal(b, &v) != nil {
		r.Code, r.Reason = "REFUSED", "unreadable --vars JSON"
		return
	}
	if v.ResourceGroup != rg || v.Name != r.Name || !standingUUID.MatchString(v.SubscriptionID) {
		r.Code, r.Reason = "REFUSED", "--vars must name the same resource_group_name and name as the command and carry a subscription_id UUID"
		return
	}
	if old, err := readIdentity(work); err == nil && (old.ResourceGroup != rg || old.SubscriptionID != v.SubscriptionID) {
		r.Code, r.Reason = "REFUSED", "this box's state is pinned to "+old.ResourceGroup+"; destroy it before planning another target"
		return
	}
	if prior, _ := readApply(work); prior.State == "started" || prior.State == "unresolved" {
		r.Code, r.Reason = "UNRESOLVED", "a previous apply is unresolved; no new plan until it is resolved"
		return
	}
	id := standingIdentity{Name: r.Name, ResourceGroup: rg, SubscriptionID: v.SubscriptionID, VarsSHA256: fmt.Sprintf("%x", sha256.Sum256(b))}
	idb, _ := json.Marshal(id)
	if err := copyModule(module, work); err != nil {
		r.Code, r.Reason = "FAILED", "copy module: "+err.Error()
		return
	}
	if os.WriteFile(filepath.Join(work, standingVarsFile), b, 0o600) != nil || os.WriteFile(filepath.Join(work, standingIdentFile), idb, 0o600) != nil {
		r.Code, r.Reason = "FAILED", "cannot pin the inputs and identity"
		return
	}
	varsOpt := []string{"-var-file=" + filepath.Join(work, standingVarsFile)}
	if out, err := tf("init", []string{"-backend=false"}); err != nil {
		fail("terraform init", out)
	} else if out, err := tf("plan", append(varsOpt, "-out="+standingPlanFile)); err != nil {
		fail("terraform plan", out)
	} else if js, err := standingTool(ctx, work, "terraform", "show", "-json", standingPlanFile); err != nil || os.WriteFile(filepath.Join(work, "standing-plan.json"), js, 0o600) != nil {
		fail("terraform show", js)
	} else {
		r.Code, r.Resources, r.Reason = "OK", planAddresses(js), "plan written to "+filepath.Join(work, standingPlanFile)+"; review it, then apply with a launch token"
	}
}

// standingVerifyGone checks the pinned RG in the pinned subscription: gone, or present and listing no resources.
func standingVerifyGone(az func(...string) ([]byte, error), id standingIdentity) (string, string) {
	out, err := az("group", "exists", "--name", id.ResourceGroup)
	if err != nil {
		return "UNKNOWN", "az group exists failed: " + lastLine(out)
	}
	switch strings.TrimSpace(string(out)) {
	case "false":
		return "OK", "RG gone"
	case "true":
		left, err := az("resource", "list", "--resource-group", id.ResourceGroup)
		var items []json.RawMessage
		if err != nil || json.Unmarshal(left, &items) != nil || items == nil {
			return "UNKNOWN", "RG resource list unreadable: " + lastLine(left)
		}
		if len(items) > 0 {
			return "NOT_CLEAN", fmt.Sprintf("RG still lists %d resource(s) after destroy", len(items))
		}
		return "OK", "RG kept and lists no resources"
	}
	return "UNKNOWN", "unexpected az group exists output: " + lastLine(out)
}

func readIdentity(work string) (standingIdentity, error) {
	var id standingIdentity
	b, err := os.ReadFile(filepath.Join(work, standingIdentFile))
	if err == nil {
		err = json.Unmarshal(b, &id)
	}
	if err == nil && (!standingName.MatchString(id.Name) || !standingRG.MatchString(id.ResourceGroup) || !standingUUID.MatchString(id.SubscriptionID)) {
		err = fmt.Errorf("invalid pinned identity")
	}
	return id, err
}

func readApply(work string) (standingApply, error) {
	var a standingApply
	b, err := os.ReadFile(filepath.Join(work, standingApplyFile))
	if err == nil {
		err = json.Unmarshal(b, &a)
	}
	return a, err
}

func writeApply(work, state string) error {
	b, _ := json.Marshal(standingApply{State: state, At: time.Now().UTC().Format(time.RFC3339)})
	tmp := filepath.Join(work, standingApplyFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(work, standingApplyFile))
}

// classifyPower maps the instance view to a power state and whether compute is billing: an OS-level shutdown
// leaves "stopped" (still allocated, still billing); "deallocating"/"deallocated" are not compute-billed.
func classifyPower(view []byte) (string, string) {
	var v struct {
		InstanceView struct{ Statuses []struct{ Code string } } `json:"instanceView"`
	}
	if json.Unmarshal(view, &v) != nil {
		return "UNKNOWN", "UNKNOWN"
	}
	for _, s := range v.InstanceView.Statuses {
		if state, found := strings.CutPrefix(s.Code, "PowerState/"); found {
			switch state {
			case "deallocated", "deallocating":
				return state, "no"
			case "running", "starting", "stopped", "stopping":
				return state, "yes"
			}
			return state, "UNKNOWN"
		}
	}
	return "UNKNOWN", "UNKNOWN"
}

func planAddresses(plan []byte) []string {
	var p struct {
		ResourceChanges []struct {
			Address string
			Change  struct{ Actions []string }
		} `json:"resource_changes"`
	}
	out := []string{}
	if json.Unmarshal(plan, &p) == nil {
		for _, c := range p.ResourceChanges {
			out = append(out, strings.Join(c.Change.Actions, "+")+" "+c.Address)
		}
	}
	return out
}

// copyModule copies the module's top-level .tf, cloud-init and lock files into the work dir (state stays there).
func copyModule(src, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !(strings.HasSuffix(n, ".tf") || n == "cloud-init.yaml" || n == ".terraform.lock.hcl") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, n))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, n), b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines[len(lines)-1]
}
