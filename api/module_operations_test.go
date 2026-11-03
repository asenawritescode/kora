package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/policy"
	"github.com/gin-gonic/gin"
)

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
