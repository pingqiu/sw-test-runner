package smoke

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClusterSubscriptionResidueAndTagFailuresNeverClean(t *testing.T) {
	for _, kind := range []string{"tagged-nic", "foreign-vm", "foreign-disk", "foreign-ip", "null", "timeout", "tag-error", "positive"} {
		t.Run(kind, func(t *testing.T) {
			c := clusterFixture(t)
			f := &fake{}
			ctl := controller(f)
			tags, scans := 0, 0
			ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
				if tool == "az" && args[0] == "tag" {
					tags++
					if kind == "tag-error" {
						return nil, errors.New("tag denied")
					}
					if !strings.Contains(strings.Join(args, " "), "seaweed_run=output") {
						t.Fatal("wrong run tag")
					}
					return []byte(`{}`), nil
				}
				if tool == "az" && strings.Contains(strings.Join(args, " "), "tags.seaweed_run") {
					scans++
					if !strings.Contains(strings.Join(args, " "), "Microsoft.Compute/disks") || !strings.Contains(strings.Join(args, " "), "Microsoft.Network/publicIPAddresses") {
						t.Fatal("incomplete subscription scan")
					}
					if scans > 1 {
						switch kind {
						case "null":
							return []byte(`null`), nil
						case "timeout":
							return nil, context.DeadlineExceeded
						case "tagged-nic", "foreign-vm", "foreign-disk", "foreign-ip":
							return json.Marshal([]string{"/subscriptions/foreign/" + kind})
						}
					}
				}
				return f.command(ctx, dir, tool, args...)
			}
			r := ctl.Execute(context.Background(), "run", c)
			if kind == "positive" {
				if r.Code != "COMPLETE" || tags != 10 || scans != 2 || !strings.Contains(r.RemainingSummary, "remaining: 0 (checked") {
					t.Fatalf("%+v tags=%d scans=%d", r, tags, scans)
				}
			} else if r.Code == "COMPLETE" || r.Code == "EMPTY" {
				t.Fatalf("residue/failure certified clean: %+v", r)
			}
			if kind == "tag-error" && r.Code != "DISK_TAG_FAILED" {
				t.Fatal(r)
			}
			if kind == "foreign-vm" && (len(r.Remaining) != 1 || !r.RemainingObserved) {
				t.Fatal(r)
			}
			if kind == "timeout" && r.RemainingObserved {
				t.Fatal("timeout reported observed")
			}
		})
	}
}

func TestClusterRunIDAndRecoveryConfigSavedBeforeApply(t *testing.T) {
	c := clusterFixture(t)
	f := &fake{fail: "apply"}
	ctl := controller(f)
	ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
		if tool == "terraform" && args[0] == "apply" {
			b, e := os.ReadFile(filepath.Join(c.Output, "recovery.json"))
			var saved Config
			if e != nil || StrictJSON(b, &saved) != nil || saved.ModuleSHA256 != c.ModuleSHA256 || saved.VariablesSHA256 != c.VariablesSHA256 {
				t.Fatal("missing recovery pins before apply")
			}
			b, e = os.ReadFile(filepath.Join(dir, "host.auto.tfvars.json"))
			var v map[string]json.RawMessage
			if e != nil || json.Unmarshal(b, &v) != nil || string(v["run_id"]) != `"output"` {
				t.Fatal("generated run identity missing")
			}
		}
		return f.command(ctx, dir, tool, args...)
	}
	r := ctl.Execute(context.Background(), "run", c)
	if !r.CreateUnresolved || r.Code == "COMPLETE" {
		t.Fatal(r)
	}
}

func TestCumulativeEstimateAndUnsettledActualCost(t *testing.T) {
	c := clusterFixture(t)
	c.PriorUSD = 10
	f := &fake{}
	if r := controller(f).Execute(context.Background(), "run", c); r.Code != "INPUT_REFUSED" || len(f.events) != 0 {
		t.Fatal(r)
	}
	c.PriorUSD = .04
	r := controller(f).Execute(context.Background(), "plan", c)
	if r.Code != "PLAN_ONLY" || r.EstimatedUSD != .12 || r.CumulativeUSD != .16 || r.ActualCostUSD != nil || !strings.Contains(r.RemainingSummary, "UNKNOWN") {
		t.Fatal(r)
	}
}
