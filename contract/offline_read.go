package contract

import (
	"encoding/json"
	"fmt"
)

// OfflineCapabilities describes server bounds and the current catalog revision.
//
//contract:wire:v2
type OfflineCapabilities struct {
	ProtocolVersion         uint16                        `json:"protocol_version"`
	CatalogRevision         uint64                        `json:"catalog_revision"`
	MaxBatchSize            uint16                        `json:"max_batch_size"`
	MaxPageSize             uint16                        `json:"max_page_size"`
	SnapshotLifetimeSeconds uint32                        `json:"snapshot_lifetime_seconds"`
	OfflineQueries          []ApplicationCatalogQuery     `json:"offline_queries"`
	OfflineOperations       []ApplicationCatalogOperation `json:"offline_operations"`
}

func (c OfflineCapabilities) Validate() error {
	if c.ProtocolVersion != 2 || c.CatalogRevision == 0 || c.MaxBatchSize == 0 || c.MaxBatchSize > 20 || c.MaxPageSize == 0 || c.MaxPageSize > 100 || c.SnapshotLifetimeSeconds == 0 || len(c.OfflineQueries) > 2000 || len(c.OfflineOperations) > 2000 {
		return fmt.Errorf("offline capability identity or bounds are invalid")
	}
	queries := make(map[string]bool, len(c.OfflineQueries))
	for _, query := range c.OfflineQueries {
		key := string(query.OwnerModuleID) + "/" + query.Name
		factKeyPublished := false
		for _, field := range query.AllowedFields {
			factKeyPublished = factKeyPublished || field == query.OfflineFactKey
		}
		if !query.OfflineEligible || query.OwnerModuleID == "" || !validModuleName(query.Name) || query.InputSchema.ID == "" || query.InputSchema.Version == 0 || query.OutputSchema.ID == "" || query.OutputSchema.Version == 0 || query.ScopePolicy != ScopeTenantShared && query.ScopePolicy != ScopeEntityOwned || query.PageLimit == 0 || query.PageLimit > 1000 || !validModuleName(query.OfflineFactKey) || !factKeyPublished || queries[key] {
			return fmt.Errorf("offline capability contains an invalid or duplicate query")
		}
		queries[key] = true
	}
	operations := make(map[string]bool, len(c.OfflineOperations))
	for _, operation := range c.OfflineOperations {
		if !operation.OfflineEligible || operation.Availability != OperationAvailable || operation.OperationRef == "" || operation.InputSchema.ID == "" || operation.InputSchema.Version == 0 || operation.OutputSchema.ID == "" || operation.OutputSchema.Version == 0 || operations[operation.OperationRef] {
			return fmt.Errorf("offline capability contains an invalid or duplicate operation")
		}
		operations[operation.OperationRef] = true
	}
	return nil
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
	FactKeyField    string           `json:"fact_key_field"`
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

func (p OfflineSnapshotPage) Validate(query PublishedQuery, limit uint16, scope BusinessScope) error {
	if !query.OfflineEligible || p.FactKeyField != query.OfflineFactKey || p.BusinessScope != scope || p.SchemaRevision != query.OutputSchema || p.CatalogRevision == 0 || p.SnapshotToken == "" || p.ReplayPosition == "" {
		return fmt.Errorf("offline snapshot identity does not match its published query")
	}
	if limit == 0 || limit > query.PageLimit || len(p.Rows) == 0 || !json.Valid(p.Rows) {
		return fmt.Errorf("offline snapshot rows exceed their published query bounds")
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(p.Rows, &rows); err != nil || len(rows) > int(limit) {
		return fmt.Errorf("offline snapshot rows must be a bounded row array")
	}
	for _, row := range rows {
		if _, err := OfflineQueryFactKey(row, query.OfflineFactKey); err != nil {
			return fmt.Errorf("offline snapshot row has no stable fact identity")
		}
	}
	return nil
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
