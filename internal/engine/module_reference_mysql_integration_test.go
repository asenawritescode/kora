//go:build integration

package engine

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/plugin"
	"github.com/asenawritescode/kora/policy"
	_ "github.com/go-sql-driver/mysql"
)

func TestInternalProviderBusinessReferenceIsEntityScopedOnMySQL(t *testing.T) {
	dsn := os.Getenv("KORA_ENGINE_MODULE_COMMAND_DISPOSABLE_MYSQL_DSN")
	if dsn == "" { t.Skip("set KORA_ENGINE_MODULE_COMMAND_DISPOSABLE_MYSQL_DSN to a disposable MySQL database") }
	ctx := context.Background()
	database, err := sql.Open("mysql", dsn)
	if err != nil { t.Fatal(err) }
	database.SetMaxOpenConns(4)
	t.Cleanup(func(){ _ = database.Close() })
	if err := database.PingContext(ctx); err != nil { t.Fatal(err) }
	var suffix string
	if err := database.QueryRowContext(ctx, "SELECT REPLACE(UUID(), '-', '')").Scan(&suffix); err != nil { t.Fatal(err) }
	name := "Warehouse Reference " + suffix[:8]
	target := &doctype.DocType{Name:name, BusinessResourceType:"warehouse.location", BusinessIdentityField:"external_code", ModuleOwnerID:"warehouse.module", ModuleAccess:contract.ModulePublished, BusinessScopePolicy:contract.ScopeEntityOwned, BusinessScopeField:"legal_entity_id", Fields:[]doctype.Field{{Fieldname:"external_code", Fieldtype:"Data", Unique:true}, {Fieldname:"legal_entity_id", Fieldtype:"Data"}}}
	table := target.RawTableName()
	quotedTable := db.Resolve("mysql").QuoteIdent(table)
	if _, err := database.ExecContext(ctx, "CREATE TABLE "+quotedTable+" (name varchar(140) PRIMARY KEY, external_code varchar(100) NOT NULL UNIQUE, legal_entity_id varchar(100) NOT NULL)"); err != nil { t.Fatal("create disposable provider resource table:", err) }
	t.Cleanup(func(){ _, _ = database.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+quotedTable) })
	if _, err := database.ExecContext(ctx, "INSERT INTO "+quotedTable+" (name, external_code, legal_entity_id) VALUES (?, ?, ?), (?, ?, ?)", "ROW-A", "EXT-A", "entity-a", "ROW-B", "EXT-B", "entity-b"); err != nil { t.Fatal(err) }
	registry := doctype.NewRegistry()
	registry.Register(target)
	tenant := contract.TenantID("tenant-" + suffix)
	capability := contract.CapabilityName("warehouse.lookup")
	schema := contract.SchemaRef{ID:"warehouse.lookup", Version:1}
	offer := contract.CapabilityOffer{Name:capability, Contract:schema}
	consumer := contract.ModuleDescriptor{ID:"sales.orders", Version:"1.0.0", Requires:[]contract.CapabilityRequirement{{Name:capability, Contract:schema, Required:true}}}
	owner := contract.ModuleDescriptor{ID:"warehouse.module", Version:"1.0.0", Provides:[]contract.CapabilityOffer{offer}}
	factory := &kernelCommandProviderFactory{database:database, registry:registry, engine:kernel.New(db.Resolve("mysql"), nil), tenant:tenant, owner:owner.ID, capability:capability}
	provider := plugin.ProviderRegistration{TenantID:tenant, ProviderID:"engine.internal.reference-fixture", OwnerModuleID:owner.ID, Kind:plugin.ProviderInternal, Offers:[]contract.CapabilityOffer{offer}, Factories:map[contract.CapabilityName]plugin.ProviderFactory{capability:factory}}
	scopeA := contract.BusinessScope{TenantID:tenant, Kind:contract.ScopeKindLegalEntity, LegalEntityID:"entity-a"}
	plan, err := plugin.ResolveModules(scopeA, []contract.ModuleDescriptor{consumer, owner}, []plugin.ProviderRegistration{provider}, nil, 1)
	if err != nil { t.Fatal(err) }
	execution, err := policy.NewVerifiedExecution(tenant, contract.ActorContext{PrincipalID:"fixture-user", PrincipalType:contract.PrincipalHuman, Site:string(tenant), AuthenticatedAt:time.Now().UTC()})
	if err != nil { t.Fatal(err) }
	rule := contract.ReferenceRule{Kind:"published_business_ref", CapabilityName:capability, ResourceType:"warehouse.location"}
	refA := contract.BusinessRef{ProviderID:provider.ProviderID, ResourceType:"warehouse.location", ID:"EXT-A", Scope:scopeA}
	if _, err := plan.ResolveBusinessReference(ctx, execution, scopeA, consumer.ID, rule, refA); err != nil { t.Fatal("resolve entity A identity:", err) }
	wrongEntityID := refA
	wrongEntityID.ID = "EXT-B"
	if _, err := plan.ResolveBusinessReference(ctx, execution, scopeA, consumer.ID, rule, wrongEntityID); err == nil { t.Fatal("entity B identity resolved from entity A scope") }
	if err := database.PingContext(ctx); err != nil { t.Fatal(fmt.Errorf("MySQL connection after reference verification: %w", err)) }
}
