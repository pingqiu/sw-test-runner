// Command seaweed-inventory validates data only; it cannot deploy or authorize.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/pingqiu/sw-test-runner/inventory"
	"io"
	"os"
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
func run(args []string) error {
	if len(args) == 1 && args[0] == "schema" {
		_, err := os.Stdout.Write(inventory.Schema())
		return err
	}
	if len(args) != 2 && len(args) != 3 {
		return fmt.Errorf("usage: seaweed-inventory validate FILE | join REQUESTED RESOLVED | schema")
	}
	raw, err := read(args[1])
	if err != nil {
		return err
	}
	var d *inventory.Document
	switch {
	case args[0] == "validate" && len(args) == 2:
		d, err = inventory.Decode(raw)
	case args[0] == "join" && len(args) == 3:
		observed, e := read(args[2])
		if e != nil {
			return e
		}
		d, err = inventory.Join(raw, observed)
	default:
		return fmt.Errorf("unknown operation; this command never executes inventory")
	}
	if err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"status": "valid_data_only", "state": d.State, "input_sha256": hex.EncodeToString(digest[:]), "execution_authorized": false})
}
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
