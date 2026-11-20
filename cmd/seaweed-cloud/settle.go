package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/pingqiu/sw-test-runner/provision/smoke"
)

const settleEventLimit = 1000

type settlement struct {
	Schema        string   `json:"schema"`
	Code          string   `json:"code"`
	ResourceGroup string   `json:"resource_group"`
	VM            string   `json:"vm"`
	Resources     []string `json:"resources"`
	Reason        string   `json:"reason"`
}

// settle observes only. It never clears CREATE_UNRESOLVED or releases a claim.
func runSettle(args []string, w io.Writer) int {
	r := settlement{Schema: "seaweed.cloud-settlement/v1", Code: "UNKNOWN", Resources: []string{}}
	fs := flag.NewFlagSet("settle", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&r.ResourceGroup, "resource-group", "", "run's resource group")
	fs.StringVar(&r.VM, "vm", "", "run's VM name")
	sub := fs.String("subscription", "", "subscription UUID; defaults to the Azure CLI context")
	name := regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,89}$`)
	uuid := regexp.MustCompile(`^[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$`)
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || !name.MatchString(r.ResourceGroup) || !name.MatchString(r.VM) || (*sub != "" && !uuid.MatchString(*sub)) {
		r.Reason = "use settle --resource-group RG --vm VM [--subscription UUID]"
	} else if runtime.GOOS != "linux" {
		r.Reason = "settle requires the Linux controller"
	} else {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		queries := [][]string{
			{"monitor", "activity-log", "list", "--offset", "1h", "--max-events", "1000"},
			{"resource", "list"}, {"disk", "list"},
		}
		var raw [3][]byte
		failed := false
		for i, argv := range queries {
			argv = append(argv, "--resource-group", r.ResourceGroup, "--output", "json", "--only-show-errors")
			if *sub != "" {
				argv = append(argv, "--subscription", *sub)
			}
			var err error
			raw[i], err = smoke.Native(ctx, "", "az", argv...)
			failed = failed || err != nil
		}
		if failed {
			r.Reason = "Azure query failed or timed out; no settlement inferred"
		} else {
			r.Code, r.Resources, r.Reason = classifySettlement(raw, r.ResourceGroup, r.VM, *sub)
		}
	}
	if json.NewEncoder(w).Encode(r) != nil {
		return 2
	}
	switch r.Code {
	case "SETTLED-CLEAN":
		return 0
	case "SETTLED-EXISTS":
		return 1
	default:
		return 2
	}
}

func settleScope(id, rg, sub string) bool {
	p := strings.Split(strings.Trim(id, "/"), "/")
	return len(p) >= 4 && strings.EqualFold(p[0], "subscriptions") && p[1] != "" && strings.EqualFold(p[2], "resourceGroups") && strings.EqualFold(p[3], rg) && (sub == "" || strings.EqualFold(p[1], sub))
}

type settleActivity struct {
	Category, OperationName, Status         struct{ Value string }
	OperationID, ResourceID, EventTimestamp string
}

func classifySettlement(raw [3][]byte, rg, vm, sub string) (string, []string, string) {
	unknown := func(reason string) (string, []string, string) { return "UNKNOWN", []string{}, reason }
	ids := map[string]string{}
	for _, data := range raw[1:] {
		var resources []struct{ ID string }
		if json.Unmarshal(data, &resources) != nil || resources == nil {
			return unknown("invalid resource or disk list")
		}
		for _, resource := range resources {
			if !settleScope(resource.ID, rg, sub) {
				return unknown("resource or disk lacks an in-scope ID")
			}
			ids[strings.ToLower(resource.ID)] = resource.ID
		}
	}
	var events []settleActivity
	if json.Unmarshal(raw[0], &events) != nil || events == nil || len(events) >= settleEventLimit {
		return unknown("invalid or possibly truncated activity log")
	}
	type observed struct {
		event       settleActivity
		at          time.Time
		conflicting bool
	}
	latest := map[string]observed{}
	var deleted, written time.Time
	for _, event := range events {
		if !strings.EqualFold(event.Category.Value, "Administrative") {
			if event.Category.Value == "" {
				return unknown("activity category missing")
			}
			continue
		}
		op := strings.ToLower(event.OperationName.Value)
		if strings.HasSuffix(op, "/read") {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, event.EventTimestamp)
		if err != nil || event.OperationID == "" || event.Status.Value == "" || op == "" || !settleScope(event.ResourceID, rg, sub) {
			return unknown("incomplete mutation evidence")
		}
		key := strings.ToLower(event.OperationID + "|" + event.ResourceID + "|" + op)
		if old, ok := latest[key]; !ok || at.After(old.at) {
			latest[key] = observed{event: event, at: at}
		} else if old.at.Equal(at) && !strings.EqualFold(old.event.Status.Value, event.Status.Value) {
			old.conflicting = true
			latest[key] = old
		}
		if strings.HasSuffix(strings.ToLower(event.ResourceID), "/providers/microsoft.compute/virtualmachines/"+strings.ToLower(vm)) {
			if op == "microsoft.compute/virtualmachines/delete" && strings.EqualFold(event.Status.Value, "Succeeded") && at.After(deleted) {
				deleted = at
			}
			if op == "microsoft.compute/virtualmachines/write" && at.After(written) {
				written = at
			}
		}
	}
	for _, operation := range latest {
		if operation.conflicting {
			return unknown("conflicting latest activity statuses")
		}
		switch strings.ToLower(operation.event.Status.Value) {
		case "succeeded", "failed", "canceled", "cancelled":
		default:
			return unknown("mutation has no terminal activity status")
		}
	}
	remaining := make([]string, 0, len(ids))
	for _, id := range ids {
		remaining = append(remaining, id)
	}
	sort.Strings(remaining)
	if len(remaining) != 0 {
		return "SETTLED-EXISTS", remaining, "resources observed; no listed mutation is pending"
	}
	if deleted.IsZero() || !written.Before(deleted) {
		return unknown("empty lists without a successful VM delete after every observed VM write")
	}
	return "SETTLED-CLEAN", remaining, "VM delete terminal; resource and disk lists empty (one-hour observation only)"
}
