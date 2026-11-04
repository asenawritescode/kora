//go:build integration

package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asenawritescode/kora/api"
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
	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
)

type moduleCommandFixtureSchemas struct {
	input, output contract.SchemaRef
	rejectOutput  bool
}

type publishedOperationExternalFactory struct {
	capability contract.CapabilityName
	handle     *publishedOperationExternalHandle
}

func (f publishedOperationExternalFactory) Capability() contract.CapabilityName { return f.capability }
func (f publishedOperationExternalFactory) Open(contract.OperationContext) (contract.CapabilityHandle, error) {
	return f.handle, nil
}

type publishedOperationExternalHandle struct {
	mu              sync.Mutex
	capability      contract.CapabilityName
	calls           int
	reconciliations int
	lastInvocation  plugin.CapabilityInvocation
	revoked         bool
	started         chan struct{}
	release         chan struct{}
}

func (h *publishedOperationExternalHandle) Name() contract.CapabilityName { return h.capability }
func (h *publishedOperationExternalHandle) Scope() contract.GrantScope {
	return contract.GrantScopeOperation
}
func (h *publishedOperationExternalHandle) Revoke() { h.revoked = true }
func (h *publishedOperationExternalHandle) Evidence() contract.CleanupEvidence {
	return contract.CleanupEvidence{}
}
func (h *publishedOperationExternalHandle) InvokeCapability(_ context.Context, invocation plugin.CapabilityInvocation) (plugin.CapabilityInvocationResult, error) {
	h.mu.Lock()
	h.calls++
	h.lastInvocation = invocation
	started, release := h.started, h.release
	h.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	return plugin.CapabilityInvocationResult{ExecutionID: "external-provider-execution", Output: json.RawMessage(`{"settled":true}`)}, errors.New("provider response was lost")
}
func (h *publishedOperationExternalHandle) ReconcileCapability(_ context.Context, pin contract.PinnedDispatch, _ contract.CapabilityLease) (contract.ExternalOutcome, plugin.CapabilityInvocationResult, error) {
	h.mu.Lock()
	h.reconciliations++
	lastInvocation := h.lastInvocation
	h.mu.Unlock()
	if pin.IdempotencyKey != lastInvocation.Meta.IdempotencyKey || pin.InputHash != configstore.PublishedOperationInputHash(lastInvocation.Payload) {
		return contract.ExternalUnknown, plugin.CapabilityInvocationResult{}, errors.New("reconciliation did not use the original intent")
	}
	return contract.ExternalApplied, plugin.CapabilityInvocationResult{ExecutionID: "external-provider-execution", Output: json.RawMessage(`{"settled":true}`)}, nil
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
	rootConfig, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse disposable MySQL DSN:", err)
	}
	rootConfig.DBName = ""
	root, err := sql.Open("mysql", rootConfig.FormatDSN())
	if err != nil {
		t.Fatal("open disposable MySQL server:", err)
	}
	if err := root.PingContext(ctx); err != nil {
		_ = root.Close()
		t.Fatal("ping disposable MySQL server:", err)
	}
	var databaseSuffix string
	if err := root.QueryRowContext(ctx, "SELECT REPLACE(UUID(), '-', '')").Scan(&databaseSuffix); err != nil {
		_ = root.Close()
		t.Fatal("create disposable database identity:", err)
	}
	databaseName := "kora_module_command_" + databaseSuffix
	if _, err := root.ExecContext(ctx, "CREATE DATABASE `"+databaseName+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = root.Close()
		t.Fatal("create isolated module command database:", err)
	}
	t.Cleanup(func() { _, _ = root.ExecContext(context.Background(), "DROP DATABASE IF EXISTS `"+databaseName+"`"); _ = root.Close() })
	tenantConfig := *rootConfig
	tenantConfig.DBName = databaseName
	database, err := sql.Open("mysql", tenantConfig.FormatDSN())
	if err != nil {
		t.Fatal("open isolated module command database:", err)
	}
	database.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = database.Close() })
	if err := database.PingContext(ctx); err != nil {
		t.Fatal("ping isolated module command database:", err)
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
	registry.LoadFull([]*doctype.DocType{invoice}, []*doctype.Role{{Name: doctype.AdminRole}}, []*doctype.Permission{{Doctype: invoice.Name, Role: doctype.AdminRole, Read: true, Create: true}, {Doctype: "module.finance.ar.operation.issue", Role: doctype.AdminRole, Write: true}})
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
	owner := contract.ModuleDescriptor{ID: "finance.ar", Version: "1.0.0", Provides: []contract.CapabilityOffer{offer}, PublishedOperations: []contract.PublishedOperation{{Name: "issue", Capability: offer.Name, Operation: operation.Name, ScopePolicy: contract.ScopeEntityOwned, OfflineEligible: true, AllowedCallers: []contract.ModuleID{"sales.orders"}}}}
	caller := contract.ModuleDescriptor{ID: "sales.orders", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: offer.Name, Contract: offer.Contract, Required: true}}}
	providers, err := registeredKernelCommandProviders(tenant, database, registry, []contract.ModuleDescriptor{caller, owner}, commands, commandKernel, schemas, nil)
	if err != nil {
		t.Fatal("register internal owner command provider:", err)
	}
	if len(providers) != 1 || providers[0].Kind != plugin.ProviderInternal {
		t.Fatalf("internal providers = %+v, want one internal owner provider", providers)
	}
	scope := contract.BusinessScope{TenantID: tenant, Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.finance.ar.operation.issue", Role: doctype.AdminRole, Write: true, Scope: scope})
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
	publicMeta := contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("public-" + suffix[:12])}
	resolvePublicPlan := func(requested contract.BusinessScope) (plugin.BindingPlan, error) {
		resolved, resolveErr := plugin.ResolveModules(requested, []contract.ModuleDescriptor{caller, owner}, providers, nil, 4)
		resolved.LeaseAdmission = reloadedStore
		return resolved, resolveErr
	}
	staleMeta := contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("public-stale-" + suffix[:12]), ExpectedRevision: "0"}
	_, staleErr := executePublishedOperation(ctx, reloadedStore, resolvePublicPlan, commandKernel, commands, registry, execution, scope, owner.ID, owner.PublishedOperations[0], inputSchema, staleMeta, payload)
	var staleContractErr *contract.Error
	if !errors.As(staleErr, &staleContractErr) || staleContractErr.Type != contract.CodeStaleSubject {
		t.Fatalf("public operation with stale catalog revision = %v, want typed stale revision conflict", staleErr)
	}
	publicReceipt, publicErr := executePublishedOperation(ctx, reloadedStore, resolvePublicPlan, commandKernel, commands, registry, execution, scope, owner.ID, owner.PublishedOperations[0], inputSchema, publicMeta, payload)
	if publicErr != nil || publicReceipt.Status != contract.ReceiptCompleted {
		t.Fatalf("public registered operation = (%+v,%v)", publicReceipt, publicErr)
	}
	publicReplay, publicErr := executePublishedOperation(ctx, reloadedStore, resolvePublicPlan, commandKernel, commands, registry, execution, scope, owner.ID, owner.PublishedOperations[0], inputSchema, publicMeta, payload)
	if publicErr != nil || publicReplay.ExecutionID != publicReceipt.ExecutionID {
		t.Fatalf("public registered operation retry = (%+v,%v), want original %s", publicReplay, publicErr, publicReceipt.ExecutionID)
	}
	beforeHTTPRecords, beforeHTTPEvents, beforeHTTPAudits := moduleCommandCounts(t, ctx, database, dialect, invoice, tenant)
	// Exercise the actual offline-intents HTTP boundary against the same durable
	// SQL receipt executor used by tenant runtime assembly. Replaying the
	// already committed idempotency key must return the original receipt and
	// must not apply the owner command a second time.
	moduleSnapshot, err = reloadedStore.Load(tenant)
	if err != nil {
		t.Fatal("load module snapshot for offline HTTP retry:", err)
	}
	runtimeServices := api.NewSiteRuntimeServices()
	runtimeServices.Replace(string(tenant), api.SiteRuntimeService{
		LoadModuleSnapshot: func() (contract.ModuleSnapshot, error) { return moduleSnapshot, nil },
		ExecutePublishedOperation: func(requestCtx context.Context, actor policy.Execution, businessScope contract.BusinessScope, moduleOwner contract.ModuleID, publishedOperation contract.PublishedOperation, schemaRef contract.SchemaRef, meta contract.CommandMeta, inputJSON []byte) (contract.OperationReceipt, error) {
			return executePublishedOperation(requestCtx, reloadedStore, resolvePublicPlan, commandKernel, commands, registry, actor, businessScope, moduleOwner, publishedOperation, schemaRef, meta, inputJSON)
		},
	})
	handler := api.NewHandler(registry, nil)
	handler.RuntimeServices = runtimeServices
	intent := contract.OfflineIntent{
		LocalOperationID: "offline-retry-" + suffix[:12], BusinessScope: scope,
		OperationRef: contract.PublishedOperationRef(owner.ID, offer.Name, operation.Name),
		InputSchema:  inputSchema, Input: payload, IdempotencyKey: publicMeta.IdempotencyKey,
		CatalogRevision: moduleSnapshot.Revision,
	}
	requestBody, err := json.Marshal(contract.OfflineIntentsRequest{Intents: []contract.OfflineIntent{intent}})
	if err != nil {
		t.Fatal("encode offline intent request:", err)
	}
	gin.SetMode(gin.TestMode)
	response := httptest.NewRecorder()
	requestContext, _ := gin.CreateTestContext(response)
	requestContext.Request = httptest.NewRequest("POST", "/api/v2/offline/intents", strings.NewReader(string(requestBody)))
	requestContext.Request.Header.Set("X-Kora-Legal-Entity-ID", scope.LegalEntityID)
	requestContext.Set("site_name", string(tenant))
	requestContext.Set("policy_execution", execution)
	handler.HandleOfflineIntentsV2(requestContext)
	if response.Code != 200 {
		t.Fatalf("offline HTTP retry status=%d body=%s", response.Code, response.Body.String())
	}
	var offlineResponse struct {
		Data contract.OfflineIntentsResponse `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &offlineResponse); err != nil {
		t.Fatal("decode offline HTTP retry response:", err)
	}
	if len(offlineResponse.Data.Receipts) != 1 || offlineResponse.Data.Receipts[0].LocalOperationID != intent.LocalOperationID || offlineResponse.Data.Receipts[0].Receipt.ExecutionID != publicReceipt.ExecutionID {
		t.Fatalf("offline HTTP retry did not preserve durable receipt identity: %+v; original=%+v", offlineResponse.Data, publicReceipt)
	}
	afterHTTPRecords, afterHTTPEvents, afterHTTPAudits := moduleCommandCounts(t, ctx, database, dialect, invoice, tenant)
	if afterHTTPRecords != beforeHTTPRecords || afterHTTPEvents != beforeHTTPEvents || afterHTTPAudits != beforeHTTPAudits {
		t.Fatalf("offline idempotent HTTP retry changed committed command effects from (%d,%d,%d) to (%d,%d,%d)", beforeHTTPRecords, beforeHTTPEvents, beforeHTTPAudits, afterHTTPRecords, afterHTTPEvents, afterHTTPAudits)
	}
	publicLookup, foundPublic, lookupErr := lookupPublishedOperation(ctx, reloadedStore, commandKernel, commands, resolvePublicPlan, execution, scope, owner.ID, owner.PublishedOperations[0], publicMeta.IdempotencyKey)
	if lookupErr != nil || !foundPublic || publicLookup.ExecutionID != publicReceipt.ExecutionID {
		t.Fatalf("public operation lookup = (%+v,%v,%v), want original %s", publicLookup, foundPublic, lookupErr, publicReceipt.ExecutionID)
	}
	byID, foundPublic, lookupErr := reloadedStore.LoadPublishedOperationReceipt(ctx, tenant, publicReceipt.ExecutionID, execution.Actor(), scope)
	if lookupErr != nil || !foundPublic || byID.OperationRef != publicReceipt.OperationRef {
		t.Fatalf("public receipt by execution ID = (%+v,%v,%v)", byID, foundPublic, lookupErr)
	}
	otherActor, err := policy.NewVerifiedExecutionWithGrants(tenant, contract.ActorContext{PrincipalID: "other-operator", PrincipalType: contract.PrincipalHuman, Site: string(tenant), Roles: []string{doctype.AdminRole}, AuthenticatedAt: time.Now().UTC()}, []policy.Grant{{Resource: "module.finance.ar.operation.issue", Operation: "write", Scope: scope}})
	if err != nil {
		t.Fatal("create second public operation reader:", err)
	}
	if _, otherFound, otherErr := reloadedStore.LoadPublishedOperationReceipt(ctx, tenant, publicReceipt.ExecutionID, otherActor.Actor(), scope); otherErr != nil || otherFound {
		t.Fatalf("other actor receipt lookup = (found %v, err %v), want hidden", otherFound, otherErr)
	}
	wrongScope := scope
	wrongScope.LegalEntityID = "entity-other"
	if _, scopeFound, scopeErr := reloadedStore.LoadPublishedOperationReceipt(ctx, tenant, publicReceipt.ExecutionID, execution.Actor(), wrongScope); scopeErr == nil || scopeFound {
		t.Fatalf("cross-entity receipt lookup = (found %v, err %v), want denied", scopeFound, scopeErr)
	}
	publicIntentKey, intentErr := contract.PublishedOperationIntentKey(scope, publicReceipt.OperationRef, publicMeta.IdempotencyKey)
	if intentErr != nil {
		t.Fatal("derive public operation intent key:", intentErr)
	}
	publicAttempt, foundPublic, lookupErr := reloadedStore.LoadPublishedOperationAttempt(ctx, tenant, strings.TrimPrefix(string(publicIntentKey), "published:"), execution.Actor(), scope)
	if lookupErr != nil || !foundPublic || publicAttempt.Lease == nil {
		t.Fatalf("load public operation lease = (%+v,%v,%v)", publicAttempt, foundPublic, lookupErr)
	}
	if err := reloadedStore.RevokeCapabilityLease(ctx, *publicAttempt.Lease, "security-admin", "public operation grant revoked", time.Now().UTC()); err != nil {
		t.Fatal("revoke public operation lease:", err)
	}
	newMeta := contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("after-revoke-" + suffix[:12])}
	_, revokeErr = executePublishedOperation(ctx, reloadedStore, resolvePublicPlan, commandKernel, commands, registry, execution, scope, owner.ID, owner.PublishedOperations[0], inputSchema, newMeta, payload)
	var revokeContractErr *contract.Error
	if !errors.As(revokeErr, &revokeContractErr) || revokeContractErr.Type != contract.CodePermissionDenied {
		t.Fatalf("new public operation after grant revocation = %v, want permission denied", revokeErr)
	}

	// A provider response can be lost after its business effect. The public
	// intent and pin survive that response loss; retry reconciles the original
	// provider and does not dispatch a second effect.
	externalScope := contract.BusinessScope{TenantID: tenant, Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-external"}
	externalInput := contract.SchemaRef{ID: "billing.external.input", Version: 1}
	externalOutput := contract.SchemaRef{ID: "billing.external.output", Version: 1}
	externalCapability := contract.CapabilityName("billing.external")
	externalOperation := contract.OperationSpec{Name: "settle", Input: externalInput, Output: externalOutput, Effect: contract.EffectExternal}
	externalOffer := contract.CapabilityOffer{Name: externalCapability, Contract: externalOutput, Operations: []contract.OperationSpec{externalOperation}}
	externalOwner := contract.ModuleDescriptor{ID: "finance.external", Version: "1.0.0", Provides: []contract.CapabilityOffer{externalOffer}, PublishedOperations: []contract.PublishedOperation{{Name: "settle", Capability: externalCapability, Operation: "settle", ScopePolicy: contract.ScopeEntityOwned, AllowedCallers: []contract.ModuleID{"sales.external"}}}}
	externalCaller := contract.ModuleDescriptor{ID: "sales.external", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: externalCapability, Contract: externalOutput, Required: true}}}
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.finance.external.operation.settle", Role: doctype.AdminRole, Write: true, Scope: externalScope})
	currentSnapshot, err := reloadedStore.Load(tenant)
	if err != nil {
		t.Fatal("load current module snapshot for external fixture:", err)
	}
	currentSnapshot.Modules = append(currentSnapshot.Modules, externalCaller, externalOwner)
	currentSnapshot.Installations = append(currentSnapshot.Installations,
		contract.ModuleInstallation{TenantID: tenant, ModuleID: externalCaller.ID, Version: externalCaller.Version, ArtifactDigest: strings.Repeat("c", 64), State: string(contract.ModuleActive), Revision: 1},
		contract.ModuleInstallation{TenantID: tenant, ModuleID: externalOwner.ID, Version: externalOwner.Version, ArtifactDigest: strings.Repeat("d", 64), State: string(contract.ModuleActive), Revision: 1},
	)
	externalTx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("begin external fixture module activation:", err)
	}
	if _, err := reloadedStore.CommitActivation(ctx, externalTx, currentSnapshot.Revision, currentSnapshot); err != nil {
		_ = externalTx.Rollback()
		t.Fatal("persist external fixture module ownership:", err)
	}
	if err := externalTx.Commit(); err != nil {
		t.Fatal("commit external fixture module ownership:", err)
	}
	externalHandle := &publishedOperationExternalHandle{capability: externalCapability}
	externalProvider := plugin.ProviderRegistration{TenantID: tenant, ProviderID: "billing-external-provider", OwnerModuleID: externalOwner.ID, Kind: plugin.ProviderExternal, Offers: []contract.CapabilityOffer{externalOffer}, Factories: map[contract.CapabilityName]plugin.ProviderFactory{externalCapability: publishedOperationExternalFactory{capability: externalCapability, handle: externalHandle}}, ValidateSchema: func(_ contract.SchemaRef, raw []byte) error {
		if !json.Valid(raw) {
			return errors.New("invalid JSON")
		}
		return nil
	}}
	externalResolver := func(requested contract.BusinessScope) (plugin.BindingPlan, error) {
		plan, resolveErr := plugin.ResolveModules(requested, []contract.ModuleDescriptor{externalCaller, externalOwner}, []plugin.ProviderRegistration{externalProvider}, nil, currentSnapshot.Revision)
		plan.LeaseAdmission = reloadedStore
		return plan, resolveErr
	}
	externalExecution, err := policy.NewVerifiedExecutionWithGrants(tenant, execution.Actor(), []policy.Grant{{Resource: "module.finance.external.operation.settle", Operation: "write", Scope: externalScope}})
	if err != nil {
		t.Fatal("create external operation actor:", err)
	}
	externalMeta := contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("external-settle-" + suffix[:12])}
	externalInputJSON := json.RawMessage(`{"invoice_id":"INV-EXT-1"}`)
	unknown, err := executePublishedOperation(ctx, reloadedStore, externalResolver, commandKernel, commands, registry, externalExecution, externalScope, externalOwner.ID, externalOwner.PublishedOperations[0], externalInput, externalMeta, externalInputJSON)
	if err != nil || unknown.Status != contract.ReceiptOutcomeUnknown || externalHandle.calls != 1 {
		t.Fatalf("lost external response = (%+v,%v), provider calls=%d", unknown, err, externalHandle.calls)
	}
	recovered, err := executePublishedOperation(ctx, reloadedStore, externalResolver, commandKernel, commands, registry, externalExecution, externalScope, externalOwner.ID, externalOwner.PublishedOperations[0], externalInput, externalMeta, externalInputJSON)
	if err != nil || recovered.Status != contract.ReceiptCompleted || recovered.ExecutionID != unknown.ExecutionID || externalHandle.calls != 1 || externalHandle.reconciliations != 1 {
		t.Fatalf("external retry recovery = (%+v,%v), calls=%d reconciliations=%d", recovered, err, externalHandle.calls, externalHandle.reconciliations)
	}
	recoveredByKey, found, err := lookupPublishedOperation(ctx, reloadedStore, commandKernel, commands, externalResolver, externalExecution, externalScope, externalOwner.ID, externalOwner.PublishedOperations[0], externalMeta.IdempotencyKey)
	if err != nil || !found || recoveredByKey.Status != contract.ReceiptCompleted || recoveredByKey.ExecutionID != unknown.ExecutionID {
		t.Fatalf("external recovery lookup=(%+v,%v,%v)", recoveredByKey, found, err)
	}

	// Two concurrent requests for a fresh external intent must not both
	// dispatch. The duplicate observes the durable running receipt while the
	// sole claimant is inside the provider call.
	externalHandle.started, externalHandle.release = make(chan struct{}, 1), make(chan struct{})
	concurrentMeta := contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("external-race-" + suffix[:12])}
	firstResult := make(chan contract.OperationReceipt, 1)
	firstErr := make(chan error, 1)
	go func() {
		receipt, callErr := executePublishedOperation(ctx, reloadedStore, externalResolver, commandKernel, commands, registry, externalExecution, externalScope, externalOwner.ID, externalOwner.PublishedOperations[0], externalInput, concurrentMeta, externalInputJSON)
		firstResult <- receipt
		firstErr <- callErr
	}()
	select {
	case <-externalHandle.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first external intent did not reach provider")
	}
	duplicate, duplicateErr := executePublishedOperation(ctx, reloadedStore, externalResolver, commandKernel, commands, registry, externalExecution, externalScope, externalOwner.ID, externalOwner.PublishedOperations[0], externalInput, concurrentMeta, externalInputJSON)
	if duplicateErr != nil || duplicate.Status != contract.ReceiptRunning {
		t.Fatalf("concurrent duplicate = (%+v,%v), want running receipt", duplicate, duplicateErr)
	}
	close(externalHandle.release)
	firstReceipt := <-firstResult
	if callErr := <-firstErr; callErr != nil || firstReceipt.Status != contract.ReceiptOutcomeUnknown {
		t.Fatalf("first external claimant = (%+v,%v), want outcome_unknown", firstReceipt, callErr)
	}
	externalHandle.mu.Lock()
	concurrentCalls := externalHandle.calls
	externalHandle.started, externalHandle.release = nil, nil
	externalHandle.mu.Unlock()
	if concurrentCalls != 2 { // one completed intent above plus this one claimant
		t.Fatalf("external provider dispatches=%d, want 2 total", concurrentCalls)
	}
}

func assertModuleCommandCounts(t *testing.T, ctx context.Context, database *sql.DB, dialect kdb.Dialect, invoice *doctype.DocType, tenant contract.TenantID, wantRecords, wantEvents, wantAudits int) {
	t.Helper()
	records, events, audits := moduleCommandCounts(t, ctx, database, dialect, invoice, tenant)
	if records != wantRecords || events != wantEvents || audits != wantAudits {
		t.Fatalf("owner command effects (records=%d events=%d audits=%d), want (%d,%d,%d)", records, events, audits, wantRecords, wantEvents, wantAudits)
	}
}

func moduleCommandCounts(t *testing.T, ctx context.Context, database *sql.DB, dialect kdb.Dialect, invoice *doctype.DocType, tenant contract.TenantID) (int, int, int) {
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
	return records, events, audits
}
