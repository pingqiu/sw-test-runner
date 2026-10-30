package inventory

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// Resolve is the producer boundary. The caller supplies observed facts; this
// function neither discovers hosts nor attests facts. Join refuses any change
// to the original request, and binds the exact bytes, including whitespace.
func Resolve(requested []byte, observed Document) ([]byte, error) {
	observed.State = "resolved"
	if observed.Observation == nil {
		return nil, fmt.Errorf("missing per-host observations")
	}
	observation := *observed.Observation
	observation.RequestedSHA256 = digest(requested)
	observed.Observation = &observation
	raw, err := json.Marshal(observed)
	if err != nil {
		return nil, err
	}
	if _, err = Join(requested, raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// ConsumerBinding is DATA supplied from an independently admitted runtime, not
// an authorization token. Bind alone grants no execution or trust in a receipt.
type ConsumerBinding struct {
	RequestedSHA256, ResolvedSHA256          string
	RunID, Transport, ArtifactManifestSHA256 string
	Deadlines                                Deadlines
	Hosts                                    []HostBinding
}
type HostBinding struct {
	ID, Address, User, HostKey, RunRoot, BootID string
	SSHPort                                     int
}

func digest(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

// Bind joins exact producer bytes to the consumer's complete admitted host set,
// original deadlines and artifact digest. There is no HTTP fallback mapping.
func Bind(requested, resolved []byte, b ConsumerBinding) (*Document, error) {
	if digest(requested) != b.RequestedSHA256 || digest(resolved) != b.ResolvedSHA256 {
		return nil, fmt.Errorf("producer input pin mismatch")
	}
	d, err := Join(requested, resolved)
	if err != nil {
		return nil, err
	}
	if d.RunID != b.RunID || d.Transport != b.Transport || d.ArtifactManifestSHA256 != b.ArtifactManifestSHA256 || d.Deadlines != b.Deadlines || len(d.Hosts) != len(b.Hosts) {
		return nil, fmt.Errorf("consumer envelope mismatch")
	}
	boots := map[string]string{}
	for _, o := range d.Observation.Hosts {
		boots[o.HostID] = o.BootID
	}
	hosts := map[string]HostBinding{}
	for _, h := range b.Hosts {
		if _, ok := hosts[h.ID]; ok {
			return nil, fmt.Errorf("duplicate consumer host")
		}
		hosts[h.ID] = h
	}
	for _, h := range d.Hosts {
		w, ok := hosts[h.ID]
		if !ok || w.Address != h.Management.Address || w.SSHPort != h.Management.Port || w.User != h.Management.User || w.HostKey != *h.Management.HostKey || w.RunRoot != h.RunRoot || w.BootID != boots[h.ID] {
			return nil, fmt.Errorf("consumer host generation mismatch")
		}
	}
	return d, nil
}
