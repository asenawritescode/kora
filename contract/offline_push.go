package contract

import (
	"encoding/json"
	"errors"
	"strings"
)

var ErrInvalidOfflineIntent = errors.New("offline intent is invalid")

// OfflineIntent is a durable request to execute one explicitly published
// domain operation. Its stable identity must be retained by the client until
// Engine returns a terminal receipt.
//
//contract:wire:v2
type OfflineIntent struct {
	LocalOperationID string        `json:"local_operation_id"`
	BusinessScope    BusinessScope `json:"business_scope"`
	OperationRef     string        `json:"operation_ref"`
	InputSchema      SchemaRef     `json:"input_schema"`
	//contract:maxJSONBytes=1048576
	//contract:json
	Input            json.RawMessage `json:"input"`
	IdempotencyKey   IdempotencyKey  `json:"idempotency_key"`
	ExpectedRevision Revision        `json:"expected_revision,omitempty"`
	CatalogRevision  uint64          `json:"catalog_revision"`
}

// OfflineIntentReceipt binds the client's stable local ID to Engine's
// authoritative operation receipt.
//
//contract:wire:v2
type OfflineIntentReceipt struct {
	LocalOperationID string           `json:"local_operation_id"`
	Receipt          OperationReceipt `json:"receipt"`
}

// OfflineIntentsRequest is a bounded batch; each item is independently
// authorized and durably receipted through the published operation service.
//
//contract:wire:v2
type OfflineIntentsRequest struct {
	Intents []OfflineIntent `json:"intents"`
}

//contract:wire:v2
type OfflineIntentsResponse struct {
	Receipts []OfflineIntentReceipt `json:"receipts"`
}

func (i OfflineIntent) Validate() error {
	if i.LocalOperationID == "" || len(i.LocalOperationID) > 128 || strings.TrimSpace(i.LocalOperationID) != i.LocalOperationID || i.BusinessScope.Validate() != nil || i.OperationRef == "" || len(i.OperationRef) > 256 || strings.TrimSpace(i.OperationRef) != i.OperationRef || i.InputSchema.ID == "" || i.InputSchema.Version == 0 || len(i.Input) == 0 || len(i.Input) > 1<<20 || !json.Valid(i.Input) || i.IdempotencyKey == "" || len(i.IdempotencyKey) > 256 || i.CatalogRevision == 0 {
		return ErrInvalidOfflineIntent
	}
	if i.ExpectedRevision != "" {
		if _, err := ParseRevision(string(i.ExpectedRevision)); err != nil {
			return ErrInvalidOfflineIntent
		}
	}
	return nil
}
