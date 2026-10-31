package seaweed

import (
	"os"
	"testing"
)

func TestR10WithoutProspectiveAnchorStaysUnqualified(t *testing.T) {
	raw, err := os.ReadFile("testdata/r10-dc-load.json")
	if err != nil {
		t.Fatal(err)
	}
	c := SmokeConfig{RunID: "s0b-m01m02-20261031-r10-read-restart", Client: "m01", Transport: "dc", Keys: 4, Length: 1048576, Root: "/testops/s0b-m01m02-20261031-r10-read-restart/m01", RDMAAddress: "10.0.2.3:20980"}
	// R10 recorded no prospective P. Do not synthesize one from its outcomes,
	// schedule or ready observation, or manufacture unexecuted recovery jobs.
	if _, err = CheckReadTaskOutput(captured(raw), c, "load", "dc", ReadWindow{}); err == nil {
		t.Fatal("r10 without prospective P became qualified")
	}
}
