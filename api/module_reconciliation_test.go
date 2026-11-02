package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/plugin"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/recon"
	"github.com/gin-gonic/gin"
)

type reconciliationQueryExecutor struct{ rows json.RawMessage }

func (e reconciliationQueryExecutor) ExecutePublishedQuery(context.Context, contract.ModuleExecution, contract.PublishedQuery, json.RawMessage, string, uint16) (contract.QueryResult, error) {
	return contract.QueryResult{Data: append(json.RawMessage(nil), e.rows...), SnapshotToken: "snapshot-1", SourceRevision: "revision-1", AsOf: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC), Freshness: contract.QueryFresh}, nil
}

type reconciliationRunCapture struct{ run recon.BusinessRun }

func (r *reconciliationRunCapture) Save(_ context.Context, run recon.BusinessRun) error {
	r.run = run
	return nil
}

type reconciliationCapabilityFactory struct {
	handle *reconciliationCapabilityHandle
}

func (f reconciliationCapabilityFactory) Capability() contract.CapabilityName { return f.handle.name }
func (f reconciliationCapabilityFactory) Open(contract.OperationContext) (contract.CapabilityHandle, error) {
	return f.handle, nil
}

type reconciliationCapabilityHandle struct {
	name       contract.CapabilityName
	calls      int
	revoked    bool
	invocation plugin.CapabilityInvocation
}

func (h *reconciliationCapabilityHandle) Name() contract.CapabilityName { return h.name }
func (h *reconciliationCapabilityHandle) Scope() contract.GrantScope {
	return contract.GrantScopeOperation
}
func (h *reconciliationCapabilityHandle) Revoke() { h.revoked = true }
func (h *reconciliationCapabilityHandle) Evidence() contract.CleanupEvidence {
	return contract.CleanupEvidence{}
}
func (h *reconciliationCapabilityHandle) InvokeCapability(_ context.Context, invocation plugin.CapabilityInvocation) (plugin.CapabilityInvocationResult, error) {
	h.calls++
	h.invocation = invocation
	return plugin.CapabilityInvocationResult{ExecutionID: "owner-execution-1", Output: json.RawMessage(`{}`)}, nil
}

type reconciliationLeaseAdmission struct{ calls int }

func (a *reconciliationLeaseAdmission) AdmitCapabilityAttempt(context.Context, contract.CapabilityLease, time.Time) error {
	a.calls++
	return nil
}

func TestBusinessReconciliationSummaryIsAuthorizedAndScopeFiltered(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	registry := doctype.NewRegistry()
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	resource := "module.business_reconciliation.findings"
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: resource, Role: "Reader", Read: true, Scope: scope})
	execution, err := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: "reader", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", Roles: []string{"Reader"}})
	if err != nil {
		t.Fatal(err)
	}
	scopeJSON, _ := json.Marshal(scope)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM _kora_business_recon_finding f JOIN _kora_business_recon_run r ON r.tenant_id=f.tenant_id AND r.run_id=f.run_id WHERE f.tenant_id = ? AND r.scope_json = ? AND f.state IN (?, ?)")).WithArgs("tenant-a", string(scopeJSON), "open", "investigating").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	handler := NewHandler(registry, nil)
	handler.RuntimeServices = NewSiteRuntimeServices()
	handler.RuntimeServices.Replace("tenant-a", SiteRuntimeService{BusinessReconciliation: &recon.BusinessReconciliationService{Store: &recon.BusinessRunStore{DB: database, Dialect: &db.MySQLDialect{}}}})
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("GET", "/api/modules/reconciliation/findings", nil)
	ctx.Set("site_name", "tenant-a")
	ctx.Set("policy_execution", execution)
	ctx.Request.Header.Set("X-Kora-Legal-Entity-ID", "entity-a")
	handler.HandleBusinessReconciliationSummary(ctx)
	if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), `"unresolved_business_findings":2`) {
		t.Fatalf("authorized business finding summary = %d %s", recorder.Code, recorder.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBusinessFindingSummaryAuthorizationRejectsWrongEntity(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-b"}
	execution, err := policy.NewVerifiedExecutionWithGrants("tenant-a", contract.ActorContext{PrincipalID: "reader", PrincipalType: contract.PrincipalService, Site: "tenant-a"}, []policy.Grant{{Resource: "module.business_reconciliation.findings", Operation: "read", Scope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}}})
	if err != nil {
		t.Fatal(err)
	}
	err = businessFindingSummaryAuthorization{execution: execution, roles: doctype.NewRegistry()}.CanInspectBusinessFindings(context.Background(), scope, "reader")
	if err == nil {
		t.Fatal("entity grant crossed legal entity boundary")
	}
}

func TestBusinessReconciliationRunUsesOwnerRegisteredRuleAndPublishedQueries(t *testing.T) {
	_, plan, registry, execution := reconciliationAPIFixture(t, json.RawMessage(`[{"key":"sale:1","source_ref":"sale:1","entity_id":"entity-a","amount":"10.00","currency":"KES","occurred_at":"2026-11-02T10:00:00Z"}]`), json.RawMessage(`[]`), true)
	captured := &reconciliationRunCapture{}
	service := &recon.BusinessReconciliationService{Runs: captured}
	handler := NewHandler(registry, nil)
	handler.RuntimeServices = NewSiteRuntimeServices()
	handler.RuntimeServices.Replace("tenant-a", SiteRuntimeService{BusinessReconciliation: service, ResolveModuleBindings: func(contract.BusinessScope) (plugin.BindingPlan, error) { return plan, nil }})
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("POST", "/api/modules/reconciliation/runs", strings.NewReader(`{"owner_module_id":"finance.reconciliation","rule_id":"sales-ar","run_id":"run-http-1"}`))
	ctx.Set("site_name", "tenant-a")
	ctx.Set("policy_execution", execution)
	handler.HandleBusinessReconciliationRun(ctx)
	if recorder.Code != 200 || captured.run.ID != "run-http-1" || !captured.run.Complete || len(captured.run.Findings) != 1 || captured.run.Findings[0].Kind != recon.FindingMissingEffect {
		t.Fatalf("owner registered reconciliation run = status %d run %+v response=%s", recorder.Code, captured.run, recorder.Body.String())
	}
}

func reconciliationAPIFixture(t *testing.T, sourceRows, targetRows json.RawMessage, report bool) (contract.BusinessScope, plugin.BindingPlan, *doctype.Registry, policy.Execution) {
	t.Helper()
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	input, output := contract.SchemaRef{ID: "recon.input", Version: 1}, contract.SchemaRef{ID: "recon.output", Version: 1}
	queryModule := func(owner, queryName, capability string, fields []string, rows json.RawMessage) (contract.ModuleDescriptor, plugin.ProviderRegistration) {
		operation := contract.OperationSpec{Name: "list", Input: input, Output: output, Effect: contract.EffectQuery}
		offer := contract.CapabilityOffer{Name: contract.CapabilityName(capability), Contract: output, Operations: []contract.OperationSpec{operation}}
		query := contract.PublishedQuery{OwnerModuleID: contract.ModuleID(owner), Name: queryName, Capability: offer.Name, Operation: operation.Name, InputSchema: input, OutputSchema: output, ScopePolicy: contract.ScopeTenantShared, AllowedFields: fields, PageLimit: 10}
		module := contract.ModuleDescriptor{ID: contract.ModuleID(owner), Version: "1.0.0", Provides: []contract.CapabilityOffer{offer}, Queries: []contract.PublishedQuery{query}}
		provider := plugin.ProviderRegistration{TenantID: scope.TenantID, ProviderID: owner + "-provider", OwnerModuleID: module.ID, Kind: plugin.ProviderInternal, Offers: []contract.CapabilityOffer{offer}, Queries: map[string]plugin.PublishedQueryExecutor{queryName: reconciliationQueryExecutor{rows: rows}}, ValidateSchema: func(contract.SchemaRef, []byte) error { return nil }}
		return module, provider
	}
	source, sourceProvider := queryModule("sales.orders", "billable", "sales.reconcile", []string{"key", "source_ref", "entity_id", "amount", "currency", "occurred_at"}, sourceRows)
	target, targetProvider := queryModule("accounts.receivable", "invoices", "ar.reconcile", []string{"key", "target_ref", "entity_id", "amount", "currency", "occurred_at"}, targetRows)
	repairOffer := contract.CapabilityOffer{Name: "ar.repair", Contract: output, Operations: []contract.OperationSpec{{Name: "issue", Input: input, Output: output, Effect: contract.EffectLocal}}}
	target.Provides = append(target.Provides, repairOffer)
	targetProvider.Offers = append(targetProvider.Offers, repairOffer)
	targetProvider.Factories = map[contract.CapabilityName]plugin.ProviderFactory{"ar.repair": plugin.NewStaticFactory("ar.repair", nil)}
	owner := contract.ModuleDescriptor{ID: "finance.reconciliation", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: "ar.repair", Contract: output, Required: true}}}
	registration := plugin.BusinessReconciliationRule{OwnerModuleID: owner.ID, ID: "sales-ar", Version: 1,
		SourceQueryOwnerModuleID: source.ID, SourceQueryName: "billable", SourceQueryInput: json.RawMessage(`{}`), SourcePageLimit: 10,
		TargetQueryOwnerModuleID: target.ID, TargetQueryName: "invoices", TargetQueryInput: json.RawMessage(`{}`), TargetPageLimit: 10, MaxRows: 100,
		RepairCallerModuleID: owner.ID, RepairCapability: "ar.repair", RepairOperation: "issue", RepairLeaseID: "repair-lease"}
	ownerProvider := plugin.ProviderRegistration{TenantID: scope.TenantID, ProviderID: "reconciliation-owner", OwnerModuleID: owner.ID, Kind: plugin.ProviderInternal, BusinessReconciliationRules: []plugin.BusinessReconciliationRule{registration}}
	plan, err := plugin.ResolveModules(scope, []contract.ModuleDescriptor{source, target, owner}, []plugin.ProviderRegistration{sourceProvider, targetProvider, ownerProvider}, nil, 4)
	if err != nil {
		t.Fatal("resolve programmatic reconciliation module plan:", err)
	}
	registry := doctype.NewRegistry()
	for _, resource := range []string{"module.sales.orders.query.billable", "module.accounts.receivable.query.invoices", "module.finance.reconciliation.reconciliation.sales-ar"} {
		registry.Permissions.SetPermission(&doctype.Permission{Doctype: resource, Role: "Reader", Read: true, Report: report, Write: true})
	}
	execution, err := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: "reader", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", Roles: []string{"Reader"}})
	if err != nil {
		t.Fatal(err)
	}
	return scope, plan, registry, execution
}

func TestBusinessReconciliationWaiveRoutePersistsReasonAndAudit(t *testing.T) {
	_, plan, registry, execution := reconciliationAPIFixture(t, json.RawMessage(`[]`), json.RawMessage(`[]`), true)
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	scopeJSON, _ := json.Marshal(contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant})
	finding := recon.BusinessFinding{ID: "bf_1", Key: "sale:1", Kind: recon.FindingMissingEffect, Expected: recon.BusinessObligation{Key: "sale:1", SourceRef: "sale:1", EntityID: "entity-a", Amount: "10.00", Currency: "KES"}}
	findingJSON, _ := json.Marshal(finding)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT owner_module_id, rule_id, rule_version, scope_json, cutoff_at, complete, cursors_json, watermarks_json, created_at FROM _kora_business_recon_run WHERE tenant_id = ? AND run_id = ?")).WithArgs("tenant-a", "run-http-1").WillReturnRows(sqlmock.NewRows([]string{"owner_module_id", "rule_id", "rule_version", "scope_json", "cutoff_at", "complete", "cursors_json", "watermarks_json", "created_at"}).AddRow("finance.reconciliation", "sales-ar", 1, string(scopeJSON), "2026-11-02T00:00:00Z", 1, `{}`, `{}`, "2026-11-02T00:00:00Z"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT finding_json, state, evidence FROM _kora_business_recon_finding WHERE tenant_id = ? AND run_id = ? ORDER BY finding_id LIMIT 10000")).WithArgs("tenant-a", "run-http-1").WillReturnRows(sqlmock.NewRows([]string{"finding_json", "state", "evidence"}).AddRow(string(findingJSON), "open", ""))
	// The route validates that the path's registered owner/rule/version match
	// the stored run before calling the service's separately authorized waiver.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT owner_module_id, rule_id, rule_version, scope_json, cutoff_at, complete, cursors_json, watermarks_json, created_at FROM _kora_business_recon_run WHERE tenant_id = ? AND run_id = ?")).WithArgs("tenant-a", "run-http-1").WillReturnRows(sqlmock.NewRows([]string{"owner_module_id", "rule_id", "rule_version", "scope_json", "cutoff_at", "complete", "cursors_json", "watermarks_json", "created_at"}).AddRow("finance.reconciliation", "sales-ar", 1, string(scopeJSON), "2026-11-02T00:00:00Z", 1, `{}`, `{}`, "2026-11-02T00:00:00Z"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT finding_json, state, evidence FROM _kora_business_recon_finding WHERE tenant_id = ? AND run_id = ? ORDER BY finding_id LIMIT 10000")).WithArgs("tenant-a", "run-http-1").WillReturnRows(sqlmock.NewRows([]string{"finding_json", "state", "evidence"}).AddRow(string(findingJSON), "open", ""))
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT state FROM _kora_business_recon_finding WHERE tenant_id = ? AND run_id = ? AND finding_id = ?")).WithArgs("tenant-a", "run-http-1", "bf_1").WillReturnRows(sqlmock.NewRows([]string{"state"}).AddRow("open"))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE _kora_business_recon_finding SET state = ?, evidence = ? WHERE tenant_id = ? AND run_id = ? AND finding_id = ? AND state = ?")).WithArgs("waived", "confirmed not applicable", "tenant-a", "run-http-1", "bf_1", "open").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_business_recon_audit (tenant_id, run_id, finding_id, actor_id, action, reason, occurred_at) VALUES (?, ?, ?, ?, ?, ?, ?)")).WithArgs("tenant-a", "run-http-1", "bf_1", "reader", "waive", "confirmed not applicable", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	service := &recon.BusinessReconciliationService{Store: &recon.BusinessRunStore{DB: database, Dialect: &db.MySQLDialect{}}}
	handler := NewHandler(registry, nil)
	handler.RuntimeServices = NewSiteRuntimeServices()
	handler.RuntimeServices.Replace("tenant-a", SiteRuntimeService{BusinessReconciliation: service, ResolveModuleBindings: func(contract.BusinessScope) (plugin.BindingPlan, error) { return plan, nil }})
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "owner", Value: "finance.reconciliation"}, {Key: "rule", Value: "sales-ar"}, {Key: "run", Value: "run-http-1"}}
	ctx.Request = httptest.NewRequest("POST", "/api/modules/reconciliation/runs/finance.reconciliation/sales-ar/run-http-1/waive", strings.NewReader(`{"finding_id":"bf_1","reason":"confirmed not applicable"}`))
	ctx.Set("site_name", "tenant-a")
	ctx.Set("policy_execution", execution)
	handler.HandleBusinessReconciliationWaive(ctx)
	if recorder.Code != 200 {
		t.Fatalf("authorized waiver route = %d %s", recorder.Code, recorder.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBusinessReconciliationWaiveRouteRejectsRunFromDifferentRule(t *testing.T) {
	_, plan, registry, execution := reconciliationAPIFixture(t, json.RawMessage(`[]`), json.RawMessage(`[]`), true)
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	scopeJSON, _ := json.Marshal(contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant})
	mock.ExpectQuery(regexp.QuoteMeta("SELECT owner_module_id, rule_id, rule_version, scope_json, cutoff_at, complete, cursors_json, watermarks_json, created_at FROM _kora_business_recon_run WHERE tenant_id = ? AND run_id = ?")).WithArgs("tenant-a", "run-other").WillReturnRows(sqlmock.NewRows([]string{"owner_module_id", "rule_id", "rule_version", "scope_json", "cutoff_at", "complete", "cursors_json", "watermarks_json", "created_at"}).AddRow("other.owner", "other-rule", 9, string(scopeJSON), "2026-11-02T00:00:00Z", 1, `{}`, `{}`, "2026-11-02T00:00:00Z"))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT finding_json, state, evidence FROM _kora_business_recon_finding WHERE tenant_id = ? AND run_id = ? ORDER BY finding_id LIMIT 10000")).WithArgs("tenant-a", "run-other").WillReturnRows(sqlmock.NewRows([]string{"finding_json", "state", "evidence"}))
	service := &recon.BusinessReconciliationService{Store: &recon.BusinessRunStore{DB: database, Dialect: &db.MySQLDialect{}}}
	handler := NewHandler(registry, nil)
	handler.RuntimeServices = NewSiteRuntimeServices()
	handler.RuntimeServices.Replace("tenant-a", SiteRuntimeService{BusinessReconciliation: service, ResolveModuleBindings: func(contract.BusinessScope) (plugin.BindingPlan, error) { return plan, nil }})
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "owner", Value: "finance.reconciliation"}, {Key: "rule", Value: "sales-ar"}, {Key: "run", Value: "run-other"}}
	ctx.Request = httptest.NewRequest("POST", "/api/modules/reconciliation/runs/finance.reconciliation/sales-ar/run-other/waive", strings.NewReader(`{"finding_id":"bf_1","reason":"confirmed not applicable"}`))
	ctx.Set("site_name", "tenant-a")
	ctx.Set("policy_execution", execution)
	handler.HandleBusinessReconciliationWaive(ctx)
	if recorder.Code != 404 {
		t.Fatalf("waiver against another owner's run = %d %s", recorder.Code, recorder.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBusinessCapabilityRepairerInvokesPinnedOwnerCommandWithStableIdempotency(t *testing.T) {
	scope, plan, _, execution := reconciliationAPIFixture(t, json.RawMessage(`[]`), json.RawMessage(`[]`), true)
	handle := &reconciliationCapabilityHandle{name: "ar.repair"}
	for i := range plan.Bindings {
		if plan.Bindings[i].Key.ConsumerModuleID == "finance.reconciliation" && plan.Bindings[i].Key.CapabilityName == "ar.repair" {
			plan.Bindings[i].Factory = reconciliationCapabilityFactory{handle: handle}
		}
	}
	if handle.calls != 0 {
		t.Fatal("repair provider was called during fixture setup")
	}
	admission := &reconciliationLeaseAdmission{}
	plan.LeaseAdmission = admission
	lease := contract.CapabilityLease{ID: "repair-lease", TenantID: scope.TenantID, Scope: scope, CallerModuleID: "finance.reconciliation", ProviderModuleID: "accounts.receivable", ProviderID: "accounts.receivable-provider", Operation: "issue", BindingRevision: plan.Revision, GrantEpoch: 1, ExpiresAt: time.Now().UTC().Add(time.Hour), State: "active"}
	registration := plugin.BusinessReconciliationRule{OwnerModuleID: "finance.reconciliation", ID: "sales-ar", Version: 1, RepairCallerModuleID: "finance.reconciliation", RepairCapability: "ar.repair", RepairOperation: "issue"}
	key := contract.IdempotencyKey("recon-repair-test-key")
	expected := recon.BusinessObligation{Key: "sale:1", SourceRef: "sale:1", EntityID: "entity-a", Amount: "10.00", Currency: "KES"}
	evidence, err := (businessCapabilityRepairer{plan: plan, execution: execution, scope: scope, registration: registration, lease: lease}).RepairBusinessObligation(context.Background(), expected, key)
	if err != nil || evidence != "owner-execution-1" {
		t.Fatalf("owner capability repair = evidence %q, err %v", evidence, err)
	}
	if handle.calls != 1 || !handle.revoked || admission.calls != 1 || handle.invocation.Meta.IdempotencyKey != key || handle.invocation.Operation.Name != "issue" {
		t.Fatalf("owner command evidence: calls=%d revoked=%t admissions=%d invocation=%+v", handle.calls, handle.revoked, admission.calls, handle.invocation)
	}
	var received recon.BusinessObligation
	if err := json.Unmarshal(handle.invocation.Payload, &received); err != nil || received != expected {
		t.Fatalf("owner repair received payload %+v, err %v", received, err)
	}
}
