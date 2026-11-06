//go:build integration

package engine

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/plugin"
	"github.com/asenawritescode/kora/policy"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
)

// Published business-reference lookups must bind placeholders through the
// tenant dialect: PostgreSQL needs $N, MySQL keeps ?.
func TestInternalProviderBusinessReferenceResolvesAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_CONFIG_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_CONFIG_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(tc.env))
			if dsn == "" {
				t.Skipf("set %s to a disposable database", tc.env)
			}
			ctx := context.Background()
			database, _ := isolatedDB(t, tc.driver, dsn)
			dialect := db.Resolve(tc.driver)
			target := &doctype.DocType{Name: "Warehouse Reference", BusinessResourceType: "warehouse.location", BusinessIdentityField: "external_code", ModuleOwnerID: "warehouse.module", ModuleAccess: contract.ModulePublished, BusinessScopePolicy: contract.ScopeEntityOwned, BusinessScopeField: "legal_entity_id", Fields: []doctype.Field{{Fieldname: "external_code", Fieldtype: "Data", Unique: true}, {Fieldname: "legal_entity_id", Fieldtype: "Data"}}}
			table := dialect.QuoteIdent(target.RawTableName())
			if _, err := database.ExecContext(ctx, "CREATE TABLE "+table+" (name varchar(140) PRIMARY KEY, external_code varchar(100) NOT NULL UNIQUE, legal_entity_id varchar(100) NOT NULL)"); err != nil {
				t.Fatal("create provider resource table:", err)
			}
			if _, err := database.ExecContext(ctx, db.Rebind(dialect, "INSERT INTO "+table+" (name, external_code, legal_entity_id) VALUES (?, ?, ?), (?, ?, ?)"), "ROW-A", "EXT-A", "entity-a", "ROW-B", "EXT-B", "entity-b"); err != nil {
				t.Fatal(err)
			}
			registry := doctype.NewRegistry()
			registry.Register(target)
			tenant := contract.TenantID("tenant-ref-" + tc.driver)
			capability := contract.CapabilityName("warehouse.lookup")
			schema := contract.SchemaRef{ID: "warehouse.lookup", Version: 1}
			offer := contract.CapabilityOffer{Name: capability, Contract: schema}
			consumer := contract.ModuleDescriptor{ID: "sales.orders", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: capability, Contract: schema, Required: true}}}
			owner := contract.ModuleDescriptor{ID: "warehouse.module", Version: "1.0.0", Provides: []contract.CapabilityOffer{offer}}
			factory := &kernelCommandProviderFactory{database: database, registry: registry, engine: kernel.New(dialect, nil), tenant: tenant, owner: owner.ID, capability: capability}
			provider := plugin.ProviderRegistration{TenantID: tenant, ProviderID: "engine.internal.reference-fixture", OwnerModuleID: owner.ID, Kind: plugin.ProviderInternal, Offers: []contract.CapabilityOffer{offer}, Factories: map[contract.CapabilityName]plugin.ProviderFactory{capability: factory}}
			scopeA := contract.BusinessScope{TenantID: tenant, Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
			plan, err := plugin.ResolveModules(scopeA, []contract.ModuleDescriptor{consumer, owner}, []plugin.ProviderRegistration{provider}, nil, 1)
			if err != nil {
				t.Fatal(err)
			}
			execution, err := policy.NewVerifiedExecution(tenant, contract.ActorContext{PrincipalID: "fixture-user", PrincipalType: contract.PrincipalHuman, Site: string(tenant), AuthenticatedAt: time.Now().UTC()})
			if err != nil {
				t.Fatal(err)
			}
			rule := contract.ReferenceRule{Kind: "published_business_ref", CapabilityName: capability, ResourceType: "warehouse.location"}
			refA := contract.BusinessRef{ProviderID: provider.ProviderID, ResourceType: "warehouse.location", ID: "EXT-A", Scope: scopeA}
			if _, err := plan.ResolveBusinessReference(ctx, execution, scopeA, consumer.ID, rule, refA); err != nil {
				t.Fatal("resolve entity A identity (entity-scoped branch):", err)
			}
			otherEntity := refA
			otherEntity.ID = "EXT-B"
			if _, err := plan.ResolveBusinessReference(ctx, execution, scopeA, consumer.ID, rule, otherEntity); err == nil {
				t.Fatal("entity B identity resolved from entity A scope")
			}
			missing := refA
			missing.ID = "EXT-NONE"
			if _, err := plan.ResolveBusinessReference(ctx, execution, scopeA, consumer.ID, rule, missing); err == nil {
				t.Fatal("unknown identity resolved")
			}
			// Tenant-owned branch (no entity filter) is the single-placeholder query.
			target.BusinessScopePolicy = contract.ScopeTenantShared
			target.BusinessScopeField = ""
			scopeT := contract.BusinessScope{TenantID: tenant, Kind: contract.ScopeKindTenant}
			planT, err := plugin.ResolveModules(scopeT, []contract.ModuleDescriptor{consumer, owner}, []plugin.ProviderRegistration{provider}, nil, 1)
			if err != nil {
				t.Fatal(err)
			}
			refT := contract.BusinessRef{ProviderID: provider.ProviderID, ResourceType: "warehouse.location", ID: "EXT-B", Scope: scopeT}
			if _, err := planT.ResolveBusinessReference(ctx, execution, scopeT, consumer.ID, rule, refT); err != nil {
				t.Fatal("resolve tenant-owned identity:", err)
			}
		})
	}
}
