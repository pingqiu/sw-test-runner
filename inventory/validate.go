package inventory

import (
	"encoding/base64"
	"fmt"
	"net/netip"
	"path"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

var slug = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
var user = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
var nic = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)
var sha = regexp.MustCompile(`^[0-9a-f]{64}$`)
var gid = regexp.MustCompile(`^[0-9a-f]{32}$`)
var bootID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var safePath = regexp.MustCompile(`^/[A-Za-z0-9_./-]+$`)
var resourceID = regexp.MustCompile(`^[A-Za-z0-9_./:@+=-]+$`)
var dns = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)

func cleanPath(s string) bool { return s != "/" && safePath.MatchString(s) && path.Clean(s) == s }
func unicast(s string) bool {
	a, e := netip.ParseAddr(s)
	if e != nil || a.Zone() != "" || a.String() != s {
		return false
	}
	a = a.Unmap()
	return !a.IsUnspecified() && !a.IsMulticast()
}
func address(s string) bool {
	if _, e := netip.ParseAddr(s); e == nil {
		return unicast(s)
	}
	return len(s) <= 253 && dns.MatchString(s) && strings.ContainsAny(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
}
func addressKey(s string) string {
	if a, e := netip.ParseAddr(s); e == nil {
		return a.Unmap().String()
	}
	return strings.ToLower(s)
}
func oneOf(s string, values ...string) bool {
	for _, v := range values {
		if s == v {
			return true
		}
	}
	return false
}
func has(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
func distinct(values []string, valid func(string) bool) bool {
	seen := map[string]bool{}
	for _, v := range values {
		if !valid(v) || seen[v] {
			return false
		}
		seen[v] = true
	}
	return true
}
func validKey(value string) bool {
	fields := strings.Fields(value)
	if len(fields) != 2 || strings.Join(fields, " ") != value {
		return false
	}
	key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(value))
	return err == nil && len(options) == 0 && len(rest) == 0 && fields[0] == key.Type() && fields[1] == base64.StdEncoding.EncodeToString(key.Marshal())
}
func validate(d *Document) error {
	if d.Schema != Version || !oneOf(d.State, "requested", "resolved") || !slug.MatchString(d.RunID) || !oneOf(d.Target, "azure", "aws", "gcp", "onprem") {
		return fmt.Errorf("invalid schema, state, run_id or target")
	}
	if !sha.MatchString(d.ArtifactManifestSHA256) {
		return fmt.Errorf("artifact manifest digest is required")
	}
	if len(d.ManagedResourceIDs) > 4096 || !distinct(d.ManagedResourceIDs, func(s string) bool { return len(s) <= 2048 && resourceID.MatchString(s) }) {
		return fmt.Errorf("invalid or duplicate managed resource IDs")
	}
	resources := map[string]bool{}
	for _, id := range d.ManagedResourceIDs {
		key := id
		if d.Target == "azure" {
			key = strings.ToLower(id)
		}
		if resources[key] {
			return fmt.Errorf("duplicate managed resource identity")
		}
		resources[key] = true
	}
	if d.Target == "onprem" && len(resources) != 0 || d.Target != "onprem" && len(resources) == 0 {
		return fmt.Errorf("managed resources must match target")
	}
	if len(d.Hosts) < 1 || len(d.Hosts) > 128 {
		return fmt.Errorf("hosts must contain1..128 entries")
	}
	ids, endpoints, dataAddresses := map[string]bool{}, map[string]bool{}, map[string]bool{}
	hostResources := map[string]bool{}
	for _, h := range d.Hosts {
		if !slug.MatchString(h.ID) || ids[h.ID] {
			return fmt.Errorf("invalid or duplicate host ID")
		}
		ids[h.ID] = true
		if d.Target == "onprem" {
			if h.ResourceID != "" {
				return fmt.Errorf("onprem host cannot claim a cloud resource")
			}
		} else {
			key := h.ResourceID
			if d.Target == "azure" {
				key = strings.ToLower(key)
			}
			if !resources[key] || hostResources[key] {
				return fmt.Errorf("host resource is foreign or duplicated")
			}
			hostResources[key] = true
		}
		if len(h.Roles) < 1 || len(h.Roles) > 5 || !distinct(h.Roles, func(s string) bool { return oneOf(s, "master", "filer", "volume", "s3", "client") }) {
			return fmt.Errorf("invalid or duplicate roles")
		}
		m := h.Management
		endpoint := addressKey(m.Address) + fmt.Sprint(":", m.Port)
		if !address(m.Address) || m.Port < 1 || m.Port > 65535 || !user.MatchString(m.User) || !slug.MatchString(m.CredentialRef) || m.HostKey == nil || endpoints[endpoint] {
			return fmt.Errorf("invalid or duplicate management endpoint/identity")
		}
		endpoints[endpoint] = true
		if *m.HostKey != "" && !validKey(*m.HostKey) || d.State == "resolved" && *m.HostKey == "" {
			return fmt.Errorf("resolved host needs a canonical public host-key pin")
		}
		if !unicast(h.Data.Address) || dataAddresses[addressKey(h.Data.Address)] || h.Data.NIC != "" && !nic.MatchString(h.Data.NIC) || h.Data.MTU != 0 && (h.Data.MTU < 576 || h.Data.MTU > 65535) {
			return fmt.Errorf("invalid or duplicate data network")
		}
		dataAddresses[addressKey(h.Data.Address)] = true
		if d.State == "resolved" && (h.Data.NIC == "" || h.Data.MTU == 0) {
			return fmt.Errorf("resolved data network is missing NIC/MTU")
		}
		if !cleanPath(h.RunRoot) {
			return fmt.Errorf("run_root must be canonical absolute Linux path, not root")
		}
		if len(h.Disks) > 32 || has(h.Roles, "volume") && len(h.Disks) == 0 {
			return fmt.Errorf("volume host needs declared disks")
		}
		disks, mounts, devices := map[string]bool{}, map[string]bool{}, map[string]bool{}
		for _, disk := range h.Disks {
			if !slug.MatchString(disk.ID) || disks[disk.ID] || !cleanPath(disk.Mount) || mounts[disk.Mount] || disk.FormatAllowed == nil {
				return fmt.Errorf("invalid/duplicate disk or missing format declaration")
			}
			disks[disk.ID] = true
			mounts[disk.Mount] = true
			if disk.DeviceID != "" && devices[disk.DeviceID] {
				return fmt.Errorf("duplicate disk device identity")
			}
			if disk.DeviceID != "" {
				devices[disk.DeviceID] = true
			}
			if disk.DeviceID != "" && (!strings.HasPrefix(disk.DeviceID, "/dev/disk/by-id/") || !cleanPath(disk.DeviceID)) || d.State == "resolved" && disk.DeviceID == "" {
				return fmt.Errorf("disk needs an exact by-id device, never a wildcard")
			}
		}
		required := d.Transport != "http" && (has(h.Roles, "volume") || has(h.Roles, "client"))
		if err := validateRDMA(h.RDMA, d.State, d.Transport, required); err != nil {
			return fmt.Errorf("host %s: %w", h.ID, err)
		}
	}
	if d.State == "requested" {
		if d.Observation != nil {
			return fmt.Errorf("requested data cannot claim an observation")
		}
		return nil
	}
	if d.Observation == nil || !sha.MatchString(d.Observation.RequestedSHA256) || len(d.Observation.Hosts) != len(d.Hosts) {
		return fmt.Errorf("resolved inventory needs exact observation coverage")
	}
	observed := map[string]bool{}
	for _, o := range d.Observation.Hosts {
		_, err := time.Parse(time.RFC3339Nano, o.CapturedAt)
		if !ids[o.HostID] || observed[o.HostID] || !bootID.MatchString(o.BootID) || err != nil || !sha.MatchString(o.EvidenceSHA256) {
			return fmt.Errorf("invalid, duplicate or foreign host observation")
		}
		observed[o.HostID] = true
	}
	return nil
}
func validateRDMA(r RDMA, state, transport string, required bool) error {
	if !required {
		if r.Status != "not_applicable" || r.Device != "" || r.Port != 0 || r.GIDIndex != nil || r.GID != "" || r.LinkLayer != "" || r.ActiveMTU != 0 || len(r.Capabilities) != 0 || r.FabricEvidenceSHA256 != "" {
			return fmt.Errorf("unused RDMA must explicitly be not_applicable")
		}
		return nil
	}
	want := "requested"
	if state == "resolved" {
		want = "observed"
	}
	if r.Status != want || len(r.Capabilities) < 1 || len(r.Capabilities) > 2 || !distinct(r.Capabilities, func(s string) bool { return oneOf(s, "rc", "dc") }) || !has(r.Capabilities, transport) {
		return fmt.Errorf("required transport lacks explicit RDMA capability/state")
	}
	if r.Device != "" && !nic.MatchString(r.Device) || r.Port < 0 || r.Port > 255 || r.GIDIndex != nil && (*r.GIDIndex < 0 || *r.GIDIndex > 255) || r.GID != "" && (!gid.MatchString(r.GID) || r.GID == strings.Repeat("0", 32)) || r.LinkLayer != "" && !oneOf(r.LinkLayer, "ethernet", "infiniband") || r.ActiveMTU != 0 && !oneOf(fmt.Sprint(r.ActiveMTU), "256", "512", "1024", "2048", "4096") {
		return fmt.Errorf("invalid RDMA device/GID/link/MTU")
	}
	if state == "requested" {
		if r.FabricEvidenceSHA256 != "" {
			return fmt.Errorf("requested RDMA cannot claim observed fabric evidence")
		}
		return nil
	}
	if r.Device == "" || r.Port == 0 || r.GIDIndex == nil || r.GID == "" || r.LinkLayer == "" || r.ActiveMTU == 0 || !sha.MatchString(r.FabricEvidenceSHA256) {
		return fmt.Errorf("observed RDMA device/GID/MTU/fabric evidence missing")
	}
	return nil
}
