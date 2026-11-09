package doctype

import (
	"testing"

	"github.com/asenawritescode/kora/contract"
)

func TestBindBusinessScopeRejectsForgedAndChangedEntity(t *testing.T) {
	dt := &DocType{Name: "Invoice", BusinessScopePolicy: contract.ScopeEntityOwned, BusinessScopeField: "legal_entity_id", Fields: []Field{{Fieldname: "legal_entity_id", Fieldtype: "Link", Options: "LegalEntity"}}}
	other := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-b"}
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	doc := NewDocument(dt.Name)
	doc.Set("legal_entity_id", "entity-b")
	if err := dt.BindBusinessScope(doc, scope, true, nil); err == nil {
		t.Fatal("client-supplied entity ID was accepted")
	}
	doc.Set("legal_entity_id", "entity-a")
	if err := dt.BindBusinessScope(doc, scope, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := dt.BindBusinessScope(doc, other, false, doc); err == nil {
		t.Fatal("record could be rebound to another entity")
	}
	if err := dt.BindBusinessScope(doc, contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}, false, doc); err == nil {
		t.Fatal("entity-owned record accepted missing entity scope")
	}
}
