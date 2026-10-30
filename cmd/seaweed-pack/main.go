// seaweed-pack exposes local planning only. Runtime uses approved-run's strict
// service adapter; there is no run, SSH, upload, apply or fallback command here.
package main

import (
	"encoding/json"
	"fmt"
	"github.com/pingqiu/sw-test-runner/packs/seaweed"
	"io"
	"os"
)

func main() { os.Exit(run(os.Args[1:])) }
func run(args []string) int {
	out := seaweed.Result{Schema: "seaweed.pack-result/v1", Status: "refused", Code: "USAGE", Remedy: "use [--json] plan PHASE FILE or profile INPUT.json; no execution command exists", Effects: []string{}}
	code := 2
	if len(args) > 0 && args[0] == "--json" {
		args = args[1:]
	}
	if len(args) == 2 && args[0] == "profile" {
		f, err := os.Open(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "PROFILE_INPUT_UNREADABLE")
			return 2
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
			fmt.Fprintln(os.Stderr, "PROFILE_INPUT_UNREADABLE")
			return 2
		}
		raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
		if err != nil {
			fmt.Fprintln(os.Stderr, "PROFILE_INPUT_UNREADABLE")
			return 2
		}
		input, err := seaweed.DecodeProfileInput(raw)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		bundle, err := seaweed.BuildProfile(input)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		if err = json.NewEncoder(os.Stdout).Encode(bundle); err != nil {
			return 1
		}
		return 0
	}
	if len(args) == 3 && args[0] == "plan" {
		f, err := os.Open(args[2])
		var raw []byte
		if err == nil {
			info, e := f.Stat()
			if e != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
				_ = f.Close()
				out.Code = "INPUT_UNREADABLE"
				out.Remedy = "supply a regular local plan <=1 MiB"
				_ = json.NewEncoder(os.Stdout).Encode(out)
				return 2
			}
			raw, err = io.ReadAll(io.LimitReader(f, (1<<20)+1))
			_ = f.Close()
		}
		if err != nil {
			out.Code = "INPUT_UNREADABLE"
			out.Remedy = "supply a readable bounded local plan"
		} else {
			out, err = seaweed.PlanPhase(string(raw), args[1])
			if err == nil {
				code = 0
			}
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, "OUTPUT_FAILED")
		return 1
	}
	return code
}
