package inventory

// DockerProfile declares Gate 0a placement, not a privileged executor receipt.
// Gate 0b Docker/SR-IOV is deliberately not representable by this extension.
type DockerProfile struct {
	Parent                DockerParent           `json:"parent"`
	Network               DockerNetwork          `json:"network"`
	RDMAMode              string                 `json:"rdma_mode"`
	UnavailableTransports []UnavailableTransport `json:"unavailable_transports"`
	MetadataHostID        string                 `json:"metadata_host_id"`
	MetadataStore         string                 `json:"metadata_store"`
}
type DockerParent struct {
	ID             string     `json:"id"`
	Management     Management `json:"management"`
	RunRoot        string     `json:"run_root"`
	ExecutorRef    string     `json:"executor_ref"`
	ExecutorSHA256 string     `json:"executor_sha256"`
	BootID         string     `json:"boot_id"`
	RootDevice     string     `json:"root_device"`
	RootInode      string     `json:"root_inode"`
}
type DockerNetwork struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}
type UnavailableTransport struct {
	Transport string `json:"transport"`
	Status    string `json:"status"`
	Reason    string `json:"reason"`
}

// Container is one role on one parent, reachable through the existing pinned
// SSH path. Full IDs and namespace identity are separately observed facts.
type Container struct {
	ParentID    string           `json:"parent_id"`
	Name        string           `json:"name"`
	ImageDigest string           `json:"image_digest"`
	ID          string           `json:"id"`
	NetNSInode  string           `json:"netns_inode"`
	Privileged  *bool            `json:"privileged"`
	Mounts      []ContainerMount `json:"mounts"`
}

// ContainerMount binds a logical disk to a private parent-directory bind mount.
// Device/inode refer to the parent stat identity, never a block format target.
type ContainerMount struct {
	DiskID       string `json:"disk_id"`
	SourcePath   string `json:"source_path"`
	SourceDevice string `json:"source_device"`
	SourceInode  string `json:"source_inode"`
}
