package inventory

import (
	"encoding/json"
	"os"
	"testing"
)

func TestObservedRootFilesystemDoesNotGrantRootRunOrFormat(t *testing.T) {
	raw, err := os.ReadFile("../examples/inventory/m01-m02.resolved.json")
	if err != nil {
		t.Fatal(err)
	}
	var d Document
	if err = json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	d.Hosts[1].Disks[0].Mount = "/"
	encode := func() []byte {
		b, e := json.Marshal(d)
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	if _, err = Decode(encode()); err != nil {
		t.Fatalf("actual root filesystem cannot be represented: %v", err)
	}
	yes := true
	d.Hosts[1].Disks[0].FormatAllowed = &yes
	if _, err = Decode(encode()); err == nil {
		t.Fatal("format of root filesystem admitted")
	}
	no := false
	d.Hosts[1].Disks[0].FormatAllowed = &no
	d.Hosts[1].RunRoot = "/"
	if _, err = Decode(encode()); err == nil {
		t.Fatal("root run directory admitted")
	}
}
