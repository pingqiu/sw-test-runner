package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/pingqiu/sw-test-runner/provision/smoke"
)

// runSweep is `seaweed-cloud sweep`: the scheduled recovery sweep of the dedicated resource group (smoke.Sweep).
// --listing replays a recorded resource list and is accepted only with --dry-run: then no Azure call is made at all.
// Each sweep writes its receipt and its az journal (NNN-az-{intent,output,status}) to a fresh directory under
// --receipts. Exit 0 when the sweep completed, 1 on any error; the caller reports when the receipt says Acted.
func runSweep(args []string, w io.Writer) int {
	fs := flag.NewFlagSet("seaweed-cloud sweep", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sub := fs.String("subscription", "", "subscription id")
	group := fs.String("resource-group", "", "the dedicated resource group, and only it")
	receipts := fs.String("receipts", "", "directory for this sweep's receipt and journal")
	dry := fs.Bool("dry-run", false, "decide and report; delete nothing")
	listing := fs.String("listing", "", "dry run only: a recorded `az resource list` output to decide from, no Azure call")
	fail := func(e error) int {
		_ = json.NewEncoder(w).Encode(map[string]string{"schema": "seaweed.cloud-sweep/v1", "error": e.Error()})
		return 1
	}
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return fail(errors.New("use: sweep --subscription ID --resource-group RG --receipts DIR [--dry-run [--listing FILE]]"))
	}
	uuid := regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)
	switch {
	case !uuid.MatchString(*sub) || !regexp.MustCompile(`^[A-Za-z0-9_-]{1,90}$`).MatchString(*group) || !filepath.IsAbs(*receipts):
		return fail(errors.New("sweep needs a subscription UUID, a resource group name and an absolute receipts directory"))
	case *listing != "" && !*dry:
		return fail(errors.New("--listing decides from a recording: it is accepted only with --dry-run"))
	case *listing == "" && runtime.GOOS != "linux":
		return fail(errors.New("LINUX_REQUIRED: Azure calls stay on the Linux controller"))
	}
	now := time.Now().UTC()
	dir := filepath.Join(*receipts, "sweep-"+now.Format("20060102T150405Z"))
	if err := os.MkdirAll(*receipts, 0700); err != nil {
		return fail(err)
	}
	if err := os.Mkdir(dir, 0700); err != nil { // fresh: never mixes two sweeps' journals
		return fail(err)
	}
	seq := 0
	call := func(ctx context.Context, tool string, args ...string) ([]byte, error) {
		seq++
		label := filepath.Join(dir, fmt.Sprintf("%03d-%s", seq, tool))
		intent, _ := json.Marshal(args)
		if e := os.WriteFile(label+"-intent.json", intent, 0600); e != nil {
			return nil, e
		}
		var b []byte
		var e error
		switch {
		case *listing != "" && tool == "az" && len(args) > 1 && args[0] == "resource" && args[1] == "list":
			b, e = os.ReadFile(*listing)
		case *listing != "":
			e = errors.New("a recorded-listing dry run makes no other call")
		default:
			b, e = smoke.Native(ctx, "", tool, args...)
		}
		status := "OK"
		if e != nil {
			status = e.Error()
		}
		e = errors.Join(e, os.WriteFile(label+"-output", b, 0600), os.WriteFile(label+"-status", []byte(status), 0600))
		return b, e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	r := smoke.Sweep(ctx, *sub, *group, now, *dry, sweepLive, call)
	raw, _ := json.MarshalIndent(r, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "receipt.json"), append(raw, '\n'), 0600); err != nil {
		return fail(err)
	}
	if err := json.NewEncoder(w).Encode(r); err != nil || r.Error != "" {
		return 1
	}
	return 0
}

// sweepLive is the live-controller check; a var so a test can pin it.
var sweepLive = liveController

// liveController: a seaweed-cloud process whose command line names this run (its config or state path carries the
// run id). When /proc, or a still-present process's command line, cannot be read it answers true: an unknown
// controller is never treated as dead. Only a process proven gone is skipped.
func liveController(run string) bool { return liveControllerAt("/proc", run) }

func liveControllerAt(proc, run string) bool {
	entries, err := os.ReadDir(proc)
	if err != nil {
		return true
	}
	self := fmt.Sprint(os.Getpid())
	for _, e := range entries {
		if !e.IsDir() || strings.Trim(e.Name(), "0123456789") != "" || e.Name() == self {
			continue
		}
		cmd, err := os.ReadFile(filepath.Join(proc, e.Name(), "cmdline"))
		if err != nil {
			if _, serr := os.Stat(filepath.Join(proc, e.Name())); errors.Is(serr, fs.ErrNotExist) {
				continue // exited while listing
			}
			return true
		}
		argv := strings.Split(strings.TrimRight(string(cmd), "\x00"), "\x00")
		if len(argv) > 1 && filepath.Base(argv[0]) == "seaweed-cloud" && argv[1] != "sweep" && strings.Contains(string(cmd), run) {
			return true
		}
	}
	return false
}
