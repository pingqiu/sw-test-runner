// Package inventory validates deployment data. A valid inventory is never GO,
// host trust, a lease, or permission to execute or format anything.
package inventory

const Version = "seaweed.inventory/v1"

// Document is requested infrastructure or a separately observed resolution.
type Document struct {
	Schema                 string       `json:"schema"`
	State                  string       `json:"state"`
	RunID                  string       `json:"run_id"`
	Target                 string       `json:"target"`
	Transport              string       `json:"transport"`
	ArtifactManifestSHA256 string       `json:"artifact_manifest_sha256"`
	ManagedResourceIDs     []string     `json:"managed_resource_ids"`
	Hosts                  []Host       `json:"hosts"`
	Deadlines              Deadlines    `json:"deadlines"`
	Observation            *Observation `json:"observation,omitempty"`
}

// Host binds one stable node to declared roles, networks and disks.
type Host struct {
	ResourceID string      `json:"resource_id"`
	ID         string      `json:"id"`
	Roles      []string    `json:"roles"`
	Management Management  `json:"management"`
	Data       DataNetwork `json:"data"`
	RunRoot    string      `json:"run_root"`
	Disks      []Disk      `json:"disks"`
	Services   []Service   `json:"services"`
	RDMA       RDMA        `json:"rdma"`
}

// Management names a controller-held credential and an out-of-band host key.
type Management struct {
	Address       string `json:"address"`
	Port          int    `json:"port"`
	User          string `json:"user"`
	CredentialRef string `json:"credential_ref"`
	// Requested hosts explicitly use an empty string until an out-of-band pin is available.
	HostKey *string `json:"host_key"`
}

// DataNetwork distinguishes the data NIC MTU from active RDMA MTU.
type DataNetwork struct {
	Address string `json:"address"`
	NIC     string `json:"nic"`
	MTU     int    `json:"mtu"`
}

// Disk declares a logical device and formatting scope; it grants no permission.
type Disk struct {
	ID       string `json:"id"`
	DeviceID string `json:"device_id"`
	Mount    string `json:"mount"`
	// This declaration must still be authorized by S0b; observations cannot expand it.
	FormatAllowed *bool `json:"format_allowed"`
}

// RDMA separates requested selections from observed required capabilities.
type RDMA struct {
	Status               string   `json:"status"`
	Device               string   `json:"device,omitempty"`
	Port                 int      `json:"port,omitempty"`
	GIDIndex             *int     `json:"gid_index,omitempty"`
	GID                  string   `json:"gid,omitempty"`
	LinkLayer            string   `json:"link_layer,omitempty"`
	ActiveMTU            int      `json:"active_mtu,omitempty"`
	Capabilities         []string `json:"capabilities,omitempty"`
	FabricEvidenceSHA256 string   `json:"fabric_evidence_sha256,omitempty"`
}

// Observation binds per-host facts to the exact requested document bytes.
type Observation struct {
	RequestedSHA256 string            `json:"requested_sha256"`
	Hosts           []HostObservation `json:"hosts"`
}

// HostObservation identifies a capture generation; its hash is not attestation.
type HostObservation struct {
	HostID         string `json:"host_id"`
	BootID         string `json:"boot_id"`
	CapturedAt     string `json:"captured_at"`
	EvidenceSHA256 string `json:"evidence_sha256"`
}

// Deadlines preserves the original work envelope. Decode validates data, not GO
// or freshness; live admission checks the clock and may never extend this bound.
type Deadlines struct {
	Work             string `json:"work"`
	Hard             string `json:"hard"`
	CleanupReserveMS int64  `json:"cleanup_reserve_ms"`
}

// Service declares an explicit per-host listener; no implicit port arithmetic.
type Service struct {
	Name string `json:"name"`
	Port int    `json:"port"`
}
