package contract

// SiteDirectoryVersion is the current provider-neutral site directory wire
// contract. Breaking changes require a new versioned contract.
const SiteDirectoryVersion = 1

// SiteDescriptor is the credential-free projection shared by Engine and its
// control-plane consumers. Database connection details are never part of this
// contract; data-plane references are opaque identifiers only.
type SiteDescriptor struct {
	Version           int      `json:"version"`
	SiteID            string   `json:"site_id"`
	Aliases           []string `json:"aliases"`
	Status            string   `json:"status"`
	ConfigRevision    uint64   `json:"config_revision,omitempty"`
	DirectoryRevision uint64   `json:"directory_revision,omitempty"`
	RuntimeCellID     string   `json:"runtime_cell_id,omitempty"`
	DataPlaneRef      string   `json:"data_plane_ref,omitempty"`
}
