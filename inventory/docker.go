package inventory

import (
	"fmt"
	"path"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

func decimal(s string, zero bool) bool {
	n, err := strconv.ParseUint(s, 10, 64)
	return err == nil && (zero || n > 0) && strconv.FormatUint(n, 10) == s
}

// validateDocker validates declarations, never Docker daemon state or custody.
func validateDocker(d *Document) error {
	if d.Target != "docker" {
		if d.Docker != nil {
			return fmt.Errorf("docker profile is not valid on this target")
		}
		for _, h := range d.Hosts {
			if h.Container != nil || h.MetadataHostID != nil {
				return fmt.Errorf("container fields require docker target")
			}
		}
		return nil
	}
	p := d.Docker
	if p == nil {
		return fmt.Errorf("docker target requires its Gate 0a profile")
	}
	a := p.Parent
	m := a.Management
	if !slug.MatchString(a.ID) || !address(m.Address) || m.Port < 1 || m.Port > 65535 || !user.MatchString(m.User) || !slug.MatchString(m.CredentialRef) || m.HostKey == nil || !validKey(*m.HostKey) || !cleanPath(a.RunRoot) || path.Base(a.RunRoot) != d.RunID || !slug.MatchString(a.ExecutorRef) || !sha.MatchString(a.ExecutorSHA256) {
		return fmt.Errorf("docker parent identity/pin/private root/executor declaration is invalid")
	}
	if a.BootID != "" && !bootID.MatchString(a.BootID) || a.RootDevice != "" && !decimal(a.RootDevice, true) || a.RootInode != "" && !decimal(a.RootInode, false) || d.State == "resolved" && (a.BootID == "" || a.RootDevice == "" || a.RootInode == "") {
		return fmt.Errorf("docker parent generation/root identity missing or invalid")
	}
	if p.Network.Name != d.RunID+"-net" || p.Network.ID != "" && !sha.MatchString(p.Network.ID) || d.State == "resolved" && p.Network.ID == "" {
		return fmt.Errorf("docker needs a run-owned network name and full resolved ID")
	}
	mode, reason := "none", "software_profile_no_dc"
	if d.Transport == "rc" {
		mode, reason = "rxe", "rxe_no_dc"
	} else if d.Transport != "http" {
		return fmt.Errorf("Gate 0a DC is not_applicable; no transport fallback")
	}
	if p.RDMAMode != mode || len(p.UnavailableTransports) != 1 || p.UnavailableTransports[0] != (UnavailableTransport{Transport: "dc", Status: "not_applicable", Reason: reason}) {
		return fmt.Errorf("docker transport mode or explicit DC not_applicable declaration invalid")
	}
	if p.MetadataStore != "mysql" || !slug.MatchString(p.MetadataHostID) {
		return fmt.Errorf("Gate 0a requires one declared shared mysql metadata host")
	}
	counts := map[string]int{}
	resources := map[string]bool{"docker:" + a.ID + ":network:" + p.Network.Name: true}
	cids, netns, mounts := map[string]bool{}, map[string]bool{}, map[string]bool{}
	if a.RootDevice != "" && a.RootInode != "" {
		mounts[a.RootDevice+":"+a.RootInode] = true
	}
	for _, h := range d.Hosts {
		c := h.Container
		if h.ID == a.ID || len(h.Roles) != 1 || !oneOf(h.Roles[0], "master", "filer", "volume", "client", "metadata") || c == nil {
			return fmt.Errorf("docker needs distinct one-role container hosts")
		}
		role := h.Roles[0]
		counts[role]++
		if c.ParentID != a.ID || c.Name != d.RunID+"-"+h.ID || len(c.ImageDigest) != 71 || !strings.HasPrefix(c.ImageDigest, "sha256:") || !sha.MatchString(strings.TrimPrefix(c.ImageDigest, "sha256:")) || c.Privileged == nil || *c.Privileged {
			return fmt.Errorf("container placement/image/nonprivileged declaration invalid")
		}
		if addressKey(h.Management.Address) != addressKey(m.Address) || h.Management.Port == m.Port {
			return fmt.Errorf("container SSH needs a distinct published endpoint on its declared parent")
		}
		if c.ID != "" && (!sha.MatchString(c.ID) || cids[c.ID]) || c.NetNSInode != "" && (!decimal(c.NetNSInode, false) || netns[c.NetNSInode]) || d.State == "resolved" && (c.ID == "" || c.NetNSInode == "") {
			return fmt.Errorf("container needs unique full ID and netns identity")
		}
		if c.ID != "" {
			cids[c.ID] = true
		}
		if c.NetNSInode != "" {
			netns[c.NetNSInode] = true
		}
		resource := "docker:" + a.ID + ":container:" + c.Name
		if h.ResourceID != resource {
			return fmt.Errorf("container logical ownership key mismatch")
		}
		resources[resource] = true
		if role == "filer" && (h.MetadataHostID == nil || *h.MetadataHostID != p.MetadataHostID) || role != "filer" && h.MetadataHostID != nil || role == "metadata" && h.ID != p.MetadataHostID {
			return fmt.Errorf("filers must reference the same declared metadata host")
		}
		if role == "metadata" && len(h.Disks) == 0 || len(c.Mounts) != len(h.Disks) {
			return fmt.Errorf("container disk/bind-mount coverage missing")
		}
		disks := map[string]bool{}
		for _, disk := range h.Disks {
			if disk.DeviceID != "" || *disk.FormatAllowed {
				return fmt.Errorf("Docker bind mounts cannot declare block-device access or formatting")
			}
			disks[disk.ID] = true
		}
		covered := map[string]bool{}
		for _, b := range c.Mounts {
			if !disks[b.DiskID] || covered[b.DiskID] || b.SourcePath != path.Join(a.RunRoot, h.ID, b.DiskID) {
				return fmt.Errorf("bind source must be the exact private per-host disk directory")
			}
			covered[b.DiskID] = true
			if b.SourceDevice != "" && !decimal(b.SourceDevice, true) || b.SourceInode != "" && !decimal(b.SourceInode, false) || d.State == "resolved" && (b.SourceDevice == "" || b.SourceInode == "") {
				return fmt.Errorf("bind source stat identity missing or invalid")
			}
			if b.SourceDevice != "" && a.RootDevice != "" && b.SourceDevice != a.RootDevice {
				return fmt.Errorf("bind source must remain on the declared private-root filesystem")
			}
			if b.SourceDevice != "" && b.SourceInode != "" {
				identity := b.SourceDevice + ":" + b.SourceInode
				if mounts[identity] {
					return fmt.Errorf("bind sources cannot share a directory identity")
				}
				mounts[identity] = true
			}
		}
		if has(h.RDMA.Capabilities, "dc") {
			return fmt.Errorf("supported RC/DC capabilities must be disjoint from DC not_applicable")
		}
	}
	if counts["master"] != 3 || counts["filer"] != 2 || counts["volume"] < 4 || counts["client"] < 4 || counts["metadata"] != 1 {
		return fmt.Errorf("Gate 0a requires 3 masters, 2 filers, >=4 volumes, >=4 clients and 1 shared metadata container")
	}
	if len(resources) != len(d.ManagedResourceIDs) {
		return fmt.Errorf("docker ownership set must contain exactly containers and network")
	}
	for _, id := range d.ManagedResourceIDs {
		if !resources[id] {
			return fmt.Errorf("foreign docker ownership key")
		}
	}
	return nil
}

func pinned(want, got string) bool { return want == "" || want == got }
func joinDocker(want, got *Document) error {
	if want.Target != "docker" {
		return nil
	}
	a, b := *want.Docker, *got.Docker
	if !pinned(a.Parent.BootID, b.Parent.BootID) || !pinned(a.Parent.RootDevice, b.Parent.RootDevice) || !pinned(a.Parent.RootInode, b.Parent.RootInode) || !pinned(a.Network.ID, b.Network.ID) {
		return fmt.Errorf("resolution changed pinned parent/network generation")
	}
	a.Parent.BootID, b.Parent.BootID = "", ""
	a.Parent.RootDevice, b.Parent.RootDevice = "", ""
	a.Parent.RootInode, b.Parent.RootInode = "", ""
	a.Network.ID, b.Network.ID = "", ""
	if !reflect.DeepEqual(a, b) {
		return fmt.Errorf("resolution changed docker profile/parent authority declaration")
	}
	hosts := map[string]Host{}
	for _, h := range got.Hosts {
		hosts[h.ID] = h
	}
	for _, h := range want.Hosts {
		g := hosts[h.ID]
		x, y := *h.Container, *g.Container
		if !reflect.DeepEqual(h.MetadataHostID, g.MetadataHostID) || !pinned(x.ID, y.ID) || !pinned(x.NetNSInode, y.NetNSInode) {
			return fmt.Errorf("resolution changed container generation/metadata declaration")
		}
		x.ID, y.ID = "", ""
		x.NetNSInode, y.NetNSInode = "", ""
		x.Mounts = slices.Clone(x.Mounts)
		y.Mounts = slices.Clone(y.Mounts)
		byDisk := map[string]ContainerMount{}
		for _, m := range y.Mounts {
			byDisk[m.DiskID] = m
		}
		normalized := make([]ContainerMount, 0, len(x.Mounts))
		for i, m := range x.Mounts {
			n, ok := byDisk[m.DiskID]
			if !ok || !pinned(m.SourceDevice, n.SourceDevice) || !pinned(m.SourceInode, n.SourceInode) {
				return fmt.Errorf("resolution changed bind source generation")
			}
			m.SourceDevice, m.SourceInode = "", ""
			n.SourceDevice, n.SourceInode = "", ""
			x.Mounts[i] = m
			normalized = append(normalized, n)
		}
		// Preserve empty versus nonempty slices semantically; both declarations must
		// already have exact coverage, and no source may be added or removed.
		y.Mounts = normalized
		if !reflect.DeepEqual(x, y) {
			return fmt.Errorf("resolution changed container image/placement/storage scope")
		}
	}
	return nil
}
