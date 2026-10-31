package contract

import "encoding/json"

// OfflineDocumentV1 is a selected DocType record in bootstrap or pull.
//
//contract:wire
type OfflineDocumentV1 struct {
	Doctype string `json:"doctype"`
	//contract:json
	Record json.RawMessage `json:"record"`
}

// OfflineTombstoneV1 removes a server record from the client's clean baseline.
//
//contract:wire
type OfflineTombstoneV1 struct {
	Doctype string `json:"doctype"`
	Name    string `json:"name"`
}

//contract:wire
type OfflineBootstrapRequestV1 struct {
	Doctypes      []string `json:"doctypes"`
	SnapshotToken string   `json:"snapshot_token"`
	PageToken     string   `json:"page_token"`
	Limit         int      `json:"limit"`
}

//contract:wire
type OfflineBootstrapResponseV1 struct {
	SnapshotToken string              `json:"snapshot_token"`
	Generation    uint64              `json:"generation"`
	Records       []OfflineDocumentV1 `json:"records"`
	NextPageToken string              `json:"next_page_token,omitempty"`
	PullCursor    string              `json:"pull_cursor,omitempty"`
	Doctypes      []string            `json:"doctypes"`
	Complete      bool                `json:"complete"`
}

//contract:wire
type OfflinePullRequestV1 struct {
	Cursor string `json:"cursor"`
	Limit  int    `json:"limit"`
}

//contract:wire
type OfflinePullResponseV1 struct {
	Changes    []OfflineDocumentV1  `json:"changes"`
	Tombstones []OfflineTombstoneV1 `json:"tombstones"`
	NextCursor string               `json:"next_cursor"`
	HasMore    bool                 `json:"has_more"`
	Generation uint64               `json:"generation"`
}
