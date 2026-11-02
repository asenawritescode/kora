package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/policy"
	"github.com/gin-gonic/gin"
)

func TestRelatedAnalyticsQueryRequiresSourceRead(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Invoice"})
	registry.Permissions.AddRole(&doctype.Role{Name: "Reader"})
	models := []analytics.SemanticModel{{Name: "invoices", SourceDoctype: "Invoice"}}
	request := analytics.AnalyticsQueryRequest{Queries: []analytics.ModelQuery{{Model: "invoices"}}}

	ctx := newAnalyticsAuthorizationContext(t, "stable-tenant", "tenant-route", []string{"Reader"}, nil)
	if checkRelatedModelPermissions(ctx, registry, models, request) {
		t.Fatal("ordinary model query without source read permission was allowed")
	}
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Invoice", Role: "Reader", Read: true})
	ctx = newAnalyticsAuthorizationContext(t, "stable-tenant", "tenant-route", []string{"Reader"}, nil)
	if !checkRelatedModelPermissions(ctx, registry, models, request) {
		t.Fatal("ordinary model query with source read permission was denied")
	}
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Invoice", Role: "Reader", Read: true, IfOwner: true})
	ctx = newAnalyticsAuthorizationContext(t, "stable-tenant", "tenant-route", []string{"Reader"}, nil)
	if checkRelatedModelPermissions(ctx, registry, models, request) {
		t.Fatal("owner-scoped permission authorized an unscoped aggregate query")
	}
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Invoice", Role: "Reader", Read: true, IfAssigned: []string{"assignee_user"}})
	ctx = newAnalyticsAuthorizationContext(t, "stable-tenant", "tenant-route", []string{"Reader"}, nil)
	if checkRelatedModelPermissions(ctx, registry, models, request) {
		t.Fatal("assignment-scoped permission authorized an unscoped aggregate query")
	}
}

func TestModuleOwnedResourcesNeverEnterGenericAnalyticsCatalog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Private Invoice", ModuleOwnerID: "billing.ar", ModuleAccess: contract.ModulePrivate})
	registry.Permissions.AddRole(&doctype.Role{Name: "Administrator"})
	catalog := analytics.BuildSemanticCatalog([]*doctype.DocType{registry.Get("Private Invoice")})
	ctx := newAnalyticsAuthorizationContext(t, "stable-tenant", "tenant-route", []string{"Administrator"}, nil)
	if filtered := filterReadableAnalyticsCatalog(ctx, registry, catalog); len(filtered.Models) != 0 {
		t.Fatalf("module-owned resource leaked through analytics catalog: %#v", filtered.Models)
	}
}

func TestRelatedAnalyticsQueryAcceptsExactVerifiedSourceGrant(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Invoice"})
	models := []analytics.SemanticModel{{Name: "invoices", SourceDoctype: "Invoice"}}
	request := analytics.AnalyticsQueryRequest{Queries: []analytics.ModelQuery{{Model: "invoices"}}}
	ctx := newAnalyticsAuthorizationContext(t, "stable-tenant", "tenant-route", nil, []policy.Grant{{Resource: "Invoice", Operation: "read"}})
	ctx.Set("auth_type", "extension")
	if !checkRelatedModelPermissions(ctx, registry, models, request) {
		t.Fatal("exact service read grant was denied")
	}
}

func TestEntityOwnedAnalyticsRequiresExactEntityGrant(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Invoice", BusinessScopePolicy: contract.ScopeEntityOwned, BusinessScopeField: "legal_entity_id"})
	models := []analytics.SemanticModel{{Name: "invoices", SourceDoctype: "Invoice"}}
	request := analytics.AnalyticsQueryRequest{Queries: []analytics.ModelQuery{{Model: "invoices"}}}
	scope := contract.BusinessScope{TenantID: "stable-tenant", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Invoice", Role: "Reader", Read: true, Scope: scope})
	ctx := newAnalyticsAuthorizationContext(t, "stable-tenant", "tenant-route", []string{"Reader"}, nil)
	ctx.Request.Header.Set("X-Kora-Legal-Entity-ID", "entity-b")
	if checkRelatedModelPermissions(ctx, registry, models, request) {
		t.Fatal("entity grant authorized analytics for a different legal entity")
	}
	ctx = newAnalyticsAuthorizationContext(t, "stable-tenant", "tenant-route", []string{"Reader"}, nil)
	ctx.Request.Header.Set("X-Kora-Legal-Entity-ID", "entity-a")
	if !checkRelatedModelPermissions(ctx, registry, models, request) {
		t.Fatal("exact entity analytics grant was denied")
	}
}

func TestConfiguredReportQueryChecksReadScopeForEveryQueriedModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{{Name: "Deal"}}, []*doctype.Role{{Name: "Sales Rep"}}, []*doctype.Permission{{
		Doctype: "Deal", Role: "Sales Rep", Read: true, IfAssigned: []string{"owner_user"},
	}})
	catalog := &analytics.SemanticCatalog{Models: []analytics.SemanticModel{{Name: "deal", SourceDoctype: "Deal"}}}
	report := &analytics.ReportDefinition{
		Name:    "crm_pipeline_performance",
		Queries: []analytics.ReportQuery{{ID: "deals_by_stage", Model: "deal", Measures: []string{"count"}, Dimensions: []string{"pipeline_stage"}}},
	}

	ctx := newAnalyticsAuthorizationContext(t, "stable-tenant", "tenant-route", []string{"Sales Rep"}, nil)
	if checkRelatedReportPermissions(ctx, registry, report, catalog) {
		t.Fatal("assignment-scoped role authorized a report query over unscoped rollups")
	}

	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Deal", Role: "Sales Rep", Read: true})
	ctx = newAnalyticsAuthorizationContext(t, "stable-tenant", "tenant-route", []string{"Sales Rep"}, nil)
	if !checkRelatedReportPermissions(ctx, registry, report, catalog) {
		t.Fatal("unscoped read permission for every report source was denied")
	}
	if checkRelatedReportPermissions(ctx, registry, report, &analytics.SemanticCatalog{}) {
		t.Fatal("report query with an unresolved source model was allowed")
	}

	inlineCatalog := &analytics.SemanticCatalog{}
	report.Models = []analytics.SemanticModel{{Name: "deal", SourceDoctype: "Deal"}}
	if !checkRelatedReportPermissions(ctx, registry, report, inlineCatalog) {
		t.Fatal("explicit report model was not checked when catalog model was absent")
	}
}

func TestReportQueryRouteDeniesScopedSourceBeforeRollupQuery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("sqlmock:", err)
	}
	defer database.Close()

	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{{Name: "Deal", Fields: []doctype.Field{{Fieldname: "pipeline_stage", Fieldtype: "Select", Options: "Lead\\nQualified"}}}}, []*doctype.Role{{Name: "Sales Rep"}}, []*doctype.Permission{{
		Doctype: "Deal", Role: "Sales Rep", Read: true, IfAssigned: []string{"owner_user"},
	}})
	report := `{"name":"crm_pipeline_performance","label":"CRM pipeline performance","queries":[{"id":"deals_by_stage","model":"deal","measures":["count"],"dimensions":["pipeline_stage"]}]}`
	config, err := json.Marshal(map[string]any{"reports": []json.RawMessage{json.RawMessage(report)}})
	if err != nil {
		t.Fatal("marshal config:", err)
	}
	mock.ExpectQuery(`SELECT config FROM _kora_config_version WHERE site = \? AND status = 'Active' ORDER BY version DESC LIMIT 1`).
		WithArgs("tenant-route").WillReturnRows(sqlmock.NewRows([]string{"config"}).AddRow(string(config)))

	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set("site_name", "tenant-route")
		c.Set("site_id", "tenant-route")
		actor := contract.ActorContext{PrincipalID: "rep-1", PrincipalType: contract.PrincipalHuman, SubjectUserID: "rep-1", Site: "tenant-route", Roles: []string{"Sales Rep"}, AuthenticatedAt: time.Now().UTC()}
		execution, err := policy.NewVerifiedExecution(contract.TenantID("tenant-route"), actor)
		if err != nil {
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		c.Set("policy_execution", execution)
		c.Next()
	})
	RegisterAnalyticsRoutes(engine.Group("/api"), registry, database, nil, nil, kdb.Resolve("mysql"))
	request := httptest.NewRequest(http.MethodPost, "/api/analytics/reports/crm_pipeline_performance/query", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("row-scoped report query status = %d, want 403; body=%s", response.Code, response.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("unexpected rollup query or missing config lookup:", err)
	}
}

func TestAnalyticsMetricProjectionFiltersUnreadableSources(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Invoice"})
	registry.Register(&doctype.DocType{Name: "Payroll"})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Invoice", Role: "Reader", Read: true})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Payroll", Role: "Reader", Read: true, IfAssigned: []string{"assigned_to"}})
	ctx := newAnalyticsAuthorizationContext(t, "stable-tenant", "tenant-route", []string{"Reader"}, nil)
	got := filterReadableAnalyticsMetrics(ctx, registry, []*analytics.Metric{
		{Name: "invoice_count", DocType: "Invoice"},
		{Name: "payroll_total", DocType: "Payroll"},
	})
	if len(got) != 1 || got[0].Name != "invoice_count" {
		t.Fatalf("visible analytics metrics = %#v, want only Invoice source metric", got)
	}
}

func TestAnalyticsCatalogProjectionFiltersModelsAndDependentReports(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Invoice"})
	registry.Register(&doctype.DocType{Name: "Payroll"})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Invoice", Role: "Reader", Read: true})
	ctx := newAnalyticsAuthorizationContext(t, "stable-tenant", "tenant-route", []string{"Reader"}, nil)
	catalog := &analytics.SemanticCatalog{
		Models: []analytics.SemanticModel{
			{Name: "invoices", SourceDoctype: "Invoice"},
			{Name: "payroll", SourceDoctype: "Payroll"},
		},
		Reports: []analytics.ReportDefinition{
			{Name: "invoice_report", Queries: []analytics.ReportQuery{{Model: "invoices"}}},
			{Name: "mixed_report", Queries: []analytics.ReportQuery{{Model: "invoices"}, {Model: "payroll"}}},
			{Name: "unknown_report", Queries: []analytics.ReportQuery{{Model: "missing"}}},
		},
	}
	got := filterReadableAnalyticsCatalog(ctx, registry, catalog)
	if len(got.Models) != 1 || got.Models[0].Name != "invoices" {
		t.Fatalf("visible catalog models = %#v, want only readable Invoice model", got.Models)
	}
	if len(got.Reports) != 1 || got.Reports[0].Name != "invoice_report" {
		t.Fatalf("visible reports = %#v, want only report backed by readable models", got.Reports)
	}
}

func TestAnalyticsInsightsRouteChecksSourcePermissionBeforeDatabase(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{{Name: "Invoice"}, {Name: "Analytics"}}, []*doctype.Role{
		{Name: "Reader"}, {Name: "OwnerReader"}, {Name: "AnalyticsWriter"},
	}, []*doctype.Permission{
		{Doctype: "Invoice", Role: "Reader", Read: true},
		{Doctype: "Invoice", Role: "OwnerReader", Read: true, IfOwner: true},
		{Doctype: "Analytics", Role: "AnalyticsWriter", Write: true},
	})
	var active policy.Execution
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("site_name", "tenant-route")
		c.Set("site_id", "stable-tenant")
		c.Set("site_registry", registry)
		if active.Valid() {
			c.Set("policy_execution", active)
		}
		c.Next()
	})
	RegisterAnalyticsRoutes(router.Group("/api"), registry, nil, nil, nil, nil)
	request := func(execution policy.Execution, method, target, body string) int {
		active = execution
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(method, target, strings.NewReader(body)))
		return recorder.Code
	}
	if got := request(policy.Execution{}, "GET", "/api/analytics/insights/Invoice", ""); got != 403 {
		t.Fatalf("missing execution insights status=%d, want 403", got)
	}
	owner := newAnalyticsAuthorizationContext(t, "stable-tenant", "tenant-route", []string{"OwnerReader"}, nil)
	ownerExecution, _ := owner.Get("policy_execution")
	if got := request(ownerExecution.(policy.Execution), "GET", "/api/analytics/insights/Invoice", ""); got != 403 {
		t.Fatalf("owner-scoped insights status=%d, want 403", got)
	}
	reader := newAnalyticsAuthorizationContext(t, "stable-tenant", "tenant-route", []string{"Reader"}, nil)
	readerExecution, _ := reader.Get("policy_execution")
	if got := request(readerExecution.(policy.Execution), "GET", "/api/analytics/insights/Invoice", ""); got != 503 {
		t.Fatalf("authorized insights should reach the missing-database response; status=%d, want 503", got)
	}
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "malformed body", body: `{"from":`},
		{name: "unknown sql field", body: `{"sql":"SELECT * FROM _kora_user"}`},
		{name: "over-limit", body: `{"limit":201}`},
		{name: "conflicting metric", body: `{"metric":"other_metric"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := request(readerExecution.(policy.Execution), "POST", "/api/analytics/metrics/invoice_count/query", tc.body); got != 400 {
				t.Fatalf("unsafe analytics request status=%d, want 400", got)
			}
		})
	}
	if got := request(readerExecution.(policy.Execution), "POST", "/api/analytics/metrics", `{"name":"invoice_count","doctype":"Invoice"}`); got != 403 {
		t.Fatalf("reader created an analytics metric without Analytics write permission: status=%d", got)
	}
}

func newAnalyticsAuthorizationContext(t *testing.T, tenant, site string, roles []string, grants []policy.Grant) *gin.Context {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("GET", "/", nil)
	ctx.Set("site_name", site)
	ctx.Set("site_id", tenant)
	actor := contract.ActorContext{PrincipalID: "analytics-reader", PrincipalType: contract.PrincipalHuman, SubjectUserID: "analytics-reader", Site: site, Roles: roles, AuthenticatedAt: time.Now().UTC()}
	execution, err := policy.NewVerifiedExecutionWithGrants(contract.TenantID(tenant), actor, grants)
	if err != nil {
		t.Fatal("create verified analytics execution:", err)
	}
	ctx.Set("policy_execution", execution)
	return ctx
}
