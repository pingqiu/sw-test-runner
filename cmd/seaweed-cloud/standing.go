package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
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
	Generation string   `json:"generation,omitempty"`
	PowerState string   `json:"power_state,omitempty"`
	Billing    string   `json:"compute_billing,omitempty"`
	Resources  []string `json:"resources,omitempty"`
	Reason     string   `json:"reason"`
}

// standingIdentity is pinned in each plan generation: every later action must name the same box, RG and
// subscription, and every Azure call is scoped to them.
type standingIdentity struct {
	Name           string `json:"name"`
	ResourceGroup  string `json:"resource_group_name"`
	SubscriptionID string `json:"subscription_id"`
	VarsSHA256     string `json:"vars_sha256"`
}

// standingApply is a generation's durable apply receipt: "started" before terraform apply runs, "applied" only on
// success, "unresolved" on failure, "destroyed" after a verified teardown.
type standingApply struct {
	State string `json:"state"`
	At    string `json:"at"`
}

var (
	standingName  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)
	standingRG    = regexp.MustCompile(`^rg-[a-z0-9]+-standing-[0-9]{8}$`)
	standingToken = regexp.MustCompile(`^[0-9a-f]{24}$`)
	standingUUID  = regexp.MustCompile(`^[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$`)
	standingGen   = regexp.MustCompile(`^g[0-9]{4}$`)

	// test seams: the external command boundary, the OS gate and the state root
	standingTool  = smoke.Native
	standingGOOS  = runtime.GOOS
	standingHome  = os.UserHomeDir
	standingLstat = os.Lstat
)

// Layout under ~/.local/state/seaweed-cloud/standing/<name>/:
//
//	gNNNN/    one plan generation: module copy, box.tfvars.json, identity.json, standing.tfplan, Terraform state,
//	          READY (the plan completed), ATTEMPT (an apply was started here), apply.json (receipt)
//	CURRENT   the one apply-eligible generation; removed when any plan starts, written only after a plan succeeds
//
// The live generation is the one with an ATTEMPT whose receipt is not "destroyed"; teardown is bound to it.
const (
	standingVarsFile    = "box.tfvars.json"
	standingIdentFile   = "identity.json"
	standingApplyFile   = "apply.json"
	standingPlanFile    = "standing.tfplan"
	standingReadyFile   = "READY"
	standingAttemptFile = "ATTEMPT"
	standingCurrentFile = "CURRENT"
)

// tfArgv puts every option before the positional arguments (Terraform's parser takes options first).
func tfArgv(sub string, opts []string, pos ...string) []string {
	argv := append([]string{sub, "-input=false", "-no-color"}, opts...)
	return append(argv, pos...)
}

// runStanding drives the standing test box (provision/standing). plan writes a new generation; apply applies
// exactly the CURRENT generation's reviewed plan; start/stop/status/destroy act on the live (applied) generation
// with its own pinned inputs and scope. apply and destroy need the manager's launch token.
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
	box := filepath.Join(home, ".local", "state", "seaweed-cloud", "standing", r.Name)
	live, liveState, err := standingLive(box)
	if err != nil {
		r.Code, r.Reason = "UNKNOWN", err.Error()
		return
	}
	pending := liveState == "started" || liveState == "unresolved" || liveState == "unknown"
	if r.Action == "plan" {
		switch {
		case pending:
			r.Code, r.Reason = "UNRESOLVED", "generation "+live+" has an unresolved apply (receipt "+liveState+"); no new plan until it is resolved"
		case live != "":
			r.Code, r.Reason = "REFUSED", "generation "+live+" is applied; destroy it before planning again"
		default:
			standingPlan(ctx, r, box, rg, module, vars)
		}
		return
	}
	gen := live
	if r.Action == "apply" {
		switch {
		case pending:
			r.Code, r.Reason = "UNRESOLVED", "generation "+live+" has an unresolved apply (receipt "+liveState+"): resolve it from the provider's operations before any new create; no blind retry"
			return
		case live != "":
			r.Code, r.Reason = "REFUSED", "generation "+live+" is already applied; destroy it before another apply"
			return
		}
		cur, err := os.ReadFile(filepath.Join(box, standingCurrentFile))
		gen = strings.TrimSpace(string(cur))
		if err != nil || !standingGen.MatchString(gen) {
			r.Code, r.Reason = "REFUSED", "no apply-eligible plan (no completed CURRENT generation): run plan"
			return
		}
		if ready, err := present(filepath.Join(box, gen, standingReadyFile)); err != nil {
			r.Code, r.Reason = "UNKNOWN", "cannot read generation "+gen+" evidence: "+err.Error()
			return
		} else if !ready {
			r.Code, r.Reason = "REFUSED", "CURRENT generation "+gen+" has no completed plan: run plan"
			return
		}
	} else if gen == "" {
		r.Code, r.Reason = "REFUSED", "no applied generation for this box"
		return
	}
	r.Generation = gen
	work := filepath.Join(box, gen)
	id, err := readIdentity(work)
	if err != nil {
		r.Code, r.Reason = "REFUSED", "generation "+gen+" identity unreadable: "+err.Error()
		return
	}
	if id.Name != r.Name || id.ResourceGroup != rg {
		r.Code, r.Reason = "REFUSED", fmt.Sprintf("target %s/%s does not match generation %s's box %s/%s", rg, r.Name, gen, id.ResourceGroup, id.Name)
		return
	}
	if err := checkPinnedVars(work, id); err != nil {
		r.Code, r.Reason = "REFUSED", "pinned inputs do not match the pinned identity: "+err.Error()
		return
	}
	tf := func(sub string, opts []string, pos ...string) ([]byte, error) {
		return standingTool(ctx, work, "terraform", tfArgv(sub, opts, pos...)...)
	}
	az := func(args ...string) ([]byte, error) {
		return standingTool(ctx, "", "az", append(args, "--subscription", id.SubscriptionID, "--output", "json", "--only-show-errors")...)
	}
	fail := func(what string, out []byte) { r.Code, r.Reason = "FAILED", what+": "+lastLine(out) }
	switch r.Action {
	case "apply":
		// ATTEMPT first: from here on this generation is never "new" again; a lost receipt reads as unknown
		if writeFileSync(filepath.Join(work, standingAttemptFile), []byte(time.Now().UTC().Format(time.RFC3339))) != nil || writeApply(work, "started") != nil {
			r.Code, r.Reason = "UNKNOWN", "cannot record the apply attempt; nothing applied"
			return
		}
		if out, err := tf("apply", nil, standingPlanFile); err != nil {
			_ = writeApply(work, "unresolved")
			r.Code, r.Reason = "UNRESOLVED", "terraform apply failed; generation "+gen+" stays UNRESOLVED until resolved: "+lastLine(out)
			return
		}
		if writeApply(work, "applied") != nil {
			r.Code, r.Reason = "UNKNOWN", "applied, but the receipt could not be written"
			return
		}
		r.Code, r.Reason = "OK", "applied generation "+gen
	case "destroy":
		if out, err := tf("destroy", []string{"-var-file=" + filepath.Join(work, standingVarsFile), "-auto-approve"}); err != nil {
			fail("terraform destroy", out)
			return
		}
		code, reason := standingVerifyGone(az, id)
		if code == "OK" && pending {
			r.Code, r.Reason = "UNRESOLVED", "destroyed and "+reason+", but generation "+gen+"'s apply is unresolved (receipt "+liveState+"): an empty observation does not close it"
			return
		}
		if code == "OK" && writeApply(work, "destroyed") != nil {
			code, reason = "UNKNOWN", reason+", but the destroyed receipt could not be written"
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

// standingPlan revokes apply eligibility first, then builds a NEW generation; CURRENT names it only once the
// plan has completed, so a failed re-plan leaves no eligible plan at all.
func standingPlan(ctx context.Context, r *standingResult, box, rg, module, vars string) {
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
	gens, err := standingGens(box)
	if err != nil {
		r.Code, r.Reason = "UNKNOWN", "cannot list generations: "+err.Error()
		return
	}
	if n := len(gens); n > 0 {
		if has, err := present(filepath.Join(box, gens[n-1], standingIdentFile)); err != nil {
			r.Code, r.Reason = "UNKNOWN", "cannot read generation "+gens[n-1]+" evidence: "+err.Error()
			return
		} else if has {
			if _, err := readIdentity(filepath.Join(box, gens[n-1])); err != nil {
				r.Code, r.Reason = "REFUSED", "generation "+gens[n-1]+" identity is unreadable: "+err.Error()
				return
			}
		}
	}
	if err := os.MkdirAll(box, 0o700); err != nil {
		r.Code, r.Reason = "FAILED", "state dir: "+err.Error()
		return
	}
	if err := os.Remove(filepath.Join(box, standingCurrentFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		r.Code, r.Reason = "UNKNOWN", "cannot revoke the previous plan's eligibility: "+err.Error()
		return
	}
	gen := fmt.Sprintf("g%04d", len(gens)+1)
	if len(gens) > 0 {
		var last int
		fmt.Sscanf(gens[len(gens)-1], "g%04d", &last)
		gen = fmt.Sprintf("g%04d", last+1)
	}
	work := filepath.Join(box, gen)
	if err := os.Mkdir(work, 0o700); err != nil {
		r.Code, r.Reason = "FAILED", "new generation "+gen+": "+err.Error()
		return
	}
	r.Generation = gen
	id := standingIdentity{Name: r.Name, ResourceGroup: rg, SubscriptionID: v.SubscriptionID, VarsSHA256: fmt.Sprintf("%x", sha256.Sum256(b))}
	idb, _ := json.Marshal(id)
	if err := copyModule(module, work); err != nil {
		r.Code, r.Reason = "FAILED", "copy module: "+err.Error()
		return
	}
	if writeFileSync(filepath.Join(work, standingVarsFile), b) != nil || writeFileSync(filepath.Join(work, standingIdentFile), idb) != nil {
		r.Code, r.Reason = "FAILED", "cannot pin the inputs and identity"
		return
	}
	tf := func(sub string, opts []string, pos ...string) ([]byte, error) {
		return standingTool(ctx, work, "terraform", tfArgv(sub, opts, pos...)...)
	}
	if out, err := tf("init", []string{"-backend=false"}); err != nil {
		r.Code, r.Reason = "FAILED", "terraform init: "+lastLine(out)
	} else if out, err := tf("plan", []string{"-var-file=" + filepath.Join(work, standingVarsFile), "-out=" + standingPlanFile}); err != nil {
		r.Code, r.Reason = "FAILED", "terraform plan (no plan is apply-eligible now): "+lastLine(out)
	} else if js, err := standingTool(ctx, work, "terraform", "show", "-json", standingPlanFile); err != nil || writeFileSync(filepath.Join(work, "standing-plan.json"), js) != nil {
		r.Code, r.Reason = "FAILED", "terraform show: "+lastLine(js)
	} else if writeFileSync(filepath.Join(work, standingReadyFile), []byte(id.VarsSHA256)) != nil || writeFileSync(filepath.Join(box, standingCurrentFile), []byte(gen)) != nil {
		r.Code, r.Reason = "FAILED", "cannot mark generation "+gen+" apply-eligible"
	} else {
		r.Code, r.Resources, r.Reason = "OK", planAddresses(js), "generation "+gen+" planned ("+filepath.Join(work, standingPlanFile)+"); review it, then apply with a launch token"
	}
}

// standingGens lists the plan generations in order.
func standingGens(box string) ([]string, error) {
	entries, err := os.ReadDir(box)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var gens []string
	for _, e := range entries {
		if e.IsDir() && standingGen.MatchString(e.Name()) {
			gens = append(gens, e.Name())
		}
	}
	sort.Strings(gens)
	return gens, nil
}

// standingLive finds the generation an apply was attempted in and not verified destroyed. A generation counts as
// attempted when it has an ATTEMPT marker, a receipt or Terraform state; its receipt is then "unknown" when missing,
// unreadable or naming an unknown state. More than one live generation is an error (UNKNOWN).
func standingLive(box string) (string, string, error) {
	gens, err := standingGens(box)
	if err != nil {
		return "", "", fmt.Errorf("cannot list generations: %v", err)
	}
	live, state := "", ""
	for _, g := range gens {
		work := filepath.Join(box, g)
		used := false
		for _, f := range []string{standingAttemptFile, standingApplyFile, "terraform.tfstate"} {
			has, err := present(filepath.Join(work, f))
			if err != nil {
				return "", "", fmt.Errorf("cannot read generation %s evidence: %v", g, err)
			}
			used = used || has
		}
		if !used {
			continue
		}
		s := "unknown"
		if a, err := readApply(work); err == nil && strings.Contains(" started unresolved applied destroyed ", " "+a.State+" ") {
			s = a.State
		}
		if s == "destroyed" {
			continue
		}
		if live != "" {
			return "", "", fmt.Errorf("generations %s and %s both hold an unsettled apply", live, g)
		}
		live, state = g, s
	}
	return live, state, nil
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

// checkPinnedVars proves the pinned tfvars are the reviewed bytes and name the pinned identity.
func checkPinnedVars(work string, id standingIdentity) error {
	b, err := os.ReadFile(filepath.Join(work, standingVarsFile))
	if err != nil {
		return err
	}
	if fmt.Sprintf("%x", sha256.Sum256(b)) != id.VarsSHA256 {
		return fmt.Errorf("%s changed since plan (sha256 differs)", standingVarsFile)
	}
	var v struct {
		ResourceGroup  string `json:"resource_group_name"`
		Name           string `json:"name"`
		SubscriptionID string `json:"subscription_id"`
	}
	if json.Unmarshal(b, &v) != nil || v.ResourceGroup != id.ResourceGroup || v.Name != id.Name || v.SubscriptionID != id.SubscriptionID {
		return fmt.Errorf("%s names another box, RG or subscription", standingVarsFile)
	}
	return nil
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
	return writeFileSync(filepath.Join(work, standingApplyFile), b)
}

// writeFileSync writes atomically (temp file, fsync, rename) with owner-only permissions.
func writeFileSync(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// present reports whether generation evidence exists; any error other than "does not exist" is returned so the
// caller fails closed (UNKNOWN) instead of reading it as absent.
func present(path string) (bool, error) {
	_, err := standingLstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
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

// copyModule copies the module's top-level .tf, cloud-init and lock files into a generation dir.
func copyModule(src, dst string) error {
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
