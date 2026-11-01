package smoke

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestCreateUnprefixedCreateStillSettles(t *testing.T) {
	message := strings.TrimPrefix(rereadForms(t)["run7b_ip_create_lost_put"], "Error: ")
	if !transportOnly([]byte(message)) {
		t.Fatal("control is not an accepted envelope")
	}
	shows, imports := 0, 0
	_, err := settleCreates(context.Background(), func(ctx context.Context, tool string, args ...string) ([]byte, error) {
		if tool == "az" {
			shows++
			return json.Marshal(map[string]any{"id": args[3], "type": "Microsoft.Network/publicIPAddresses", "tags": map[string]string{"seaweed_run": "output"}})
		}
		imports++
		return nil, nil
	}, []byte(message), "11111111-1111-1111-1111-111111111111", "dedicated-test", "output")
	if err != nil || shows != 1 || imports != 1 {
		t.Fatalf("accepted ID-bearing create bypassed settlement: shows=%d imports=%d err=%v", shows, imports, err)
	}
}

func TestCreateAmbiguousTagCannotImport(t *testing.T) {
	message := rereadForms(t)["run7b_ip_create_lost_put"]
	imports := 0
	_, err := settleCreates(context.Background(), func(ctx context.Context, tool string, args ...string) ([]byte, error) {
		if tool == "az" {
			id, _ := json.Marshal(args[3])
			return []byte(`{"id":` + string(id) + `,"type":"Microsoft.Network/publicIPAddresses","tags":{"seaweed_run":"foreign","seaweed_run":"output"}}`), nil
		}
		imports++
		return nil, nil
	}, []byte(message), "11111111-1111-1111-1111-111111111111", "dedicated-test", "output")
	if err == nil || imports != 0 {
		t.Fatalf("ambiguous foreign tag authorized import: imports=%d err=%v", imports, err)
	}
}

func TestCreateNullPlanCannotProveNoReplace(t *testing.T) {
	err := keepsImported(context.Background(), func(ctx context.Context, tool string, args ...string) ([]byte, error) { return []byte(`null`), nil }, []string{`module.run.azurerm_public_ip.node["vol2"]`})
	if err == nil {
		t.Fatal("null plan certified no deletion/replacement")
	}
}
