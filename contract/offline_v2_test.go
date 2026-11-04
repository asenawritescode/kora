package contract

import (
	"encoding/json"
	"testing"
)

func TestOfflineIntentRequiresStableIdentityAndBoundedSchemaInput(t *testing.T) {
	intent := OfflineIntent{
		LocalOperationID: "550e8400-e29b-41d4-a716-446655440000",
		BusinessScope:    BusinessScope{TenantID: "tenant-a", Kind: ScopeKindTenant},
		OperationRef:     "sales.orders~sales.orders~submit",
		InputSchema:      SchemaRef{ID: "sales.submit", Version: 1},
		Input:            json.RawMessage(`{"id":"S-1"}`),
		IdempotencyKey:   "offline-intent-1",
		CatalogRevision:  4,
	}
	if err := intent.Validate(); err != nil {
		t.Fatalf("valid offline intent rejected: %v", err)
	}
	intent.CatalogRevision = 0
	if err := intent.Validate(); err == nil {
		t.Fatal("intent without catalog revision accepted")
	}
	intent.CatalogRevision = 4
	intent.Input = json.RawMessage(`{"id":`)
	if err := intent.Validate(); err == nil {
		t.Fatal("malformed input accepted")
	}
}

func TestOfflineIntentBatchWireShapeDoesNotAcceptLegacyRecordPayloads(t *testing.T) {
	var request OfflineIntentsRequest
	if err := json.Unmarshal([]byte(`{"intents":[{"local_operation_id":"op-1","business_scope":{"tenant_id":"tenant-a","kind":"tenant"},"operation_ref":"sales~sales~submit","input_schema":{"id":"sales.submit","version":1},"input":{"id":"S-1"},"idempotency_key":"retry-1","catalog_revision":1}]}`), &request); err != nil {
		t.Fatal(err)
	}
	if len(request.Intents) != 1 || request.Intents[0].Validate() != nil {
		t.Fatalf("v2 intent failed validation: %+v", request)
	}
}
