package inventory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
)

// Join binds observed facts to exact requested bytes. It prevents observations
// from changing declared hosts, resources, roots, roles or formatting scope.
// Neither this join nor a valid digest constitutes trusted observation or GO.
func Join(requested, resolved []byte) (*Document, error) {
	want, err := Decode(requested)
	if err != nil {
		return nil, fmt.Errorf("requested: %w", err)
	}
	got, err := Decode(resolved)
	if err != nil {
		return nil, fmt.Errorf("resolved: %w", err)
	}
	if want.State != "requested" || got.State != "resolved" {
		return nil, fmt.Errorf("join requires requested then resolved state")
	}
	digest := sha256.Sum256(requested)
	if got.Observation.RequestedSHA256 != hex.EncodeToString(digest[:]) {
		return nil, fmt.Errorf("resolution does not bind exact requested bytes")
	}
	if want.Deadlines != got.Deadlines || want.RunID != got.RunID || want.Target != got.Target || want.Transport != got.Transport || want.ArtifactManifestSHA256 != got.ArtifactManifestSHA256 || !sameSet(want.ManagedResourceIDs, got.ManagedResourceIDs) || len(want.Hosts) != len(got.Hosts) {
		return nil, fmt.Errorf("resolution changed request envelope or resource scope")
	}
	hosts := map[string]Host{}
	for _, h := range got.Hosts {
		hosts[h.ID] = h
	}
	for _, w := range want.Hosts {
		h, ok := hosts[w.ID]
		if !ok {
			return nil, fmt.Errorf("resolution changed host set")
		}
		if !sameServices(w.Services, h.Services) || w.ResourceID != h.ResourceID || !sameSet(w.Roles, h.Roles) || w.RunRoot != h.RunRoot || w.Management.Address != h.Management.Address || w.Management.Port != h.Management.Port || w.Management.User != h.Management.User || w.Management.CredentialRef != h.Management.CredentialRef || *w.Management.HostKey != "" && *w.Management.HostKey != *h.Management.HostKey {
			return nil, fmt.Errorf("resolution changed host identity/roles/root")
		}
		if w.Data.Address != h.Data.Address || w.Data.NIC != "" && w.Data.NIC != h.Data.NIC || w.Data.MTU != 0 && w.Data.MTU != h.Data.MTU {
			return nil, fmt.Errorf("resolution changed requested data network")
		}
		if len(w.Disks) != len(h.Disks) {
			return nil, fmt.Errorf("resolution changed disk set")
		}
		disks := map[string]Disk{}
		for _, disk := range h.Disks {
			disks[disk.ID] = disk
		}
		for _, disk := range w.Disks {
			d, ok := disks[disk.ID]
			if !ok || disk.Mount != d.Mount || *disk.FormatAllowed != *d.FormatAllowed || disk.DeviceID != "" && disk.DeviceID != d.DeviceID {
				return nil, fmt.Errorf("resolution changed disk or format permission")
			}
		}
		a, b := w.RDMA, h.RDMA
		if a.Device != "" && a.Device != b.Device || a.Port != 0 && a.Port != b.Port || a.GIDIndex != nil && (b.GIDIndex == nil || *a.GIDIndex != *b.GIDIndex) || a.GID != "" && a.GID != b.GID || a.LinkLayer != "" && a.LinkLayer != b.LinkLayer || a.ActiveMTU != 0 && a.ActiveMTU != b.ActiveMTU {
			return nil, fmt.Errorf("resolution changed requested RDMA selection")
		}
		for _, capability := range a.Capabilities {
			if !has(b.Capabilities, capability) {
				return nil, fmt.Errorf("resolution lost requested RDMA capability")
			}
		}
	}
	return got, nil
}
func sameSet(a, b []string) bool {
	a = slices.Clone(a)
	b = slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
