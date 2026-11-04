package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/plugin"
	"github.com/asenawritescode/kora/policy"
	"github.com/gin-gonic/gin"
)

func TestOfflineCapabilitiesDeclareOnlyActorAuthorizedReadyOperations(t *testing.T) {
	gin.SetMode(gin.TestMode)
	scope := contract.BusinessScope{TenantID: "offline-capability-tenant", Kind: contract.ScopeKindTenant}
	input := contract.SchemaRef{ID: "sales.action.input", Version: 1}
	output := contract.SchemaRef{ID: "sales.action.output", Version: 1}
	makeSurface := func(name string, eligible bool) contract.PublishedOperation {
		return contract.PublishedOperation{Name: name, Capability: contract.CapabilityName("sales." + name), Operation: name, ScopePolicy: contract.ScopeTenantShared, AllowedCallers: []contract.ModuleID{"sales.client"}, OfflineEligible: eligible}
	}
	issue, onlineOnly, forbidden, unavailable := makeSurface("issue", true), makeSurface("preview", false), makeSurface("archive", true), makeSurface("submit", true)
	operations := []contract.PublishedOperation{issue, onlineOnly, forbidden, unavailable}
	offers := make([]contract.CapabilityOffer, 0, len(operations))
	bindings := make([]plugin.ResolvedBinding, 0, len(operations)-1)
	for _, published := range operations {
		spec := contract.OperationSpec{Name: published.Operation, Input: input, Output: output, Effect: contract.EffectLocal}
		offers = append(offers, contract.CapabilityOffer{Name: published.Capability, Contract: output, Operations: []contract.OperationSpec{spec}})
		if published.Name != unavailable.Name {
			bindings = append(bindings, plugin.ResolvedBinding{Key: plugin.BindingKey{Scope: scope, ConsumerModuleID: "sales.invoices", CapabilityName: published.Capability}, OwnerModuleID: "sales.invoices", ProviderID: "sales.provider", Revision: 4})
		}
	}
	module := contract.ModuleDescriptor{ID: "sales.invoices", Version: "1.0.0", Provides: offers, PublishedOperations: operations, Schemas: []contract.ModuleSchemaDocument{{Ref: input, Schema: json.RawMessage(`{"type":"object"}`)}, {Ref: output, Schema: json.RawMessage(`{"type":"object"}`)}}}
	snapshot := contract.ModuleSnapshot{TenantID: scope.TenantID, Revision: 4, Modules: []contract.ModuleDescriptor{module}, Installations: []contract.ModuleInstallation{{TenantID: scope.TenantID, ModuleID: module.ID, Version: module.Version, State: string(contract.ModuleActive)}}}
	registry := doctype.NewRegistry()
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.sales.invoices.operation.issue", Role: "Operator", Write: true})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.sales.invoices.operation.preview", Role: "Operator", Write: true})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.sales.invoices.operation.submit", Role: "Operator", Write: true})
	execution, err := policy.NewVerifiedExecution(scope.TenantID, contract.ActorContext{PrincipalID: "operator", PrincipalType: contract.PrincipalHuman, Site: string(scope.TenantID), Roles: []string{"Operator"}, AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	services := NewSiteRuntimeServices()
	services.Replace(string(scope.TenantID), SiteRuntimeService{LoadModuleSnapshot: func() (contract.ModuleSnapshot, error) { return snapshot, nil }, ResolveModuleBindings: func(contract.BusinessScope) (plugin.BindingPlan, error) {
		return plugin.BindingPlan{Revision: snapshot.Revision, Bindings: bindings}, nil
	}})
	handler := NewHandler(registry, nil)
	handler.RuntimeServices = services
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("GET", "/api/v2/offline/capabilities", nil)
	ctx.Set("site_name", string(scope.TenantID))
	ctx.Set("policy_execution", execution)
	handler.HandleOfflineCapabilitiesV2(ctx)
	if recorder.Code != 200 {
		t.Fatalf("capabilities status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Data contract.OfflineCapabilities `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if err := response.Data.Validate(); err != nil {
		t.Fatalf("capabilities failed contract validation: %v", err)
	}
	if len(response.Data.OfflineOperations) != 1 || response.Data.OfflineOperations[0].OperationRef != contract.PublishedOperationRef(module.ID, issue.Capability, issue.Operation) {
		t.Fatalf("capabilities exposed unauthorized, online-only, or unavailable operations: %+v", response.Data.OfflineOperations)
	}
}

func TestExecutePublishedOperationUsesRegisteredSurfaceAndStrictRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	scope := contract.BusinessScope{TenantID: "operation-tenant", Kind: contract.ScopeKindTenant}
	inputSchema, output := contract.SchemaRef{ID: "sales.issue.input", Version: 1}, contract.SchemaRef{ID: "sales.issue.output", Version: 1}
	published := contract.PublishedOperation{Name: "issue", Capability: "sales.invoice", Operation: "issue", ScopePolicy: contract.ScopeTenantShared, AllowedCallers: []contract.ModuleID{"sales.checkout"}}
	module := contract.ModuleDescriptor{ID: "sales.invoices", Version: "1.0.0", PublishedOperations: []contract.PublishedOperation{published}, Provides: []contract.CapabilityOffer{{Name: published.Capability, Contract: output, Operations: []contract.OperationSpec{{Name: published.Operation, Input: inputSchema, Output: output, Effect: contract.EffectLocal}}}}}
	snapshot := contract.ModuleSnapshot{TenantID: scope.TenantID, Revision: 12, Modules: []contract.ModuleDescriptor{module}}
	registry := doctype.NewRegistry()
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.sales.invoices.operation.issue", Role: "Operator", Write: true})
	execution, err := policy.NewVerifiedExecution(scope.TenantID, contract.ActorContext{PrincipalID: "operator", PrincipalType: contract.PrincipalHuman, Site: string(scope.TenantID), Roles: []string{"Operator"}, AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	services := NewSiteRuntimeServices()
	var called bool
	services.Replace(string(scope.TenantID), SiteRuntimeService{LoadModuleSnapshot: func() (contract.ModuleSnapshot, error) { return snapshot, nil }, ExecutePublishedOperation: func(_ context.Context, got policy.Execution, gotScope contract.BusinessScope, owner contract.ModuleID, gotPublished contract.PublishedOperation, schema contract.SchemaRef, meta contract.CommandMeta, inputData []byte) (contract.OperationReceipt, error) {
		called = true
		if got.Tenant() != execution.Tenant() || got.Actor().PrincipalID != execution.Actor().PrincipalID || gotScope != scope || owner != module.ID || gotPublished.Name != published.Name || schema != inputSchema || meta.IdempotencyKey != "retry-1" || string(inputData) != `{"invoice_id":"INV-1"}` {
			t.Fatalf("invocation contract mismatch: scope=%+v owner=%s schema=%+v meta=%+v input=%s", gotScope, owner, schema, meta, inputData)
		}
		ref := output
		return contract.OperationReceipt{ExecutionID: "execute-1", OperationRef: contract.PublishedOperationRef(owner, published.Capability, published.Operation), BusinessScope: scope, Status: contract.ReceiptCompleted, OutputSchema: &ref, Output: json.RawMessage(`{"issued":true}`), Revision: "12"}, nil
	}})
	handler := NewHandler(registry, nil)
	handler.RuntimeServices = services
	invoke := func(body string) (int, string) {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest("POST", "/api/v2/modules/operations/"+contract.PublishedOperationRef(module.ID, published.Capability, published.Operation)+"/execute", strings.NewReader(body))
		ctx.Params = gin.Params{{Key: "operation", Value: contract.PublishedOperationRef(module.ID, published.Capability, published.Operation)}}
		ctx.Set("site_name", string(scope.TenantID))
		ctx.Set("policy_execution", execution)
		handler.HandleExecutePublishedOperation(ctx)
		return rec.Code, rec.Body.String()
	}
	status, body := invoke(`{"meta":{"idempotency_key":"retry-1"},"input_schema":{"id":"sales.issue.input","version":1},"input":{"invoice_id":"INV-1"}}`)
	if status != 200 || !strings.Contains(body, "execute-1") || !called {
		t.Fatalf("execute status=%d called=%v body=%s", status, called, body)
	}
	called = false
	status, body = invoke(`{"caller_module_id":"sales.checkout","meta":{"idempotency_key":"retry-1"},"input_schema":{"id":"sales.issue.input","version":1},"input":{"invoice_id":"INV-1"}}`)
	if status != 400 || called {
		t.Fatalf("client supplied module provenance accepted: status=%d called=%v body=%s", status, called, body)
	}
}

func TestLookupPublishedOperationReturnsOriginalReceiptWithoutRedispatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	scope := contract.BusinessScope{TenantID: "lookup-tenant", Kind: contract.ScopeKindTenant}
	inputSchema, outputSchema := contract.SchemaRef{ID: "sales.issue.input", Version: 1}, contract.SchemaRef{ID: "sales.issue.output", Version: 1}
	published := contract.PublishedOperation{Name: "issue", Capability: "sales.invoice", Operation: "issue", ScopePolicy: contract.ScopeTenantShared, AllowedCallers: []contract.ModuleID{"sales.checkout"}}
	owner := contract.ModuleDescriptor{ID: "sales.invoices", Version: "1.0.0", PublishedOperations: []contract.PublishedOperation{published}, Provides: []contract.CapabilityOffer{{Name: published.Capability, Contract: outputSchema, Operations: []contract.OperationSpec{{Name: published.Operation, Input: inputSchema, Output: outputSchema, Effect: contract.EffectLocal}}}}}
	snapshot := contract.ModuleSnapshot{TenantID: scope.TenantID, Revision: 12, Modules: []contract.ModuleDescriptor{owner}}
	registry := doctype.NewRegistry()
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.sales.invoices.operation.issue", Role: "Operator", Write: true})
	execution, err := policy.NewVerifiedExecution(scope.TenantID, contract.ActorContext{PrincipalID: "operator", PrincipalType: contract.PrincipalHuman, Site: string(scope.TenantID), Roles: []string{"Operator"}, AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	operationRef := contract.PublishedOperationRef(owner.ID, published.Capability, published.Operation)
	key := contract.IdempotencyKey("original-intent")
	output := json.RawMessage(`{"issued":true}`)
	receipt := contract.OperationReceipt{ExecutionID: "execute-original", OperationRef: operationRef, BusinessScope: scope, Status: contract.ReceiptCompleted, OutputSchema: &outputSchema, Output: output, Revision: "12"}
	services := NewSiteRuntimeServices()
	var lookupCalls, executeCalls int
	services.Replace(string(scope.TenantID), SiteRuntimeService{
		LoadModuleSnapshot: func() (contract.ModuleSnapshot, error) { return snapshot, nil },
		LookupPublishedOperation: func(_ context.Context, got policy.Execution, gotScope contract.BusinessScope, gotOwner contract.ModuleID, gotPublished contract.PublishedOperation, gotKey contract.IdempotencyKey) (contract.OperationReceipt, bool, error) {
			lookupCalls++
			if got.Actor().PrincipalID != execution.Actor().PrincipalID || gotScope != scope || gotOwner != owner.ID || gotPublished.Operation != published.Operation || gotKey != key {
				t.Fatalf("lookup identity changed: actor=%s scope=%+v owner=%s operation=%s key=%s", got.Actor().PrincipalID, gotScope, gotOwner, gotPublished.Operation, gotKey)
			}
			return receipt, true, nil
		},
		ExecutePublishedOperation: func(context.Context, policy.Execution, contract.BusinessScope, contract.ModuleID, contract.PublishedOperation, contract.SchemaRef, contract.CommandMeta, []byte) (contract.OperationReceipt, error) {
			executeCalls++
			return contract.OperationReceipt{}, nil
		},
	})
	handler := NewHandler(registry, nil)
	handler.RuntimeServices = services
	body, err := json.Marshal(contract.LookupPublishedOperation{OperationRef: operationRef, IdempotencyKey: key})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("POST", "/api/v2/module-executions/lookup", strings.NewReader(string(body)))
	ctx.Set("site_name", string(scope.TenantID))
	ctx.Set("policy_execution", execution)
	handler.HandleLookupPublishedOperation(ctx)
	if recorder.Code != 200 || lookupCalls != 1 || executeCalls != 0 || !strings.Contains(recorder.Body.String(), string(receipt.ExecutionID)) {
		t.Fatalf("lookup status=%d lookup_calls=%d execute_calls=%d body=%s", recorder.Code, lookupCalls, executeCalls, recorder.Body.String())
	}
}

func TestLookupPublishedOperationDeniesUnauthorizedActorBeforeReceiptRead(t *testing.T) {
	gin.SetMode(gin.TestMode)
	scope := contract.BusinessScope{TenantID: "lookup-denied-tenant", Kind: contract.ScopeKindTenant}
	inputSchema, outputSchema := contract.SchemaRef{ID: "sales.issue.input", Version: 1}, contract.SchemaRef{ID: "sales.issue.output", Version: 1}
	published := contract.PublishedOperation{Name: "issue", Capability: "sales.invoice", Operation: "issue", ScopePolicy: contract.ScopeTenantShared, AllowedCallers: []contract.ModuleID{"sales.checkout"}}
	owner := contract.ModuleDescriptor{ID: "sales.invoices", Version: "1.0.0", PublishedOperations: []contract.PublishedOperation{published}, Provides: []contract.CapabilityOffer{{Name: published.Capability, Contract: outputSchema, Operations: []contract.OperationSpec{{Name: published.Operation, Input: inputSchema, Output: outputSchema, Effect: contract.EffectLocal}}}}}
	snapshot := contract.ModuleSnapshot{TenantID: scope.TenantID, Revision: 12, Modules: []contract.ModuleDescriptor{owner}}
	execution, err := policy.NewVerifiedExecution(scope.TenantID, contract.ActorContext{PrincipalID: "unauthorized", PrincipalType: contract.PrincipalHuman, Site: string(scope.TenantID), AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	lookupCalls := 0
	services := NewSiteRuntimeServices()
	services.Replace(string(scope.TenantID), SiteRuntimeService{
		LoadModuleSnapshot: func() (contract.ModuleSnapshot, error) { return snapshot, nil },
		LookupPublishedOperation: func(context.Context, policy.Execution, contract.BusinessScope, contract.ModuleID, contract.PublishedOperation, contract.IdempotencyKey) (contract.OperationReceipt, bool, error) {
			lookupCalls++
			return contract.OperationReceipt{}, false, nil
		},
	})
	handler := NewHandler(doctype.NewRegistry(), nil)
	handler.RuntimeServices = services
	operationRef := contract.PublishedOperationRef(owner.ID, published.Capability, published.Operation)
	body, err := json.Marshal(contract.LookupPublishedOperation{OperationRef: operationRef, IdempotencyKey: "private-intent"})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("POST", "/api/v2/module-executions/lookup", strings.NewReader(string(body)))
	ctx.Set("site_name", string(scope.TenantID))
	ctx.Set("policy_execution", execution)
	handler.HandleLookupPublishedOperation(ctx)
	if recorder.Code != http.StatusForbidden || lookupCalls != 0 {
		t.Fatalf("unauthorized lookup status=%d lookup_calls=%d body=%s", recorder.Code, lookupCalls, recorder.Body.String())
	}
}

func TestLookupPublishedOperationDoesNotReturnReceiptFromAnotherEntity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	requestedScope := contract.BusinessScope{TenantID: "entity-lookup-tenant", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	otherScope := contract.BusinessScope{TenantID: requestedScope.TenantID, Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-b"}
	inputSchema, outputSchema := contract.SchemaRef{ID: "sales.issue.input", Version: 1}, contract.SchemaRef{ID: "sales.issue.output", Version: 1}
	published := contract.PublishedOperation{Name: "issue", Capability: "sales.invoice", Operation: "issue", ScopePolicy: contract.ScopeEntityOwned, AllowedCallers: []contract.ModuleID{"sales.checkout"}}
	owner := contract.ModuleDescriptor{ID: "sales.invoices", Version: "1.0.0", PublishedOperations: []contract.PublishedOperation{published}, Provides: []contract.CapabilityOffer{{Name: published.Capability, Contract: outputSchema, Operations: []contract.OperationSpec{{Name: published.Operation, Input: inputSchema, Output: outputSchema, Effect: contract.EffectLocal}}}}}
	snapshot := contract.ModuleSnapshot{TenantID: requestedScope.TenantID, Revision: 12, Modules: []contract.ModuleDescriptor{owner}}
	registry := doctype.NewRegistry()
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.sales.invoices.operation.issue", Role: "Operator", Write: true, Scope: requestedScope})
	execution, err := policy.NewVerifiedExecution(requestedScope.TenantID, contract.ActorContext{PrincipalID: "operator", PrincipalType: contract.PrincipalHuman, Site: string(requestedScope.TenantID), Roles: []string{"Operator"}, AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	operationRef := contract.PublishedOperationRef(owner.ID, published.Capability, published.Operation)
	receipt := contract.OperationReceipt{ExecutionID: "execute-entity-b", OperationRef: operationRef, BusinessScope: otherScope, Status: contract.ReceiptCompleted, OutputSchema: &outputSchema, Output: json.RawMessage(`{"entity":"entity-b","secret":"private"}`), Revision: "12"}
	services := NewSiteRuntimeServices()
	services.Replace(string(requestedScope.TenantID), SiteRuntimeService{
		LoadModuleSnapshot: func() (contract.ModuleSnapshot, error) { return snapshot, nil },
		LookupPublishedOperation: func(context.Context, policy.Execution, contract.BusinessScope, contract.ModuleID, contract.PublishedOperation, contract.IdempotencyKey) (contract.OperationReceipt, bool, error) {
			return receipt, true, nil
		},
	})
	handler := NewHandler(registry, nil)
	handler.RuntimeServices = services
	body, err := json.Marshal(contract.LookupPublishedOperation{OperationRef: operationRef, IdempotencyKey: "entity-a-intent"})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("POST", "/api/v2/module-executions/lookup", strings.NewReader(string(body)))
	ctx.Request.Header.Set("X-Kora-Legal-Entity-ID", requestedScope.LegalEntityID)
	ctx.Set("site_name", string(requestedScope.TenantID))
	ctx.Set("policy_execution", execution)
	handler.HandleLookupPublishedOperation(ctx)
	if recorder.Code != http.StatusInternalServerError || strings.Contains(recorder.Body.String(), "entity-b") || strings.Contains(recorder.Body.String(), "private") {
		t.Fatalf("cross-entity receipt status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestGetPublishedOperationReceiptUsesActorAndBusinessScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	scope := contract.BusinessScope{TenantID: "receipt-tenant", Kind: contract.ScopeKindTenant}
	inputSchema, outputSchema := contract.SchemaRef{ID: "sales.issue.input", Version: 1}, contract.SchemaRef{ID: "sales.issue.output", Version: 1}
	published := contract.PublishedOperation{Name: "issue", Capability: "sales.invoice", Operation: "issue", ScopePolicy: contract.ScopeTenantShared, AllowedCallers: []contract.ModuleID{"sales.checkout"}}
	owner := contract.ModuleDescriptor{ID: "sales.invoices", Version: "1.0.0", PublishedOperations: []contract.PublishedOperation{published}, Provides: []contract.CapabilityOffer{{Name: published.Capability, Contract: outputSchema, Operations: []contract.OperationSpec{{Name: published.Operation, Input: inputSchema, Output: outputSchema, Effect: contract.EffectLocal}}}}}
	snapshot := contract.ModuleSnapshot{TenantID: scope.TenantID, Revision: 12, Modules: []contract.ModuleDescriptor{owner}}
	registry := doctype.NewRegistry()
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.sales.invoices.operation.issue", Role: "Operator", Write: true})
	execution, err := policy.NewVerifiedExecution(scope.TenantID, contract.ActorContext{PrincipalID: "operator", PrincipalType: contract.PrincipalHuman, Site: string(scope.TenantID), Roles: []string{"Operator"}, AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	id := contract.OperationID(contract.NewOperationID())
	operationRef := contract.PublishedOperationRef(owner.ID, published.Capability, published.Operation)
	receipt := contract.OperationReceipt{ExecutionID: id, OperationRef: operationRef, BusinessScope: scope, Status: contract.ReceiptCompleted, OutputSchema: &outputSchema, Output: json.RawMessage(`{"issued":true}`), Revision: "12"}
	services := NewSiteRuntimeServices()
	lookupCalls := 0
	services.Replace(string(scope.TenantID), SiteRuntimeService{
		LoadModuleSnapshot: func() (contract.ModuleSnapshot, error) { return snapshot, nil },
		LookupPublishedOperationByID: func(_ context.Context, got policy.Execution, gotScope contract.BusinessScope, gotID contract.OperationID) (contract.OperationReceipt, bool, error) {
			lookupCalls++
			if got.Actor().PrincipalID != execution.Actor().PrincipalID || gotScope != scope || gotID != id {
				t.Fatalf("receipt lookup identity changed: actor=%s scope=%+v id=%s", got.Actor().PrincipalID, gotScope, gotID)
			}
			return receipt, true, nil
		},
	})
	handler := NewHandler(registry, nil)
	handler.RuntimeServices = services
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("GET", "/api/v2/module-executions/"+string(id), nil)
	ctx.Params = gin.Params{{Key: "execution", Value: string(id)}}
	ctx.Set("site_name", string(scope.TenantID))
	ctx.Set("policy_execution", execution)
	handler.HandleGetPublishedOperationReceipt(ctx)
	if recorder.Code != http.StatusOK || lookupCalls != 1 || !strings.Contains(recorder.Body.String(), string(id)) {
		t.Fatalf("receipt status=%d lookup_calls=%d body=%s", recorder.Code, lookupCalls, recorder.Body.String())
	}
}

func TestExecutePublishedOperationRejectsForgedSchemaAndProviderBeforeDispatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	scope := contract.BusinessScope{TenantID: "execute-validation-tenant", Kind: contract.ScopeKindTenant}
	inputSchema, outputSchema := contract.SchemaRef{ID: "sales.issue.input", Version: 1}, contract.SchemaRef{ID: "sales.issue.output", Version: 1}
	published := contract.PublishedOperation{Name: "issue", Capability: "sales.invoice", Operation: "issue", ScopePolicy: contract.ScopeTenantShared, AllowedCallers: []contract.ModuleID{"sales.checkout"}}
	owner := contract.ModuleDescriptor{ID: "sales.invoices", Version: "1.0.0", PublishedOperations: []contract.PublishedOperation{published}, Provides: []contract.CapabilityOffer{{Name: published.Capability, Contract: outputSchema, Operations: []contract.OperationSpec{{Name: published.Operation, Input: inputSchema, Output: outputSchema, Effect: contract.EffectLocal}}}}}
	snapshot := contract.ModuleSnapshot{TenantID: scope.TenantID, Revision: 12, Modules: []contract.ModuleDescriptor{owner}}
	registry := doctype.NewRegistry()
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.sales.invoices.operation.issue", Role: "Operator", Write: true})
	execution, err := policy.NewVerifiedExecution(scope.TenantID, contract.ActorContext{PrincipalID: "operator", PrincipalType: contract.PrincipalHuman, Site: string(scope.TenantID), Roles: []string{"Operator"}, AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	services := NewSiteRuntimeServices()
	executeCalls := 0
	services.Replace(string(scope.TenantID), SiteRuntimeService{
		LoadModuleSnapshot: func() (contract.ModuleSnapshot, error) { return snapshot, nil },
		ExecutePublishedOperation: func(context.Context, policy.Execution, contract.BusinessScope, contract.ModuleID, contract.PublishedOperation, contract.SchemaRef, contract.CommandMeta, []byte) (contract.OperationReceipt, error) {
			executeCalls++
			return contract.OperationReceipt{}, nil
		},
	})
	handler := NewHandler(registry, nil)
	handler.RuntimeServices = services
	operationRef := contract.PublishedOperationRef(owner.ID, published.Capability, published.Operation)
	cases := []struct {
		name string
		body string
	}{
		{name: "forged schema", body: `{"meta":{"idempotency_key":"intent-1"},"input_schema":{"id":"attacker.schema","version":1},"input":{"invoice_id":"INV-1"}}`},
		{name: "forged provider", body: `{"meta":{"idempotency_key":"intent-1"},"input_schema":{"id":"sales.issue.input","version":1},"provider_id":"attacker-provider","input":{"invoice_id":"INV-1"}}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest("POST", "/api/v2/modules/operations/"+operationRef+"/execute", strings.NewReader(testCase.body))
			ctx.Params = gin.Params{{Key: "operation", Value: operationRef}}
			ctx.Set("site_name", string(scope.TenantID))
			ctx.Set("policy_execution", execution)
			handler.HandleExecutePublishedOperation(ctx)
			if recorder.Code != http.StatusBadRequest || executeCalls != 0 {
				t.Fatalf("execute status=%d calls=%d body=%s", recorder.Code, executeCalls, recorder.Body.String())
			}
		})
	}
}

func TestOfflineIntentHTTPAuthorizesStrictlyAndPreservesRetryIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	scope := contract.BusinessScope{TenantID: "offline-intent-tenant", Kind: contract.ScopeKindTenant}
	inputSchema := contract.SchemaRef{ID: "sales.issue.input", Version: 1}
	outputSchema := contract.SchemaRef{ID: "sales.issue.output", Version: 1}
	published := contract.PublishedOperation{
		Name: "issue", Capability: "sales.invoice", Operation: "issue",
		ScopePolicy: contract.ScopeTenantShared, OfflineEligible: true,
	}
	module := contract.ModuleDescriptor{
		ID: "sales.invoices", Version: "1.0.0",
		PublishedOperations: []contract.PublishedOperation{published},
		Provides: []contract.CapabilityOffer{{Name: published.Capability, Contract: outputSchema, Operations: []contract.OperationSpec{{
			Name: published.Operation, Input: inputSchema, Output: outputSchema, Effect: contract.EffectLocal,
		}}}},
	}
	snapshot := contract.ModuleSnapshot{TenantID: scope.TenantID, Revision: 12, Modules: []contract.ModuleDescriptor{module}}
	registry := doctype.NewRegistry()
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.sales.invoices.operation.issue", Role: "Operator", Write: true})
	operator, err := policy.NewVerifiedExecution(scope.TenantID, contract.ActorContext{PrincipalID: "operator", PrincipalType: contract.PrincipalHuman, Site: string(scope.TenantID), Roles: []string{"Operator"}, AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := policy.NewVerifiedExecution(scope.TenantID, contract.ActorContext{PrincipalID: "reader", PrincipalType: contract.PrincipalHuman, Site: string(scope.TenantID), Roles: []string{"Reader"}, AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	services := NewSiteRuntimeServices()
	var calls int
	var gotMeta contract.CommandMeta
	var gotScope contract.BusinessScope
	services.Replace(string(scope.TenantID), SiteRuntimeService{
		LoadModuleSnapshot: func() (contract.ModuleSnapshot, error) { return snapshot, nil },
		ExecutePublishedOperation: func(_ context.Context, execution policy.Execution, gotBusinessScope contract.BusinessScope, _ contract.ModuleID, gotPublished contract.PublishedOperation, gotSchema contract.SchemaRef, meta contract.CommandMeta, input []byte) (contract.OperationReceipt, error) {
			calls++
			gotMeta, gotScope = meta, gotBusinessScope
			if execution.Actor().PrincipalID != "operator" || gotPublished.Name != published.Name || gotPublished.Capability != published.Capability || gotPublished.Operation != published.Operation || !gotPublished.OfflineEligible || gotSchema != inputSchema || string(input) != `{"invoice_id":"INV-1"}` {
				t.Fatalf("offline dispatch identity changed: actor=%+v operation=%+v schema=%+v input=%s", execution.Actor(), gotPublished, gotSchema, input)
			}
			return contract.OperationReceipt{
				ExecutionID: "execution-12", OperationRef: contract.PublishedOperationRef(module.ID, published.Capability, published.Operation),
				BusinessScope: gotBusinessScope, Status: contract.ReceiptCompleted, OutputSchema: &outputSchema,
				Output: json.RawMessage(`{"issued":true}`), Revision: "12",
			}, nil
		},
	})
	handler := NewHandler(registry, nil)
	handler.RuntimeServices = services
	intent := contract.OfflineIntent{
		LocalOperationID: "local-invoice-12", BusinessScope: scope,
		OperationRef: contract.PublishedOperationRef(module.ID, published.Capability, published.Operation),
		InputSchema:  inputSchema, Input: json.RawMessage(`{"invoice_id":"INV-1"}`),
		IdempotencyKey: "retry-invoice-12", ExpectedRevision: "11", CatalogRevision: snapshot.Revision,
	}
	invoke := func(execution policy.Execution, body string) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v2/offline/intents", strings.NewReader(body))
		ctx.Set("site_name", string(scope.TenantID))
		ctx.Set("policy_execution", execution)
		handler.HandleOfflineIntentsV2(ctx)
		return recorder
	}
	validBody, err := json.Marshal(contract.OfflineIntentsRequest{Intents: []contract.OfflineIntent{intent}})
	if err != nil {
		t.Fatal(err)
	}
	response := invoke(operator, string(validBody))
	if response.Code != http.StatusOK {
		t.Fatalf("valid offline intent: status=%d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		Data contract.OfflineIntentsResponse `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Data.Receipts) != 1 || result.Data.Receipts[0].LocalOperationID != intent.LocalOperationID || result.Data.Receipts[0].Receipt.ExecutionID != "execution-12" {
		t.Fatalf("offline receipt did not preserve local operation identity: %+v", result.Data)
	}
	if calls != 1 || gotMeta.IdempotencyKey != intent.IdempotencyKey || gotMeta.ExpectedRevision != intent.ExpectedRevision || gotScope != scope {
		t.Fatalf("dispatch identity changed: calls=%d meta=%+v scope=%+v", calls, gotMeta, gotScope)
	}

	unknownField := strings.Replace(string(validBody), `"operation_ref":`, `"unknown":true,"operation_ref":`, 1)
	if rejected := invoke(operator, unknownField); rejected.Code != http.StatusBadRequest || calls != 1 {
		t.Fatalf("unknown intent field was accepted or dispatched: status=%d calls=%d body=%s", rejected.Code, calls, rejected.Body.String())
	}

	wrongScope := intent
	wrongScope.BusinessScope.TenantID = "another-tenant"
	wrongScopeBody, _ := json.Marshal(contract.OfflineIntentsRequest{Intents: []contract.OfflineIntent{wrongScope}})
	if rejected := invoke(operator, string(wrongScopeBody)); rejected.Code != http.StatusBadRequest || calls != 1 {
		t.Fatalf("cross-tenant intent was accepted or dispatched: status=%d calls=%d body=%s", rejected.Code, calls, rejected.Body.String())
	}
	if rejected := invoke(reader, string(validBody)); rejected.Code != http.StatusForbidden || calls != 1 {
		t.Fatalf("protected intent was accepted or dispatched: status=%d calls=%d body=%s", rejected.Code, calls, rejected.Body.String())
	}

	published.OfflineEligible = false
	module.PublishedOperations = []contract.PublishedOperation{published}
	module.Provides[0].Operations[0].Name = published.Operation
	snapshot.Modules = []contract.ModuleDescriptor{module}
	if rejected := invoke(operator, string(validBody)); rejected.Code != http.StatusForbidden || calls != 1 {
		t.Fatalf("online-only operation was accepted or dispatched: status=%d calls=%d body=%s", rejected.Code, calls, rejected.Body.String())
	}
	module.PublishedOperations[0].OfflineEligible = true
	snapshot.Revision++
	if rejected := invoke(operator, string(validBody)); rejected.Code != http.StatusConflict || calls != 1 {
		t.Fatalf("stale catalog intent was accepted or dispatched: status=%d calls=%d body=%s", rejected.Code, calls, rejected.Body.String())
	}
}
