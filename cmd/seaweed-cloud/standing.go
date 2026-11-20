package main

import (
	"context"
	"encoding/json"
	"flag"
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

var (
	standingName  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)
	standingRG    = regexp.MustCompile(`^rg-[a-z0-9]+-standing-[0-9]{8}$`)
	standingToken = regexp.MustCompile(`^[0-9a-f]{24}$`)
)

// runStanding drives the standing test box (provision/standing): plan writes a reviewed plan file, apply applies
// exactly that file, start/stop/status act on the VM, destroy removes the whole RG content and lists it empty.
// apply and destroy need the manager's launch token; nothing here creates resources outside the box's RG.
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
	case runtime.GOOS != "linux":
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
	home, err := os.UserHomeDir()
	if err != nil {
		r.Code, r.Reason = "UNKNOWN", "no home directory"
		return
	}
	work := filepath.Join(home, ".local", "state", "seaweed-cloud", "standing", r.Name)
	tf := func(args ...string) ([]byte, error) {
		return smoke.Native(ctx, work, "terraform", append(args, "-input=false", "-no-color")...)
	}
	az := func(args ...string) ([]byte, error) {
		return smoke.Native(ctx, "", "az", append(args, "--resource-group", rg, "--output", "json", "--only-show-errors")...)
	}
	fail := func(what string, out []byte) {
		r.Code, r.Reason = "FAILED", what+": "+lastLine(out)
	}
	switch r.Action {
	case "plan":
		if err := copyModule(module, work); err != nil {
			r.Code, r.Reason = "FAILED", "copy module: "+err.Error()
			return
		}
		varsAbs, _ := filepath.Abs(vars)
		if out, err := tf("init", "-backend=false"); err != nil {
			fail("terraform init", out)
		} else if out, err := tf("plan", "-var-file="+varsAbs, "-out=standing.tfplan"); err != nil {
			fail("terraform plan", out)
		} else if js, err := smoke.Native(ctx, work, "terraform", "show", "-json", "standing.tfplan"); err != nil || os.WriteFile(filepath.Join(work, "standing-plan.json"), js, 0o600) != nil {
			fail("terraform show", js)
		} else {
			r.Code, r.Resources, r.Reason = "OK", planAddresses(js), "plan written to "+filepath.Join(work, "standing.tfplan")+"; review it, then apply with a launch token"
		}
	case "apply":
		if out, err := tf("apply", "standing.tfplan"); err != nil {
			fail("terraform apply (only the reviewed standing.tfplan)", out)
		} else {
			r.Code, r.Reason = "OK", "applied the reviewed plan"
		}
	case "destroy":
		if out, err := tf("destroy", "-auto-approve"); err != nil {
			fail("terraform destroy", out)
		} else if out, err := smoke.Native(ctx, "", "az", "group", "exists", "--name", rg, "--output", "json", "--only-show-errors"); err != nil {
			fail("az group exists", out)
		} else if strings.TrimSpace(string(out)) != "false" {
			if left, err := az("resource", "list"); err != nil || strings.TrimSpace(string(left)) != "[]" {
				r.Code, r.Reason = "NOT_CLEAN", "RG still lists resources after destroy: "+lastLine(left)
				return
			}
			r.Code, r.Reason = "OK", "destroyed; RG kept and lists no resources"
		} else {
			r.Code, r.Reason = "OK", "destroyed; RG gone"
		}
	case "start", "stop", "status":
		verb := map[string]string{"start": "start", "stop": "deallocate"}[r.Action]
		if verb != "" {
			if out, err := az("vm", verb, "--name", r.Name); err != nil {
				fail("az vm "+verb, out)
				return
			}
		}
		out, err := az("vm", "get-instance-view", "--name", r.Name)
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

// classifyPower maps the instance view to a power state and whether compute is billing: an OS-level shutdown
// leaves "stopped" (still allocated, still billing); only "deallocated" stops compute billing.
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
