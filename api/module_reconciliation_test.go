package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/recon"
	"github.com/gin-gonic/gin"
)

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
