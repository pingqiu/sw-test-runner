package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	tr "github.com/pingqiu/sw-test-runner"
	"github.com/pingqiu/sw-test-runner/packs/seaweed"
	"os"
	"os/signal"
	"syscall"
)

// This entry deliberately does not call pkgRegister: legacy actions that start
// local builds or raw shell processes are not approved owned-job executors.
func approvedRunCmd(args []string) int {
	fs := flag.NewFlagSet("approved-run", flag.ContinueOnError)
	request := fs.String("request", "", "exact worker-claimed JSON request")
	jsonMode := fs.Bool("json", false, "emit stable approved-run result JSON")
	policy := fs.String("policy", "", "protected service bootstrap")
	if err := fs.Parse(args); err != nil {
		if *jsonMode {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"schema": "seaweed.approved-result/v1", "status": "FAIL", "code": "USAGE", "remedy": "use --json --request CLAIMED --policy PROTECTED only"})
		}
		return 2
	}
	if *request == "" || *policy == "" || fs.NArg() != 0 {
		if *jsonMode {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"schema": "seaweed.approved-result/v1", "status": "FAIL", "code": "USAGE", "remedy": "supply --request and --policy only"})
		} else {
			fmt.Fprintln(os.Stderr, "approved-run requires --request and --policy only")
		}
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	result, bundle, err := tr.RunApprovedRequest(ctx, *request, *policy, seaweed.RegisterPack, nil, func(f string, a ...interface{}) {
		if !*jsonMode {
			fmt.Fprintf(os.Stdout, f+"\n", a...)
		}
	})
	if *jsonMode {
		code, status, remedy := "OK", "PASS", "inspect the bound evidence bundle"
		if err != nil {
			code, status, remedy = "S0B_ADMISSION_OR_EVIDENCE_REFUSED", "FAIL", "restore exact protected authority and retain UNKNOWN evidence"
		} else if result == nil || result.Status != tr.StatusPass {
			code, status, remedy = "S0B_RUN_FAILED", "FAIL", "inspect phase and retained cleanup failure; do not bypass admission"
		}
		if e := json.NewEncoder(os.Stdout).Encode(map[string]any{"schema": "seaweed.approved-result/v1", "status": status, "code": code, "remedy": remedy, "bundle": bundle, "result": result}); e != nil {
			return 1
		}
		if status != "PASS" {
			return 1
		}
		return 0
	}
	if bundle != "" {
		fmt.Fprintln(os.Stdout, "bundle:", bundle)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if result == nil || result.Status != tr.StatusPass {
		if result != nil {
			fmt.Fprintln(os.Stderr, result.Error)
		}
		return 1
	}
	return 0
}
