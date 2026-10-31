package contract

import "encoding/json"

// OfflinePushRequestV1 is a bounded, site-scoped batch of record commands.
// The authenticated request context supplies the site and actor.
type OfflinePushRequestV1 struct {
	Operations []OfflinePushOperationV1 `json:"operations"`
}

type OfflinePushOperationV1 struct {
	ID               string          `json:"id"`
	IdempotencyKey   string          `json:"idempotency_key"`
	Command          string          `json:"command"`
	Doctype          string          `json:"doctype"`
	Name             string          `json:"name,omitempty"`
	ExpectedRevision string          `json:"expected_revision,omitempty"`
	Payload          json.RawMessage `json:"payload"`
}

type OfflinePushResponseV1 struct {
	Receipts []OfflinePushReceiptV1 `json:"receipts"`
}

type OfflinePushReceiptV1 struct {
	OperationID string            `json:"operation_id"`
	Status      string            `json:"status"`
	Replayed    bool              `json:"replayed,omitempty"`
	Record      json.RawMessage   `json:"record,omitempty"`
	Error       *OfflineProblemV1 `json:"error,omitempty"`
}

type OfflineProblemV1 struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
