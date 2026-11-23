package testrunner

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// These are controller-order controls with a recording host model, not nft or
// lab qualification. The host implementation remains the sealed nftguard.
func TestGO2HealFallbackAfterAbortAndNormalHeal(t *testing.T) {
	raw, err := os.ReadFile("scenarios/go2/shared/heal.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, abort := range []bool{true, false} {
		t.Run(fmt.Sprint(abort), func(t *testing.T) {
			s, err := Parse(append(raw, []byte("\ntopology:\n  nodes:\n    m01: {host: 192.0.2.1}\n    m02: {host: 192.0.2.2}\n")...))
			if err != nil {
				t.Fatal(err)
			}
			s.AlwaysTimeout = Duration{Duration: 2 * time.Second}
			s.Env = map[string]string{"m01_packet": "/owned/m01/pk", "m02_packet": "/owned/m02/pk", "release_deadline": "1234567890"}
			s.Phases = append([]Phase{{Name: "ordinary", Actions: []Action{{Action: "block"}, {Action: "takeover"}, {Action: "normal-heal"}, {Action: "noresume"}}}}, s.Phases...)
			present := map[string]bool{}
			deletes, checks, normal := 0, 0, 0
			r := NewRegistry()
			r.RegisterFunc("block", TierCore, func(context.Context, *ActionContext, Action) (map[string]string, error) {
				present["m01"], present["m02"] = true, true
				return nil, nil
			})
			r.RegisterFunc("takeover", TierCore, func(context.Context, *ActionContext, Action) (map[string]string, error) {
				if abort {
					return nil, fmt.Errorf("abort between block and heal")
				}
				return nil, nil
			})
			r.RegisterFunc("normal-heal", TierCore, func(context.Context, *ActionContext, Action) (map[string]string, error) {
				normal++
				present["m01"], present["m02"] = false, false
				return nil, nil
			})
			r.RegisterFunc("noresume", TierCore, func(context.Context, *ActionContext, Action) (map[string]string, error) {
				if normal != 1 {
					t.Fatal("noresume before normal heal")
				}
				return nil, nil
			})
			r.RegisterFunc("exec", TierCore, func(ctx context.Context, _ *ActionContext, a Action) (map[string]string, error) {
				if ctx.Err() != nil {
					t.Fatal(ctx.Err())
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("unbounded cleanup")
				}
				cmd := a.Params["cmd"]
				if a.Timeout != "35s" || !strings.Contains(cmd, "/host.sh' bounded '1234567890' bash '") || strings.Contains(cmd, "d-heal") {
					t.Fatalf("bad fallback: %+v", a)
				}
				if strings.HasSuffix(cmd, " heal") {
					if present[a.Node] {
						deletes++
						present[a.Node] = false
					}
					return nil, nil
				}
				if !strings.HasSuffix(cmd, " absent") {
					t.Fatalf("unexpected call %s", cmd)
				}
				checks++
				if present[a.Node] {
					return nil, fmt.Errorf("table still present")
				}
				return nil, nil
			})
			result := NewEngine(r, nil).Run(t.Context(), s, &ActionContext{Vars: map[string]string{}})
			want := StatusPass
			wantDeletes := 0
			if abort {
				want = StatusFail
				wantDeletes = 2
			}
			if result.Status != want || checks != 2 || deletes != wantDeletes || present["m01"] || present["m02"] {
				t.Fatalf("status=%s checks=%d deletes=%d tables=%v", result.Status, checks, deletes, present)
			}
		})
	}
}
