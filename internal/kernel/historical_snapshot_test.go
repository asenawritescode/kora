package kernel

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
)

type snapshotSchemas struct {
	schema contract.SchemaRef
	called bool
	err    error
}

func (s *snapshotSchemas) Registered(ref contract.SchemaRef) bool { return ref == s.schema }
func (s *snapshotSchemas) Validate(ref contract.SchemaRef, raw json.RawMessage) error {
	s.called = true
	if s.err != nil {
		return s.err
	}
	if ref != s.schema || string(raw) != `{"label":"Approved"}` {
		return errors.New("snapshot schema payload mismatch")
	}
	return nil
}

func TestHistoricalSnapshotValidatorUsesCapturedSchemaAndVerifiedScope(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	schema := contract.SchemaRef{ID: "warehouse.location.snapshot", Version: 2}
	schemas := &snapshotSchemas{schema: schema}
	validate := newHistoricalSnapshotValidator(scope, schemas)
	snapshot := contract.HistoricalSnapshot{Source: contract.BusinessRef{ProviderID: "warehouse-old", ResourceType: "warehouse.location", ID: "EXT-1", Scope: scope}, BusinessScope: scope, CapturedAt: time.Now().UTC(), Schema: schema, Data: json.RawMessage(`{"label":"Approved"}`)}
	if err := validate(schema, snapshot); err != nil || !schemas.called {
		t.Fatalf("valid snapshot rejected: called=%v err=%v", schemas.called, err)
	}
	otherScope := scope
	otherScope.LegalEntityID = "entity-b"
	snapshot.BusinessScope = otherScope
	if err := validate(schema, snapshot); err == nil {
		t.Fatal("snapshot from another entity accepted")
	}
	if got := newHistoricalSnapshotValidator(scope, nil); got != nil {
		t.Fatal("snapshot validator available without registered schema service")
	}
}
