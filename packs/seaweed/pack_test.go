package seaweed

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	tr "github.com/pingqiu/sw-test-runner"
)

type forbiddenNode struct{ calls int }

func (n *forbiddenNode) Run(context.Context, string) (string, string, int, error) {
	n.calls++
	panic("S0b unavailable: remote execution forbidden")
}
func (n *forbiddenNode) RunRoot(context.Context, string) (string, string, int, error) {
	n.calls++
	panic("S0b unavailable: root execution forbidden")
}
func (n *forbiddenNode) Upload(string, string) error {
	n.calls++
	panic("S0b unavailable: upload forbidden")
}
func (n *forbiddenNode) Close() { n.calls++; panic("S0b unavailable: remote access forbidden") }

func TestEveryPhaseRefusesBeforeRemoteAccess(t *testing.T) {
	raw, err := os.ReadFile("../../examples/inventory/http.requested.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"seaweed_preflight", "seaweed_deploy", "seaweed_workload", "seaweed_collect", "seaweed_cleanup"} {
		for _, absent := range []bool{false, true} {
			t.Run(phase+map[bool]string{false: "/permissive", true: "/nil-context"}[absent], func(t *testing.T) {
				r := tr.NewRegistry()
				r.AllowMutating = true
				RegisterPack(r)
				h, err := r.Get(phase)
				if err != nil {
					t.Fatal(err)
				}
				n := &forbiddenNode{}
				a := &tr.ActionContext{Nodes: map[string]tr.NodeRunner{"node": n}, Vars: map[string]string{"allow_mutating": "true", "s0b_admitted": "true", "go": "true", "transport": "http"}}
				if absent {
					a = nil
				}
				out, err := h.Execute(context.Background(), a, tr.Action{Action: phase, Params: map[string]string{"admitted": "true", "inventory_json": string(raw)}})
				if !errors.Is(err, ErrS0bUnavailable) {
					t.Fatalf("phase appeared authorized: out=%v error=%v", out, err)
				}
				checkResult(t, out, "S0B_UNAVAILABLE")
				if n.calls != 0 {
					t.Fatalf("remote calls=%d", n.calls)
				}
			})
		}
	}
}
func TestInventoryDataOnly(t *testing.T) {
	b, err := os.ReadFile("../../examples/inventory/http.requested.json")
	if err != nil {
		t.Fatal(err)
	}
	r := tr.NewRegistry()
	RegisterPack(r)
	h, err := r.Get("seaweed_inventory_validate")
	if err != nil {
		t.Fatal(err)
	}
	n := &forbiddenNode{}
	out, err := h.Execute(context.Background(), &tr.ActionContext{Nodes: map[string]tr.NodeRunner{"node": n}}, tr.Action{Params: map[string]string{"inventory_json": string(b)}})
	if err != nil || out["execution_authorized"] != "false" || n.calls != 0 {
		t.Fatalf("out=%v err=%v calls=%d", out, err, n.calls)
	}
}
func TestInventoryRefusal(t *testing.T) {
	for _, raw := range []string{"", `{}`, `{"schema":"seaweed.inventory/v1","transport":"rc-or-http"}`} {
		r := tr.NewRegistry()
		RegisterPack(r)
		h, _ := r.Get("seaweed_inventory_validate")
		out, err := h.Execute(context.Background(), nil, tr.Action{Params: map[string]string{"inventory_json": raw}})
		if err == nil {
			t.Fatalf("accepted invalid inventory: %q out=%v", raw, out)
		}
		checkResult(t, out, "INVENTORY_INVALID")
	}
}

func checkResult(t *testing.T, out map[string]string, code string) {
	t.Helper()
	var r struct {
		Schema              string   `json:"schema"`
		Code                string   `json:"code"`
		Remedy              string   `json:"remedy"`
		ExecutionAuthorized bool     `json:"execution_authorized"`
		Effects             []string `json:"effects"`
	}
	if json.Unmarshal([]byte(out["value"]), &r) != nil || r.Schema != "seaweed.pack-result/v1" || r.Code != code || r.Remedy == "" || r.ExecutionAuthorized || len(r.Effects) != 0 {
		t.Fatalf("invalid machine result: %v", out)
	}
}
func TestPlanEveryPhaseWithoutAdmissionOrRemoteAccess(t *testing.T) {
	raw, err := os.ReadFile("../../examples/inventory/http.requested.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"seaweed_preflight", "seaweed_deploy", "seaweed_workload", "seaweed_collect", "seaweed_cleanup"} {
		t.Run(phase, func(t *testing.T) {
			r := tr.NewRegistry()
			RegisterPack(r) // No mutating permission: plan is read-only.
			h, err := r.Get("seaweed_plan")
			if err != nil {
				t.Fatal(err)
			}
			n := &forbiddenNode{}
			out, err := h.Execute(context.Background(), &tr.ActionContext{Nodes: map[string]tr.NodeRunner{"n": n}}, tr.Action{Params: map[string]string{"for_action": phase, "inventory_json": string(raw)}})
			if err != nil || n.calls != 0 {
				t.Fatalf("plan err=%v calls=%d", err, n.calls)
			}
			checkResult(t, out, "S0B_UNAVAILABLE")
		})
	}
}
func TestPlanRejectsUnsupportedAction(t *testing.T) {
	r := tr.NewRegistry()
	RegisterPack(r)
	h, err := r.Get("seaweed_plan")
	if err != nil {
		t.Fatal(err)
	}
	out, err := h.Execute(context.Background(), nil, tr.Action{Params: map[string]string{"for_action": "exec"}})
	if err == nil {
		t.Fatal("unsupported plan accepted")
	}
	checkResult(t, out, "ACTION_UNSUPPORTED")
}

func TestEnginePreservesJSONOnRuntimeRefusal(t *testing.T) {
	r := tr.NewRegistry()
	r.AllowMutating = true
	RegisterPack(r)
	s := &tr.Scenario{Name: "refusal", Phases: []tr.Phase{{Name: "deploy", Actions: []tr.Action{{Action: "seaweed_deploy"}}}}}
	n := &forbiddenNode{}
	result := tr.NewEngine(r, nil).Run(context.Background(), s, &tr.ActionContext{Scenario: s, Vars: map[string]string{}, Nodes: map[string]tr.NodeRunner{"n": n}})
	if result.Status != tr.StatusFail || len(result.Phases) != 1 || len(result.Phases[0].Actions) != 1 || n.calls != 0 {
		t.Fatalf("result=%+v calls=%d", result, n.calls)
	}
	action := result.Phases[0].Actions[0]
	if action.Error == "" {
		t.Fatal("runtime refusal lost")
	}
	checkResult(t, map[string]string{"value": action.Output}, "S0B_UNAVAILABLE")
}
