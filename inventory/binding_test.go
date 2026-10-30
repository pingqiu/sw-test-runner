package inventory_test

import (
	"crypto/sha256"
	"fmt"
	"github.com/pingqiu/sw-test-runner/inventory"
	"testing"
)

func TestProducerConsumerPreserveRequestedEnvelope(t *testing.T) {
	req, observed := pair(t)
	raw := bytesOf(t, observed)
	got, err := inventory.Join(req, raw)
	if err != nil {
		t.Fatal(err)
	}
	produced, err := inventory.Resolve(req, *got)
	if err != nil {
		t.Fatal(err)
	}
	h := got.Hosts[0]
	o := got.Observation.Hosts[0]
	b := inventory.ConsumerBinding{RequestedSHA256: fmt.Sprintf("%x", sha256.Sum256(req)), ResolvedSHA256: fmt.Sprintf("%x", sha256.Sum256(produced)), RunID: got.RunID, Transport: got.Transport, ArtifactManifestSHA256: got.ArtifactManifestSHA256, Deadlines: got.Deadlines, Hosts: []inventory.HostBinding{{ID: h.ID, Address: h.Management.Address, SSHPort: h.Management.Port, User: h.Management.User, HostKey: *h.Management.HostKey, RunRoot: h.RunRoot, BootID: o.BootID}}}
	if _, err = inventory.Bind(req, produced, b); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"deadline", "port", "artifact", "boot", "request bytes", "resolved bytes", "host set"} {
		t.Run(mode, func(t *testing.T) {
			changed := b
			changed.Hosts = append([]inventory.HostBinding{}, b.Hosts...)
			switch mode {
			case "deadline":
				changed.Deadlines.Work = "2030-01-01T00:08:00Z"
			case "port":
				changed.Hosts[0].SSHPort++
			case "artifact":
				changed.ArtifactManifestSHA256 = string(make([]byte, 64))
			case "boot":
				changed.Hosts[0].BootID = "foreign"
			case "request bytes":
				changed.RequestedSHA256 = changed.ResolvedSHA256
			case "resolved bytes":
				changed.ResolvedSHA256 = changed.RequestedSHA256
			case "host set":
				changed.Hosts = nil
			}
			if _, err := inventory.Bind(req, produced, changed); err == nil {
				t.Fatal("foreign consumer accepted")
			}
		})
	}
	got.Deadlines.Work = "2030-01-01T00:08:00Z"
	if _, err := inventory.Resolve(req, *got); err == nil {
		t.Fatal("producer expanded/changed original envelope")
	}
}
