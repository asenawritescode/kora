//go:build integration

package engine

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/configstore"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/plugin"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/schema"
	"github.com/asenawritescode/kora/internal/site"
)

// acceptAllSchemas is declared in published_operation_refusal_integration_test.go.

// Several first requests for one published operation, with no capability
// lease yet, must all succeed and share one durable lease.
func TestPublishedOperationFirstUseLeaseRaceAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_ENGINE_PUBOP_LEASE_RACE_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_ENGINE_PUBOP_LEASE_RACE_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(tc.env))
			if dsn == "" {
				t.Skipf("set %s to a disposable database", tc.env)
			}
			runPublishedLeaseRaceFixture(t, tc.driver, dsn)
		})
	}
}

func runPublishedLeaseRaceFixture(t *testing.T, driver, dsn string) {
	ctx := context.Background()
	database, dbName := isolatedDB(t, driver, dsn)
	dialect := kdb.Resolve(driver)
	if err := site.BootstrapSystemTables(ctx, database, dialect); err != nil {
		t.Fatal("bootstrap system tables:", err)
	}
	kernelTables, outboxTables := kdb.KernelTablesPostgres(), kdb.OutboxTablesPostgres()
	if driver == "mysql" {
		kernelTables, outboxTables = kdb.KernelTablesMySQL(), kdb.OutboxTablesMySQL()
	}
	for _, statements := range [][]string{kernelTables, outboxTables} {
		for _, statement := range statements {
			if _, err := database.ExecContext(ctx, statement); err != nil {
				t.Fatalf("create kernel support schema: %v\n%s", err, statement)
			}
		}
	}
	tenant := contract.TenantID("tenant-" + dbName)
	invoice := &doctype.DocType{Name: "RefusalInvoice", ModuleOwnerID: "finance.ar", ModuleOwnershipMode: string(contract.MutationCommands), BusinessScopePolicy: contract.ScopeEntityOwned, BusinessScopeField: "legal_entity_id", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data", Reqd: true}, {Fieldname: "legal_entity_id", Fieldtype: "Data", Reqd: true}}}
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{invoice}, []*doctype.Role{{Name: doctype.AdminRole}}, nil)
	if err := schema.MigrateSiteFromRegistry(database, dbName, registry, dialect); err != nil {
		t.Fatal("migrate owner resource:", err)
	}
	issue, err := kernel.ParseCommandResource([]byte("name: issue\nnamespace: finance.invoice\nversion: 1\ninput:\n  record: " + invoice.Name + "\nmodule_owner_id: finance.ar\ncapability: billing.invoice\noperation: issue\ninput_schema:\n  id: billing.invoice.issue.input\n  version: 1\noutput_schema:\n  id: billing.invoice.issue.output\n  version: 1\ntransaction:\n  - create:\n      record: " + invoice.Name + "\n      values:\n        title: $input.title\n"))
	if err != nil {
		t.Fatal("parse issue command:", err)
	}
	refund, err := kernel.ParseCommandResource([]byte("name: refund\nnamespace: finance.invoice\nversion: 1\ninput:\n  record: " + invoice.Name + "\nmodule_owner_id: finance.ar\ncapability: billing.invoice\noperation: refund\ninput_schema:\n  id: billing.invoice.refund.input\n  version: 1\noutput_schema:\n  id: billing.invoice.refund.output\n  version: 1\nrequire:\n  - equal: [\"1\", \"2\"]\n    message: Only a paid sale can be returned\ntransaction:\n  - create:\n      record: " + invoice.Name + "\n      values:\n        title: $input.title\n"))
	if err != nil {
		t.Fatal("parse refund command:", err)
	}
	commands := kernel.NewCommandRegistry()
	for _, command := range []*kernel.CommandResource{issue, refund} {
		if err = commands.Register(command); err != nil {
			t.Fatal("register owner command:", err)
		}
	}
	commandKernel := kernel.New(dialect, outbox.NewSQLWriter(dialect))
	commandKernel.Commands = commands
	schemas := acceptAllSchemas{}
	for _, command := range []*kernel.CommandResource{issue, refund} {
		schemas[command.InputSchema] = true
		schemas[command.OutputSchema] = true
	}
	commandKernel.CommandSchemas = schemas
	offer := contract.CapabilityOffer{Name: "billing.invoice", Contract: issue.OutputSchema, Operations: []contract.OperationSpec{
		{Name: "issue", Input: issue.InputSchema, Output: issue.OutputSchema, Effect: contract.EffectLocal},
		{Name: "refund", Input: refund.InputSchema, Output: refund.OutputSchema, Effect: contract.EffectLocal},
	}}
	published := func(op string) contract.PublishedOperation {
		return contract.PublishedOperation{Name: op, Capability: offer.Name, Operation: op, ScopePolicy: contract.ScopeEntityOwned, AllowedCallers: []contract.ModuleID{"sales.orders"}}
	}
	owner := contract.ModuleDescriptor{ID: "finance.ar", Version: "1.0.0", Provides: []contract.CapabilityOffer{offer}, PublishedOperations: []contract.PublishedOperation{published("issue"), published("refund")}}
	caller := contract.ModuleDescriptor{ID: "sales.orders", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: offer.Name, Contract: offer.Contract, Required: true}}}
	providers, err := registeredKernelCommandProviders(moduleProviderSource{tenant: tenant, db: database, registry: registry, modules: []contract.ModuleDescriptor{caller, owner}, schemas: schemas}, commands, commandKernel)
	if err != nil {
		t.Fatal("register owner command provider:", err)
	}
	scope := contract.BusinessScope{TenantID: tenant, Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	for _, op := range []string{"issue", "refund"} {
		registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.finance.ar.operation." + op, Role: doctype.AdminRole, Write: true, Scope: scope})
	}
	modules := []contract.ModuleDescriptor{caller, owner}
	moduleStore := configstore.NewModuleStore(database, dialect)
	if err = moduleStore.EnsureSchema(); err != nil {
		t.Fatal("ensure module store:", err)
	}
	resolve := func(requested contract.BusinessScope) (plugin.BindingPlan, error) {
		plan, resolveErr := plugin.ResolveModules(requested, modules, providers, nil, 4)
		plan.LeaseAdmission = moduleStore
		return plan, resolveErr
	}
	snapshot := contract.ModuleSnapshot{TenantID: tenant, Modules: modules, Installations: []contract.ModuleInstallation{
		{TenantID: tenant, ModuleID: caller.ID, Version: caller.Version, ArtifactDigest: strings.Repeat("a", 64), State: string(contract.ModuleActive), Revision: 1},
		{TenantID: tenant, ModuleID: owner.ID, Version: owner.Version, ArtifactDigest: strings.Repeat("b", 64), State: string(contract.ModuleActive), Revision: 1},
	}}
	activation, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("begin module activation:", err)
	}
	if _, err = moduleStore.CommitActivation(ctx, activation, 0, snapshot); err != nil {
		_ = activation.Rollback()
		t.Fatal("persist module ownership:", err)
	}
	if err = activation.Commit(); err != nil {
		t.Fatal("commit module ownership:", err)
	}
	execution, err := policy.NewVerifiedExecutionWithGrants(tenant, contract.ActorContext{PrincipalID: "finance-operator", PrincipalType: contract.PrincipalHuman, Site: string(tenant), Roles: []string{doctype.AdminRole}, AuthenticatedAt: time.Now().UTC()}, []policy.Grant{{Resource: invoice.Name, Operation: "create", Scope: scope}})
	if err != nil {
		t.Fatal("create verified actor execution:", err)
	}
	runtime := publishedOperationRuntime{store: moduleStore, resolve: resolve, commandKernel: commandKernel, commandRegistry: commands, registry: registry}
	target := publishedOperationTarget{execution: execution, scope: scope, owner: owner.ID, published: owner.PublishedOperations[0]}
	const callers = 8
	start := make(chan struct{})
	var group sync.WaitGroup
	receipts := make([]contract.OperationReceipt, callers)
	errs := make([]error, callers)
	for i := range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			meta := contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey(fmt.Sprintf("lease-race-%d", i))}
			receipts[i], errs[i] = executePublishedOperation(ctx, runtime, target, publishedOperationInvocation{inputSchema: issue.InputSchema, meta: meta, input: []byte(`{"title":"race"}`)})
		}()
	}
	close(start)
	group.Wait()
	for i := range callers {
		if errs[i] != nil || receipts[i].Status != contract.ReceiptCompleted {
			t.Fatalf("concurrent first use %d = (%+v, %v), want a completed receipt", i, receipts[i], errs[i])
		}
	}
	var leases int
	query := kdb.Rebind(dialect, "SELECT COUNT(*) FROM _kora_capability_lease WHERE tenant_id = ?")
	if err = database.QueryRowContext(ctx, query, string(tenant)).Scan(&leases); err != nil || leases != 1 {
		t.Fatalf("durable lease rows = %d (%v), want exactly one shared lease", leases, err)
	}
	assertConcurrentEnsureSharesLease(ctx, t, runtime, database, dialect, tenant, scope, caller.ID, owner.ID, moduleStore, resolve)
}

// Each issuer stamps its own expiry, so the racers below differ in ExpiresAt
// yet declare the same grant: all must succeed and share one row.
func assertConcurrentEnsureSharesLease(ctx context.Context, t *testing.T, _ publishedOperationRuntime, database *sql.DB, dialect kdb.Dialect, tenant contract.TenantID, scope contract.BusinessScope, caller, owner contract.ModuleID, moduleStore *configstore.ModuleStore, resolve moduleBindingResolver) {
	t.Helper()
	plan, err := resolve(scope)
	if err != nil || len(plan.Bindings) == 0 {
		t.Fatal("resolve provider binding for direct issuance:", err)
	}
	const racers = 8
	errs := make([]error, racers)
	start := make(chan struct{})
	var group sync.WaitGroup
	for i := range racers {
		group.Add(1)
		go func() {
			defer group.Done()
			lease := contract.CapabilityLease{ID: "lease-direct-race", TenantID: tenant, Scope: scope, CallerModuleID: caller, ProviderModuleID: owner, ProviderID: plan.Bindings[0].ProviderID, Operation: "issue", BindingRevision: plan.Revision, GrantEpoch: 1, ExpiresAt: time.Now().UTC().Add(time.Duration(i+1) * time.Hour), State: "active"}
			<-start
			errs[i] = moduleStore.EnsureCapabilityLease(ctx, lease)
		}()
	}
	close(start)
	group.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent issuer %d = %v, want the shared lease", i, err)
		}
	}
	var rows int
	if err := database.QueryRowContext(ctx, kdb.Rebind(dialect, "SELECT COUNT(*) FROM _kora_capability_lease WHERE tenant_id = ? AND lease_id = ?"), string(tenant), "lease-direct-race").Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("direct lease rows = %d (%v), want exactly one", rows, err)
	}
}
