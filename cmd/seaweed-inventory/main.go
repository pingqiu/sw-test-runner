// Command seaweed-inventory validates data only; it cannot deploy or authorize.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/pingqiu/sw-test-runner/inventory"
)

func read(name string) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Size() > inventory.MaxBytes {
		return nil, fmt.Errorf("inventory input must be a bounded regular file")
	}
	return io.ReadAll(io.LimitReader(f, inventory.MaxBytes+1))
}

// execute emits one JSON result on both success and refusal. JSON is the default;
// --json is an explicit spelling for agent callers. Schema emits JSON Schema.
func execute(args []string, w io.Writer) int {
	if len(args) > 0 && args[0] == "--json" {
		args = args[1:]
	}
	emit := func(v any, exit int) int {
		if json.NewEncoder(w).Encode(v) != nil {
			return 1
		}
		return exit
	}
	refuse := func(code, remedy string) int {
		return emit(map[string]any{"schema": "seaweed.inventory-result/v1", "status": "refused", "code": code, "remedy": remedy, "execution_authorized": false}, 2)
	}
	if len(args) == 1 && args[0] == "schema" {
		if _, err := w.Write(inventory.Schema()); err != nil {
			return 1
		}
		return 0
	}
	if !((len(args) == 2 && args[0] == "validate") || (len(args) == 3 && args[0] == "join")) {
		return refuse("USAGE", "use seaweed-inventory [--json] validate FILE | join REQUESTED RESOLVED | schema; this command never executes inventory")
	}
	raw, err := read(args[1])
	if err != nil {
		return refuse("INPUT_UNREADABLE", "supply a readable bounded regular inventory file on the controller")
	}
	var d *inventory.Document
	if args[0] == "validate" {
		d, err = inventory.Decode(raw)
		if err != nil {
			return refuse("INVENTORY_INVALID", "correct input against seaweed.inventory/v1 using the schema command; validation grants no execution")
		}
	} else {
		observed, e := read(args[2])
		if e != nil {
			return refuse("INPUT_UNREADABLE", "supply a readable bounded regular resolved inventory file on the controller")
		}
		d, err = inventory.Join(raw, observed)
		if err != nil {
			return refuse("INVENTORY_JOIN_REFUSED", "validate both documents and bind the exact requested digest without changing host, resource, role, disk or transport scope")
		}
	}
	digest := sha256.Sum256(raw)
	return emit(map[string]any{"schema": "seaweed.inventory-result/v1", "status": "valid_data_only", "code": "OK", "remedy": "validation grants no execution; obtain reviewed S0b admission before any effect", "state": d.State, "input_sha256": hex.EncodeToString(digest[:]), "execution_authorized": false}, 0)
}
func main() { os.Exit(execute(os.Args[1:], os.Stdout)) }
