package cli

import (
	"context"
	"flag"
	"fmt"
	tr "github.com/pingqiu/sw-test-runner"
	"os"
	"os/signal"
	"syscall"
)

// This entry deliberately does not call pkgRegister: legacy actions that start
// local builds or raw shell processes are not approved owned-job executors.
func approvedRunCmd(args []string) int {
	fs := flag.NewFlagSet("approved-run", flag.ContinueOnError)
	request := fs.String("request", "", "exact worker-claimed JSON request")
	policy := fs.String("policy", "", "protected service bootstrap")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *request == "" || *policy == "" || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "approved-run requires --request and --policy only")
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	result, bundle, err := tr.RunApprovedRequest(ctx, *request, *policy, nil, nil, func(f string, a ...interface{}) { fmt.Fprintf(os.Stdout, f+"\n", a...) })
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
