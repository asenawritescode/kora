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
	source, sourceProvider := queryModule("sales.orders", "billable", "sales.reconcile", []string{"key", "source_ref", "entity_id", "amount", "currency", "occurred_at"}, json.RawMessage(`[{"key":"sale:1","source_ref":"sale:1","entity_id":"entity-a","amount":"10.00","currency":"KES","occurred_at":"2026-11-02T10:00:00Z"}]`))
	target, targetProvider := queryModule("accounts.receivable", "invoices", "ar.reconcile", []string{"key", "target_ref", "entity_id", "amount", "currency", "occurred_at"}, json.RawMessage(`[]`))
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
		registry.Permissions.SetPermission(&doctype.Permission{Doctype: resource, Role: "Reader", Read: true, Report: true})
	}
	execution, err := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: "reader", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", Roles: []string{"Reader"}})
	if err != nil {
		t.Fatal(err)
	}
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
