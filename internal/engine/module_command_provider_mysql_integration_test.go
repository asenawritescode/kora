//go:build integration

package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/plugin"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/schema"
	"github.com/asenawritescode/kora/site"
	_ "github.com/go-sql-driver/mysql"
)

type moduleCommandFixtureSchemas struct {
	input, output contract.SchemaRef
	rejectOutput  bool
}

func (s *moduleCommandFixtureSchemas) Registered(ref contract.SchemaRef) bool {
	return ref == s.input || ref == s.output
}

func (s *moduleCommandFixtureSchemas) Validate(ref contract.SchemaRef, raw json.RawMessage) error {
	if !json.Valid(raw) {
		return errors.New("invalid JSON")
	}
	if ref == s.input {
		var payload struct {
			Data struct {
				Title string `json:"title"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil || (payload.Data.Title != "issued invoice" && payload.Data.Title != "different invoice") {
			return errors.New("input does not match registered contract")
		}
	}
	if ref == s.output && s.rejectOutput {
		return errors.New("injected output schema rejection")
	}
	return nil
}

func TestKernelCommandCapabilityRunsSchemaAndBusinessEffectsInOneTransaction(t *testing.T) {
	dsn := os.Getenv("KORA_ENGINE_MODULE_COMMAND_DISPOSABLE_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_ENGINE_MODULE_COMMAND_DISPOSABLE_MYSQL_DSN to a fresh disposable MySQL database")
	}
	ctx := context.Background()
	database, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open disposable MySQL:", err)
	}
	database.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = database.Close() })
	if err := database.PingContext(ctx); err != nil {
		t.Fatal("ping disposable MySQL:", err)
	}
	dialect := kdb.Resolve("mysql")
	if err := site.BootstrapSystemTables(database, dialect); err != nil {
		t.Fatal("bootstrap kernel system tables:", err)
	}
	for _, statements := range [][]string{kdb.KernelTablesMySQL(), kdb.OutboxTablesMySQL()} {
		for _, statement := range statements {
			if _, err := database.ExecContext(ctx, statement); err != nil {
				t.Fatalf("create kernel support schema: %v\n%s", err, statement)
			}
		}
	}
	var suffix string
	if err := database.QueryRowContext(ctx, "SELECT REPLACE(UUID(), '-', '')").Scan(&suffix); err != nil {
		t.Fatal("create fixture identity:", err)
	}
	tenant := contract.TenantID("tenant-" + suffix)
	invoice := &doctype.DocType{Name: "ModuleInvoice" + suffix[:8], ModuleOwnerID: "finance.ar", ModuleOwnershipMode: string(contract.MutationCommands), BusinessScopePolicy: contract.ScopeEntityOwned, BusinessScopeField: "legal_entity_id", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data", Reqd: true}, {Fieldname: "legal_entity_id", Fieldtype: "Data", Reqd: true}}}
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{invoice}, []*doctype.Role{{Name: doctype.AdminRole}}, []*doctype.Permission{{Doctype: invoice.Name, Role: doctype.AdminRole, Read: true, Create: true}})
	var databaseName string
	if err := database.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&databaseName); err != nil {
		t.Fatal("read disposable database name:", err)
	}
	if err := schema.MigrateSiteFromRegistry(database, databaseName, registry, dialect); err != nil {
		t.Fatal("migrate module owner resource:", err)
	}
	command, err := kernel.ParseCommandResource([]byte("name: issue\nnamespace: finance.invoice\nversion: 1\ninput:\n  record: " + invoice.Name + "\nmodule_owner_id: finance.ar\ncapability: billing.invoice\noperation: issue\ninput_schema:\n  id: billing.invoice.issue.input\n  version: 1\noutput_schema:\n  id: billing.invoice.issue.output\n  version: 1\ntransaction:\n  - create:\n      record: " + invoice.Name + "\n      values:\n        title: $input.title\nemit:\n  - finance.invoice.issued\n"))
	if err != nil {
		t.Fatal("parse owner command:", err)
	}
	commands := kernel.NewCommandRegistry()
	if err := commands.Register(command); err != nil {
		t.Fatal("register owner command:", err)
	}
	commandKernel := kernel.New(dialect, outbox.NewSQLWriter(dialect))
	commandKernel.Commands = commands
	inputSchema, outputSchema := command.InputSchema, command.OutputSchema
	schemas := &moduleCommandFixtureSchemas{input: inputSchema, output: outputSchema, rejectOutput: true}
	commandKernel.CommandSchemas = schemas
	operation := contract.OperationSpec{Name: "issue", Input: inputSchema, Output: outputSchema, Effect: contract.EffectLocal}
	offer := contract.CapabilityOffer{Name: "billing.invoice", Contract: outputSchema, Operations: []contract.OperationSpec{operation}}
	owner := contract.ModuleDescriptor{ID: "finance.ar", Version: "1.0.0", Provides: []contract.CapabilityOffer{offer}}
	caller := contract.ModuleDescriptor{ID: "sales.orders", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: offer.Name, Contract: offer.Contract, Required: true}}}
	providers, err := registeredKernelCommandProviders(tenant, database, registry, []contract.ModuleDescriptor{caller, owner}, commands, commandKernel, schemas, nil)
	if err != nil {
		t.Fatal("register internal owner command provider:", err)
	}
	if len(providers) != 1 || providers[0].Kind != plugin.ProviderInternal {
		t.Fatalf("internal providers = %+v, want one internal owner provider", providers)
	}
	scope := contract.BusinessScope{TenantID: tenant, Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	plan, err := plugin.ResolveModules(scope, []contract.ModuleDescriptor{caller, owner}, providers, nil, 4)
	if err != nil {
		t.Fatal("resolve module binding plan:", err)
	}
	moduleStore := configstore.NewModuleStore(database, dialect)
	if err := moduleStore.EnsureSchema(); err != nil {
		t.Fatal("ensure capability lease store:", err)
	}
	moduleSnapshot := contract.ModuleSnapshot{TenantID: tenant, Modules: []contract.ModuleDescriptor{caller, owner}, Installations: []contract.ModuleInstallation{
		{TenantID: tenant, ModuleID: caller.ID, Version: caller.Version, ArtifactDigest: strings.Repeat("a", 64), State: string(contract.ModuleActive), Revision: 1},
		{TenantID: tenant, ModuleID: owner.ID, Version: owner.Version, ArtifactDigest: strings.Repeat("b", 64), State: string(contract.ModuleActive), Revision: 1},
	}}
	activationTx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("begin fixture module activation:", err)
	}
	if _, err := moduleStore.CommitActivation(ctx, activationTx, 0, moduleSnapshot); err != nil {
		_ = activationTx.Rollback()
		t.Fatal("persist fixture module ownership before testing protected command:", err)
	}
	if err := activationTx.Commit(); err != nil {
		t.Fatal("commit fixture module ownership:", err)
	}
	plan.LeaseAdmission = moduleStore
	lease := contract.CapabilityLease{ID: "lease-" + suffix[:16], TenantID: tenant, Scope: scope, CallerModuleID: caller.ID, ProviderModuleID: plan.Bindings[0].OwnerModuleID, ProviderID: plan.Bindings[0].ProviderID, Operation: operation.Name, BindingRevision: plan.Revision, GrantEpoch: 1, ExpiresAt: time.Now().UTC().Add(time.Hour), State: "active"}
	if err := moduleStore.CreateCapabilityLease(ctx, lease); err != nil {
		t.Fatal("create capability lease:", err)
	}
	execution, err := policy.NewVerifiedExecutionWithGrants(tenant, contract.ActorContext{PrincipalID: "finance-operator", PrincipalType: contract.PrincipalHuman, Site: string(tenant), Roles: []string{doctype.AdminRole}, AuthenticatedAt: time.Now().UTC()}, []policy.Grant{{Resource: invoice.Name, Operation: "create", Scope: scope}})
	if err != nil {
		t.Fatal("create verified actor execution:", err)
	}
	moduleExecution, err := contract.NewCommandModuleExecution(scope, caller.ID, owner.ID, 4, command.FullName(), command.FullName())
	if err != nil {
		t.Fatal("mint test owner command execution:", err)
	}
	directOperation := kernel.Operation{Command: command.FullName(), Payload: json.RawMessage(`{"data":{"title":"issued invoice"}}`), Context: kernel.OperationContext{
		ModuleExecution: &moduleExecution, BusinessScope: scope, Site: string(tenant), TenantID: tenant,
		Execution: execution, Actor: execution.Actor(), User: "finance-operator", Owner: "finance-operator",
		UserRole: doctype.AdminRole, Roles: []string{doctype.AdminRole}, Source: kernel.SourceWorker,
		IdempotencyKey: "direct-output-schema-" + suffix[:12],
	}}
	invalidInputOperation := directOperation
	invalidInputOperation.Payload = json.RawMessage(`{"title":"wrong input"}`)
	if _, commandErr := commandKernel.Execute(ctx, database, registry, invalidInputOperation); commandErr == nil || commandErr.Type != contract.CodeValidationFailed {
		t.Fatalf("direct owner command accepted input schema mismatch: %v", commandErr)
	}
	if _, commandErr := commandKernel.Execute(ctx, database, registry, directOperation); commandErr == nil || commandErr.Type != contract.CodeValidationFailed {
		t.Fatalf("direct owner command accepted output schema mismatch: %v", commandErr)
	}
	assertModuleCommandCounts(t, ctx, database, dialect, invoice, tenant, 0, 0, 0)
	meta := contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("issue-" + suffix[:12])}
	payload := json.RawMessage(`{"data":{"title":"issued invoice"}}`)
	if _, err := plan.InvokeCapability(ctx, execution, lease, scope, caller.ID, offer.Name, operation.Name, meta, payload); err == nil {
		t.Fatal("output-schema rejection unexpectedly committed a local command")
	}
	assertModuleCommandCounts(t, ctx, database, dialect, invoice, tenant, 0, 0, 0)
	schemas.rejectOutput = false
	otherScope := contract.BusinessScope{TenantID: tenant, Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-b"}
	otherPlan, err := plugin.ResolveModules(otherScope, []contract.ModuleDescriptor{caller, owner}, providers, nil, 4)
	if err != nil {
		t.Fatal("resolve other entity binding plan:", err)
	}
	otherPlan.LeaseAdmission = moduleStore
	otherLease := lease
	otherLease.ID = "lease-other-" + suffix[:12]
	otherLease.Scope = otherScope
	if err := moduleStore.CreateCapabilityLease(ctx, otherLease); err != nil {
		t.Fatal("create other-entity capability lease:", err)
	}
	if _, err := otherPlan.InvokeCapability(ctx, execution, otherLease, otherScope, caller.ID, offer.Name, operation.Name, contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("other-entity-" + suffix[:12])}, payload); err == nil {
		t.Fatal("entity-A actor grant authorized owner command in entity B")
	}
	assertModuleCommandCounts(t, ctx, database, dialect, invoice, tenant, 0, 0, 0)
	result, err := plan.InvokeCapability(ctx, execution, lease, scope, caller.ID, offer.Name, operation.Name, meta, payload)
	if err != nil {
		t.Fatal("invoke local owner command:", err)
	}
	if result.Status != "completed" || result.OutputSchema != outputSchema {
		t.Fatalf("local capability result = %+v", result)
	}
	assertModuleCommandCounts(t, ctx, database, dialect, invoice, tenant, 1, 1, 1)
	var legalEntityID string
	if err := database.QueryRowContext(ctx, "SELECT legal_entity_id FROM "+dialect.QuoteIdent(invoice.RawTableName())).Scan(&legalEntityID); err != nil {
		t.Fatal("read owner command business scope:", err)
	}
	if legalEntityID != scope.LegalEntityID {
		t.Fatalf("owner command row legal entity = %q, want actor scope %q", legalEntityID, scope.LegalEntityID)
	}
	var auditOperationID string
	if err := database.QueryRowContext(ctx, "SELECT operation_id FROM _kora_operation_audit WHERE site=? AND command_name=? AND status='completed'", string(tenant), command.FullName()).Scan(&auditOperationID); err != nil {
		t.Fatal("read owner command execution identity:", err)
	}
	if auditOperationID != string(result.ExecutionID) {
		t.Fatalf("capability execution ID %q differs from committed Kernel operation %q", result.ExecutionID, auditOperationID)
	}
	replayed, err := plan.InvokeCapability(ctx, execution, lease, scope, caller.ID, offer.Name, operation.Name, meta, payload)
	if err != nil || replayed.ExecutionID != result.ExecutionID {
		t.Fatalf("capability idempotency replay = %+v, err=%v; want original execution %q", replayed, err, result.ExecutionID)
	}
	assertModuleCommandCounts(t, ctx, database, dialect, invoice, tenant, 1, 1, 1)
	conflictingPayload := json.RawMessage(`{"data":{"title":"different invoice"}}`)
	_, err = plan.InvokeCapability(ctx, execution, lease, scope, caller.ID, offer.Name, operation.Name, meta, conflictingPayload)
	var conflict *contract.Error
	if !errors.As(err, &conflict) || conflict.Type != contract.CodeIdempotencyKeyReused {
		t.Fatalf("capability key reuse with a different payload = %v, want idempotency key reused", err)
	}
	assertModuleCommandCounts(t, ctx, database, dialect, invoice, tenant, 1, 1, 1)
	if err := moduleStore.RevokeCapabilityLease(ctx, lease, "security-admin", "provider credential compromised", time.Now().UTC()); err != nil {
		t.Fatal("revoke capability lease:", err)
	}
	revokedMeta := contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("revoked-" + suffix[:12])}
	reloadedStore := configstore.NewModuleStore(database, dialect)
	plan.LeaseAdmission = reloadedStore
	_, err = plan.InvokeCapability(ctx, execution, lease, scope, caller.ID, offer.Name, operation.Name, revokedMeta, payload)
	var denied *contract.Error
	if !errors.As(err, &denied) || denied.Type != contract.CodePermissionDenied {
		t.Fatalf("revoked capability invocation after store reload = %v, want permission denied", err)
	}
	assertModuleCommandCounts(t, ctx, database, dialect, invoice, tenant, 1, 1, 1)

	raceLease := lease
	raceLease.ID = "lease-race-" + suffix[:12]
	raceLease.State = "active"
	raceLease.GrantEpoch = 1
	raceLease.ExpiresAt = time.Now().UTC().Add(time.Hour)
	if err := reloadedStore.CreateCapabilityLease(ctx, raceLease); err != nil {
		t.Fatal("create concurrent invocation lease:", err)
	}
	raceMeta := contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("race-invoke-" + suffix[:12])}
	start := make(chan struct{})
	var group sync.WaitGroup
	group.Add(2)
	var invokeErr, revokeErr error
	go func() {
		defer group.Done()
		<-start
		_, invokeErr = plan.InvokeCapability(ctx, execution, raceLease, scope, caller.ID, offer.Name, operation.Name, raceMeta, payload)
	}()
	go func() {
		defer group.Done()
		<-start
		revokeErr = reloadedStore.RevokeCapabilityLease(ctx, raceLease, "security-admin", "concurrent invocation race", time.Now().UTC())
	}()
	close(start)
	group.Wait()
	if revokeErr != nil {
		t.Fatalf("revoke concurrent invocation lease: %v", revokeErr)
	}
	if invokeErr != nil {
		var invokeDenied *contract.Error
		if !errors.As(invokeErr, &invokeDenied) || invokeDenied.Type != contract.CodePermissionDenied {
			t.Fatalf("concurrent capability invocation = %v, want success or permission denied", invokeErr)
		}
	}
	postRevokeMeta := contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("post-revoke-" + suffix[:12])}
	_, err = plan.InvokeCapability(ctx, execution, raceLease, scope, caller.ID, offer.Name, operation.Name, postRevokeMeta, payload)
	if !errors.As(err, &denied) || denied.Type != contract.CodePermissionDenied {
		t.Fatalf("capability invocation after concurrent revoke committed = %v, want permission denied", err)
	}
	if invokeErr == nil {
		assertModuleCommandCounts(t, ctx, database, dialect, invoice, tenant, 2, 2, 2)
	} else {
		assertModuleCommandCounts(t, ctx, database, dialect, invoice, tenant, 1, 1, 1)
	}

	expiredLease := lease
	expiredLease.ID = "lease-expired-" + suffix[:12]
	expiredLease.State = "active"
	expiredLease.GrantEpoch = 1
	expiredLease.ExpiresAt = time.Now().UTC().Add(time.Hour)
	if err := reloadedStore.CreateCapabilityLease(ctx, expiredLease); err != nil {
		t.Fatal("create expired capability lease:", err)
	}
	expiredLease.ExpiresAt = time.Now().UTC().Add(-time.Second)
	if _, err := database.ExecContext(ctx, kdb.Rebind(dialect, `UPDATE _kora_capability_lease SET expires_at=? WHERE tenant_id=? AND lease_id=?`), expiredLease.ExpiresAt.Format("2006-01-02T15:04:05.000000000Z"), string(expiredLease.TenantID), expiredLease.ID); err != nil {
		t.Fatal("expire capability lease fixture:", err)
	}
	expiredMeta := contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("expired-" + suffix[:12])}
	_, err = plan.InvokeCapability(ctx, execution, expiredLease, scope, caller.ID, offer.Name, operation.Name, expiredMeta, payload)
	if !errors.As(err, &denied) || denied.Type != contract.CodePermissionDenied {
		t.Fatalf("expired capability invocation = %v, want permission denied", err)
	}
	currentExpired, found, loadErr := reloadedStore.LoadCapabilityLease(ctx, expiredLease.TenantID, expiredLease.ID)
	if loadErr != nil || !found || currentExpired.State != "expired" {
		t.Fatalf("expired lease state = (%+v,%t,%v), want durable expired state", currentExpired, found, loadErr)
	}
	if invokeErr == nil {
		assertModuleCommandCounts(t, ctx, database, dialect, invoice, tenant, 2, 2, 2)
	} else {
		assertModuleCommandCounts(t, ctx, database, dialect, invoice, tenant, 1, 1, 1)
	}
}

func assertModuleCommandCounts(t *testing.T, ctx context.Context, database *sql.DB, dialect kdb.Dialect, invoice *doctype.DocType, tenant contract.TenantID, wantRecords, wantEvents, wantAudits int) {
	t.Helper()
	var records, events, audits int
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(invoice.RawTableName())).Scan(&records); err != nil {
		t.Fatal("count owner command records:", err)
	}
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM _kora_outbox WHERE site=? AND event_type='finance.invoice.issued'", string(tenant)).Scan(&events); err != nil {
		t.Fatal("count owner command outbox events:", err)
	}
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM _kora_operation_audit WHERE site=? AND command_name='finance.invoice.issue' AND status='completed'", string(tenant)).Scan(&audits); err != nil {
		t.Fatal("count owner command audits:", err)
	}
	if records != wantRecords || events != wantEvents || audits != wantAudits {
		t.Fatalf("owner command effects (records=%d events=%d audits=%d), want (%d,%d,%d)", records, events, audits, wantRecords, wantEvents, wantAudits)
	}
}
