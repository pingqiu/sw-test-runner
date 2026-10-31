package smoke

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestResourceObjectsAndWrapperSafeAzureArgs(t *testing.T) {
	for _, kind := range []string{"empty", "tagged-nic", "tagged-nsg", "foreign-vm", "foreign-disk", "foreign-ip", "foreign-free", "wrong-tag", "rg-free", "null", "missing-id", "missing-type", "old-string"} {
		t.Run(kind, func(t *testing.T) {
			c := clusterFixture(t)
			f := &fake{}
			if r := controller(f).Execute(context.Background(), "run", c); r.Code != "COMPLETE" {
				t.Fatal(r)
			}
			ctl := Controller{Command: func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
				joined := strings.Join(args, " ")
				if tool != "az" || strings.Contains(joined, "--query") || strings.ContainsAny(strings.Join(args, ""), "|&<>^%\"'\t\r\n") {
					t.Fatalf("unsafe argv: %q", args)
				}
				if p := os.Getenv("AZURE_ARGV_CONTROL_OUTPUT"); p != "" {
					b, _ := json.Marshal(args)
					out, e := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
					if e != nil {
						t.Fatal(e)
					}
					out.Write(append(b, '\n'))
					out.Close()
				}
				rg := strings.Contains(joined, "--resource-group")
				if kind == "null" {
					return []byte(`null`), nil
				}
				if kind == "old-string" {
					return []byte(`["/id"]`), nil
				}
				if kind == "missing-id" {
					return []byte(`[{"type":"Microsoft.Compute/disks"}]`), nil
				}
				if kind == "missing-type" {
					return []byte(`[{"id":"/id"}]`), nil
				}
				if rg {
					if kind == "rg-free" {
						return []byte(`[{"id":"/rg/vnet","type":"Microsoft.Network/virtualNetworks"}]`), nil
					}
					return []byte(`[]`), nil
				}
				types := map[string]string{"foreign-vm": "Microsoft.Compute/virtualMachines", "foreign-disk": "Microsoft.Compute/disks", "foreign-ip": "Microsoft.Network/publicIPAddresses", "tagged-nic": "Microsoft.Network/networkInterfaces", "tagged-nsg": "Microsoft.Network/networkSecurityGroups"}
				typ := types[kind]
				if typ == "" {
					typ = "Microsoft.Network/virtualNetworks"
				}
				tag := "foreign"
				if strings.HasPrefix(kind, "tagged-") {
					tag = "output"
				}
				return json.Marshal([]any{map[string]any{"id": "/subscription/" + kind, "type": typ, "tags": map[string]string{"seaweed_run": tag}}})
			}}
			r := ctl.Execute(context.Background(), "verify", c)
			wantEmpty := kind == "empty" || kind == "foreign-free" || kind == "wrong-tag"
			if (r.Code == "EMPTY") != wantEmpty {
				t.Fatalf("%s: %+v", kind, r)
			}
		})
	}
}
func TestAllAzureLifecycleArgvAreWrapperSafe(t *testing.T) {
	for _, cluster := range []bool{false, true} {
		c := fixture(t)
		if cluster {
			c = clusterFixture(t)
		}
		f := &fake{}
		ctl := controller(f)
		ctl.Command = func(ctx context.Context, dir, tool string, args ...string) ([]byte, error) {
			if tool == "az" {
				for _, a := range args {
					if strings.ContainsAny(a, "|&<>^%\"' \t\r\n") {
						t.Fatalf("unsafe Azure argv: %q", args)
					}
				}
				if strings.Contains(strings.Join(args, " "), "--query") {
					t.Fatal("query crossed native boundary")
				}
				if p := os.Getenv("AZURE_ARGV_CONTROL_OUTPUT"); p != "" {
					b, _ := json.Marshal(args)
					out, e := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
					if e != nil {
						t.Fatal(e)
					}
					_, e = out.Write(append(b, '\n'))
					out.Close()
					if e != nil {
						t.Fatal(e)
					}
				}
			}
			return f.command(ctx, dir, tool, args...)
		}
		for _, mode := range []string{"run", "verify", "destroy"} {
			r := ctl.Execute(context.Background(), mode, c)
			if r.Code != "COMPLETE" && r.Code != "EMPTY" {
				t.Fatal(r)
			}
		}
	}
}
