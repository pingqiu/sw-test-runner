package smoke

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"testing/synctest"
)

// Claude01 ruling 6ae76c16: the create re-read and the sweep take their type
// coverage from moduleTypes, and moduleTypes is exactly the module's types.
func TestModuleTypesAreTheModulesTypes(t *testing.T) {
	src, err := os.ReadFile("../terraform/azure/main.tf")
	if err != nil {
		t.Fatal(err)
	}
	var module, table []string
	for _, m := range regexp.MustCompile(`(?m)^resource "([a-z0-9_]+)"`).FindAllStringSubmatch(string(src), -1) {
		module = append(module, m[1])
	}
	arm, ranks := map[string]bool{}, map[int]bool{}
	for _, m := range moduleTypes {
		table = append(table, m.TF)
		if m.ARM != "" {
			if arm[strings.ToLower(m.ARM)] || ranks[m.Sweep] {
				t.Fatalf("duplicate ARM type or sweep rank: %+v", m)
			}
			arm[strings.ToLower(m.ARM)], ranks[m.Sweep] = true, true
		}
	}
	sort.Strings(module)
	sort.Strings(table)
	if strings.Join(module, " ") != strings.Join(table, " ") {
		t.Fatalf("module types %v != moduleTypes %v", module, table)
	}
	if r, _ := sweepRank("microsoft.compute/VIRTUALMACHINES"); r != 0 {
		t.Fatal("a VM must sweep first")
	}
}

func vmLostPut(t *testing.T) string {
	body, err := os.ReadFile("testdata/arm-public-ip-transport.json")
	if err != nil {
		t.Fatal(err)
	}
	var forms map[string]string
	if err = json.Unmarshal(body, &forms); err != nil {
		t.Fatal(err)
	}
	return forms["run7e_vm_create_lost_put"]
}

func TestLostVMCreateIsATransportForm(t *testing.T) {
	lost := vmLostPut(t)
	for _, tc := range []struct {
		name, raw string
		want      bool
	}{
		{"run7e", lost, true},
		{"no-prefix", strings.TrimPrefix(lost, "Error: "), true},
		{"address-of-another-type", strings.ReplaceAll(lost, "azurerm_linux_virtual_machine", "azurerm_network_interface"), false},
		{"type-not-in-module", strings.ReplaceAll(lost, "Microsoft.Compute/virtualMachines", "Microsoft.Compute/availabilitySets"), false},
		{"other-failure", strings.Replace(lost, "HTTP response was nil; connection may have been reset", "unexpected status 409", 1), false},
		{"trailing-error", lost + "\n\nError: quota exceeded", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if transportOnly([]byte(tc.raw)) != tc.want {
				t.Fatalf("transportOnly = %t", !tc.want)
			}
		})
	}
}

// Run 7e: client2's VM create PUT lost its response but Azure created the VM
// outside state. It is now re-read by exact ID like run 7b's Public IP.
func TestLostVMCreateRereadsAndImports(t *testing.T) {
	lost := vmLostPut(t)
	id := "/subscriptions/11111111-1111-1111-1111-111111111111/resourceGroups/dedicated-test/providers/Microsoft.Compute/virtualMachines/output-client2"
	address := `module.run.azurerm_linux_virtual_machine.node["client2"]`
	object := func(typ, tag string) func() ([]byte, error) {
		return func() ([]byte, error) {
			return json.Marshal(map[string]any{"id": id, "type": typ, "tags": map[string]string{"seaweed_run": tag}})
		}
	}
	vm := "Microsoft.Compute/virtualMachines"
	plan, _ := json.Marshal(map[string]any{"resource_changes": []any{map[string]any{"address": address, "change": map[string]any{"actions": []string{"no-op"}}}}})
	for _, tc := range []struct {
		name             string
		show             func() ([]byte, error)
		want             string
		applies, imports int
	}{
		{"present-is-imported", object(vm, "output"), "CREATE_UNRESOLVED", 2, 1},
		{"absent-retries", func() ([]byte, error) { return absentShow(id), errors.New("az: exit status 3") }, "CREATE_UNRESOLVED", 2, 0},
		{"foreign-tag-refuses", object(vm, "someone-else"), "APPLY_FAILED_OR_UNKNOWN", 1, 0},
		{"wrong-type-refuses", object("Microsoft.Network/networkInterfaces", "output"), "APPLY_FAILED_OR_UNKNOWN", 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := fixture(t)
				f := &fake{}
				ctl := controller(f)
				applies, imports := 0, []string{}
				ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
					switch {
					case tool == "az" && args[0] == "resource" && args[1] == "show":
						if args[3] != id {
							t.Fatal("re-read a different ID", args)
						}
						return tc.show()
					case tool == "terraform" && args[0] == "import":
						imports = append(imports, strings.Join(args[len(args)-2:], " "))
						return nil, nil
					case tool == "terraform" && args[0] == "show":
						return plan, nil
					case tool == "terraform" && args[0] == "apply":
						applies++
						if applies == 1 {
							return []byte(lost), errors.New("terraform: exit status 1")
						}
					}
					return f.command(ctx, dir, tool, args...)
				}
				r := ctl.Execute(context.Background(), "run", c)
				if r.Code != tc.want || applies != tc.applies || len(imports) != tc.imports {
					t.Fatalf("applies=%d imports=%v result=%+v", applies, imports, r)
				}
				if tc.imports == 1 && imports[0] != address+" "+id {
					t.Fatalf("imported %q", imports[0])
				}
			})
		})
	}
}
