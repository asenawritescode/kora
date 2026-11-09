package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/plugin"
	_ "github.com/mattn/go-sqlite3"
)

type publishedQueryTestSchemas map[contract.SchemaRef]bool

func TestIncomingModuleSchemasRegisterOfflineProviderForEmptyRuntime(t *testing.T) {
	database, err := sql.Open("sqlite3", t.TempDir()+"/candidate.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	in := contract.SchemaRef{ID: "incoming.input", Version: 1}
	out := contract.SchemaRef{ID: "incoming.output", Version: 1}
	query := contract.PublishedQuery{OwnerModuleID: "incoming.owner", Name: "list", Capability: "incoming.read", Operation: "list", InputSchema: in, OutputSchema: out, ScopePolicy: contract.ScopeTenantShared, AllowedFields: []string{"id"}, PageLimit: 10, OfflineEligible: true, OfflineFactKey: "id"}
	modules := []contract.ModuleDescriptor{{ID: query.OwnerModuleID, Version: "1.0.0", Queries: []contract.PublishedQuery{query}, Schemas: []contract.ModuleSchemaDocument{{Ref: in, Schema: json.RawMessage(`{"type":"object"}`)}, {Ref: out, Schema: json.RawMessage(`{"type":"array"}`)}}, Provides: []contract.CapabilityOffer{{Name: query.Capability, Contract: out, Operations: []contract.OperationSpec{{Name: "list", Input: in, Output: out, Effect: contract.EffectQuery}}}}}}
	schemas, err := newModulePayloadSchemas(modules, nil)
	if err != nil {
		t.Fatal(err)
	}
	providers, err := registeredSQLPublishedQueryProviders(t.Context(), moduleProviderSource{tenant: "tenant-new", db: database, dialect: db.Resolve("libsql"), modules: modules, schemas: schemas})
	if err != nil {
		t.Fatal(err)
	}
	scope := contract.BusinessScope{TenantID: "tenant-new", Kind: contract.ScopeKindTenant}
	plan, err := plugin.ResolveModules(scope, modules, providers, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Blockers) != 0 || len(plan.QueryDefinitions) != 1 {
		t.Fatalf("incoming candidate unresolved: blockers=%v queries=%d", plan.Blockers, len(plan.QueryDefinitions))
	}
	if !plan.CanReplayPublishedQueryOffline(scope, query.OwnerModuleID, query.Name) {
		t.Fatal("incoming published query is not replayable")
	}
}

func (s publishedQueryTestSchemas) Registered(ref contract.SchemaRef) bool { return s[ref] }
func (s publishedQueryTestSchemas) Validate(_ contract.SchemaRef, raw json.RawMessage) error {
	if !json.Valid(raw) {
		return fmt.Errorf("invalid JSON")
	}
	return nil
}

func TestRegisteredSQLPublishedQueryProviders_WiresOnlyUnconfiguredQueryOnlyCapabilities(t *testing.T) {
	database, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)

	in := contract.SchemaRef{ID: "query.input", Version: 1}
	out := contract.SchemaRef{ID: "query.output", Version: 1}
	query := contract.PublishedQuery{
		OwnerModuleID: "sales.orders", Name: "mine", Capability: "sales.orders.read", Operation: "list",
		InputSchema: in, OutputSchema: out, ScopePolicy: contract.ScopeEntityOwned,
		AllowedFields: []string{"id", "owner_id"}, OwnerField: "owner_id", PageLimit: 10,
		OfflineEligible: true, OfflineFactKey: "id",
	}
	offer := contract.CapabilityOffer{Name: query.Capability, Contract: out, Operations: []contract.OperationSpec{{Name: query.Operation, Input: in, Output: out, Effect: contract.EffectQuery}}}
	module := contract.ModuleDescriptor{ID: query.OwnerModuleID, Version: "1.0.0", Provides: []contract.CapabilityOffer{offer}, Queries: []contract.PublishedQuery{query}}
	schemas := publishedQueryTestSchemas{in: true, out: true}

	providers, err := registeredSQLPublishedQueryProviders(context.Background(), moduleProviderSource{tenant: "tenant-a", db: database, dialect: db.Resolve("libsql"), modules: []contract.ModuleDescriptor{module}, schemas: schemas})
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || providers[0].Queries[query.Name] == nil || providers[0].ProviderID != "engine.published-facts.sales.orders" {
		t.Fatalf("providers = %+v", providers)
	}
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	plan, err := plugin.ResolveModules(scope, []contract.ModuleDescriptor{module}, providers, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.CanReplayPublishedQueryOffline(scope, module.ID, query.Name) {
		t.Fatal("registered provider was not eligible for offline replay")
	}

	configured := []plugin.ProviderRegistration{{TenantID: "tenant-a", ProviderID: "external.sales", OwnerModuleID: module.ID, Kind: plugin.ProviderExternal, Offers: []contract.CapabilityOffer{offer}, Queries: map[string]plugin.PublishedQueryExecutor{query.Name: providers[0].Queries[query.Name]}, ValidateSchema: providers[0].ValidateSchema}}
	providers, err = registeredSQLPublishedQueryProviders(context.Background(), moduleProviderSource{tenant: "tenant-a", db: database, dialect: db.Resolve("libsql"), modules: []contract.ModuleDescriptor{module}, schemas: schemas, configured: configured})
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 0 {
		t.Fatalf("configured provider was shadowed: %+v", providers)
	}

	mixed := module
	mixed.Provides = []contract.CapabilityOffer{{Name: offer.Name, Contract: out, Operations: append(append([]contract.OperationSpec(nil), offer.Operations...), contract.OperationSpec{Name: "mutate", Input: in, Output: out, Effect: contract.EffectLocal})}}
	providers, err = registeredSQLPublishedQueryProviders(context.Background(), moduleProviderSource{tenant: "tenant-a", db: database, dialect: db.Resolve("libsql"), modules: []contract.ModuleDescriptor{mixed}, schemas: schemas})
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 0 {
		t.Fatalf("mixed effect capability was implicitly registered: %+v", providers)
	}
}
