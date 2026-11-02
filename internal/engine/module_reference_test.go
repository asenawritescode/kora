package engine

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
)

func TestInternalProviderResolvesStableIdentityWithinEntityWithoutCompanionDoctype(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	target := &doctype.DocType{Name: "Warehouse Location", BusinessResourceType: "warehouse.location", BusinessIdentityField: "external_code", ModuleOwnerID: "warehouse.module", ModuleAccess: contract.ModulePublished, BusinessScopePolicy: contract.ScopeEntityOwned, BusinessScopeField: "legal_entity_id", Fields: []doctype.Field{{Fieldname: "external_code", Fieldtype: "Data", Unique: true}, {Fieldname: "legal_entity_id", Fieldtype: "Data"}}}
	registry := doctype.NewRegistry()
	registry.Register(target)
	factory := &kernelCommandProviderFactory{database: database, registry: registry, engine: kernel.New(db.Resolve("mysql"), nil), tenant: scope.TenantID, owner: "warehouse.module", capability: "warehouse.lookup"}
	handle := &kernelCommandProviderHandle{factory: factory, scope: scope}
	ref := contract.BusinessRef{ProviderID: "engine.internal.test", ResourceType: "warehouse.location", ID: "EXT-42", Scope: scope}
	query := "SELECT name FROM `" + target.RawTableName() + "` WHERE `external_code`=? AND `legal_entity_id`=?"
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs("EXT-42", "entity-a").WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("local-row-999"))
	resolved, err := handle.Resolve(context.Background(), contract.ScopeContext{Scope: scope, ProviderID: ref.ProviderID}, ref)
	if err != nil || resolved.Canonical != ref {
		t.Fatalf("stable reference resolution = %#v, %v", resolved, err)
	}

	otherScope := scope
	otherScope.LegalEntityID = "entity-b"
	otherRef := ref
	otherRef.Scope = otherScope
	otherHandle := &kernelCommandProviderHandle{factory: factory, scope: otherScope}
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs("EXT-42", "entity-b").WillReturnRows(sqlmock.NewRows([]string{"name"}))
	if _, err := otherHandle.Resolve(context.Background(), contract.ScopeContext{Scope: otherScope, ProviderID: ref.ProviderID}, otherRef); err == nil {
		t.Fatal("identity resolved across legal entities")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
