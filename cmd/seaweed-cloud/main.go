// seaweed-cloud is a Linux-only, one-VM smoke lifecycle command, never an
// alternate workload runner. It calls the shared pack's direct cloud HTTP entry.
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
	"syscall"

	"github.com/pingqiu/sw-test-runner/provision/smoke"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout)) }
func run(args []string, w io.Writer) int {
	if len(args) > 0 && args[0] == "settle" {
		return runSettle(args[1:], w)
	}
	out := smoke.Result{Schema: "seaweed.cloud-smoke/v1", Code: "USAGE", Remedy: "use plan|run|destroy|verify --config PRIVATE.json, cleanup --run ID [--dry-run], or settle --resource-group RG --vm VM; plan is local-only", Effects: []string{}, Remaining: []string{}, RemainingSummary: "billable resources remaining: UNKNOWN (not checked)"}
	fs := flag.NewFlagSet("seaweed-cloud", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	config := fs.String("config", "", "pinned controller config")
	runID := fs.String("run", "", "saved run ID under ~/.local/state/seaweed-cloud/runs")
	dry := fs.Bool("dry-run", false, "describe saved-state cleanup without cloud calls")
	source := fs.String("source", "", "source root for local pins command")
	variables := fs.String("variables", "", "tfvars JSON for local pins command")
	launchToken := fs.String("launch-token", "", "the manager's LAUNCH hub id (24 lowercase hex); run refuses without it")
	pack := fs.String("pack", "", "what runs between inventory and destroy: seaweed (default) or exec (config pack_exec)")
	fs.Bool("json", false, "JSON output (always enabled)")
	if len(args) > 0 && args[0] == "direct" {
		return runDirect(args[1:], w)
	}
	if len(args) > 0 && args[0] == "direct-status" {
		return directStatus(args[1:], w)
	}
	if len(args) > 0 && args[0] == "claims" {
		return listClaims(w)
	}
	if len(args) > 0 && args[0] == "pins" && fs.Parse(args[1:]) == nil && fs.NArg() == 0 {
		_, pin, e := smoke.Sources(*source)
		b, ve := os.ReadFile(*variables)
		if e == nil && ve == nil {
			out.Code = "PLAN_ONLY"
			out.Remedy = "freeze these pins in the reviewed config; no cloud calls performed"
			out.Plan = map[string]any{"module_sha256": pin, "variables_sha256": fmt.Sprintf("%x", sha256.Sum256(b))}
		} else {
			out.Code = "INPUT_REFUSED"
			out.Error = fmt.Sprint(e, ve)
		}
		_ = json.NewEncoder(w).Encode(out)
		if out.Code == "PLAN_ONLY" {
			return 0
		}
		return 1
	}
	if len(args) > 0 && fs.Parse(args[1:]) == nil && fs.NArg() == 0 {
		if *dry && (args[0] == "run" || args[0] == "verify") {
			args[0] = "plan"
		}
		if args[0] == "cleanup" && regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,49}$`).MatchString(*runID) {
			home, err := os.UserHomeDir()
			if err != nil || !filepath.IsAbs(home) {
				out.Code, out.Remedy = "INPUT_REFUSED", "resolve an absolute controller HOME before cleanup --run"
				_ = json.NewEncoder(w).Encode(out)
				return 1
			}
			*config = filepath.Join(home, ".local", "state", "seaweed-cloud", "runs", *runID, "recovery.json")
			args[0] = "destroy"
		}
		b, e := os.ReadFile(*config)
		var c smoke.Config
		if e == nil {
			e = smoke.StrictJSON(b, &c)
		}
		if *launchToken != "" {
			c.LaunchToken = *launchToken // per run from the manager; the config file is the pinned part
		}
		if *pack != "" {
			c.PackName = *pack
		}
		if e != nil {
			out.Code = "INPUT_REFUSED"
			out.Error = e.Error()
		} else if *dry && args[0] == "destroy" {
			out.Code, out.Plan = "PLAN_ONLY", map[string]any{"state_root": c.Output, "steps": []string{"same-state terraform destroy", "full-RG list", "subscription residue list"}}
		} else if runtime.GOOS != "linux" && args[0] != "plan" {
			out.Code = "LINUX_REQUIRED"
			out.Remedy = "credentials and lifecycle commands stay on the Linux controller"
		} else {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			dir, err := smoke.DefaultClaimsDir()
			if err != nil || !filepath.IsAbs(dir) {
				out.Code, out.Error, out.Remedy = "INPUT_REFUSED", fmt.Sprint(err), "resolve an absolute controller HOME (resource-group claims live under it)"
			} else {
				out = (smoke.Controller{ClaimsDir: dir}).Execute(ctx, args[0], c)
			}
		}
	}
	if e := smoke.EncodeVerbatim(w, out); e != nil {
		fmt.Fprintln(os.Stderr, "OUTPUT_FAILED")
		return 1
	}
	if out.Code == "COMPLETE" || out.Code == "EMPTY" || out.Code == "PLAN_ONLY" {
		return 0
	}
	return 1
}

// listClaims prints every resource-group claim (#140) with its controller's
// liveness and whether its run's output is settled. It never releases one:
// a claim goes only when its run (or cleanup of it) records a clean outcome.
func listClaims(w io.Writer) int {
	dir, err := smoke.DefaultClaimsDir()
	var claims []smoke.ClaimStatus
	if err == nil {
		claims, err = smoke.Claims(dir)
	}
	rec := map[string]any{"schema": "seaweed.cloud-claims/v1", "claims_dir": dir, "claims": claims}
	if err != nil {
		rec["error"] = err.Error()
	}
	_ = json.NewEncoder(w).Encode(rec)
	if err != nil {
		return 1
	}
	return 0
}
