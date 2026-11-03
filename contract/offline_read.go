package contract

import "encoding/json"

// OfflineCapabilities describes server bounds and the current catalog revision.
//
//contract:wire:v2
type OfflineCapabilities struct {
	ProtocolVersion         uint16 `json:"protocol_version"`
	CatalogRevision         uint64 `json:"catalog_revision"`
	MaxBatchSize            uint16 `json:"max_batch_size"`
	MaxPageSize             uint16 `json:"max_page_size"`
	SnapshotLifetimeSeconds uint32 `json:"snapshot_lifetime_seconds"`
}

// OfflineSourceRef selects one owner-published query. Input is validated by
// that query's registered schema; actor and entity restrictions come from the
// authenticated request and Engine policy.
//
//contract:wire:v2
type OfflineSourceRef struct {
	OwnerModuleID ModuleID  `json:"owner_module_id"`
	QueryName     string    `json:"query_name"`
	InputSchema   SchemaRef `json:"input_schema"`
	//contract:maxJSONBytes=65536
	//contract:json
	Input json.RawMessage `json:"input"`
}

//contract:wire:v2
type OfflineBootstrapRequest struct {
	Source          OfflineSourceRef `json:"source"`
	BusinessScope   BusinessScope    `json:"business_scope"`
	CatalogRevision uint64           `json:"catalog_revision"`
	Limit           uint16           `json:"limit"`
	SnapshotToken   string           `json:"snapshot_token,omitempty"`
	PageCursor      string           `json:"page_cursor,omitempty"`
	InputHash       string           `json:"input_hash,omitempty"`
}

// OfflineSnapshotPage binds result pages and the first replay position to the
// same actor, scope, query input and catalog revision.
//
//contract:wire:v2
type OfflineSnapshotPage struct {
	Source          OfflineSourceRef `json:"source"`
	InputHash       string           `json:"input_hash"`
	BusinessScope   BusinessScope    `json:"business_scope"`
	SubjectID       string           `json:"subject_id"`
	SchemaRevision  SchemaRef        `json:"schema_revision"`
	CatalogRevision uint64           `json:"catalog_revision"`
	SnapshotToken   string           `json:"snapshot_token"`
	PageCursor      string           `json:"page_cursor,omitempty"`
	NextPageCursor  string           `json:"next_page_cursor,omitempty"`
	ReplayPosition  string           `json:"replay_position"`
	//contract:json
	Rows     json.RawMessage `json:"rows"`
	Complete bool            `json:"complete"`
}

//contract:wire:v2
type OfflinePullRequest struct {
	Source          OfflineSourceRef `json:"source"`
	BusinessScope   BusinessScope    `json:"business_scope"`
	CatalogRevision uint64           `json:"catalog_revision"`
	SnapshotToken   string           `json:"snapshot_token"`
	InputHash       string           `json:"input_hash"`
	ReplayPosition  string           `json:"replay_position"`
	ThroughCursor   string           `json:"through_cursor,omitempty"`
	PageCursor      string           `json:"page_cursor,omitempty"`
	Limit           uint16           `json:"limit"`
}

// OfflineChange carries a complete published row replacement or an explicit
// deletion for its stable owner fact key.
//
//contract:wire:v2
type OfflineChange struct {
	FactKey string `json:"fact_key"`
	Deleted bool   `json:"deleted"`
	//contract:json
	Data json.RawMessage `json:"data,omitempty"`
}

//contract:wire:v2
type OfflinePullResponse struct {
	Source          OfflineSourceRef `json:"source"`
	InputHash       string           `json:"input_hash"`
	BusinessScope   BusinessScope    `json:"business_scope"`
	SubjectID       string           `json:"subject_id"`
	SchemaRevision  SchemaRef        `json:"schema_revision"`
	CatalogRevision uint64           `json:"catalog_revision"`
	SnapshotToken   string           `json:"snapshot_token"`
	ReplayPosition  string           `json:"replay_position"`
	ThroughCursor   string           `json:"through_cursor"`
	NextPageCursor  string           `json:"next_page_cursor,omitempty"`
	Changes         []OfflineChange  `json:"changes"`
	HasMore         bool             `json:"has_more"`
}
