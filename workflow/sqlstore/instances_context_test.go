package sqlstore

import (
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

func TestPersistedWorkflowBusinessContextRetainsCausationDepth(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	raw, err := json.Marshal(persistedWorkflowBusinessContext{BusinessScope: scope, CausationDepth: 7})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := decodePersistedWorkflowBusinessContext(string(raw), "tenant-a")
	if err != nil || stored.BusinessScope != scope || stored.CausationDepth != 7 {
		t.Fatalf("workflow context round trip = %+v, %v", stored, err)
	}
	// Existing scope JSON remains readable and starts at the root depth.
	legacy, err := json.Marshal(scope)
	if err != nil {
		t.Fatal(err)
	}
	stored, err = decodePersistedWorkflowBusinessContext(string(legacy), "tenant-a")
	if err != nil || stored.BusinessScope != scope || stored.CausationDepth != 0 {
		t.Fatalf("legacy workflow context = %+v, %v", stored, err)
	}
}
