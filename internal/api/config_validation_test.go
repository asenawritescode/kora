package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/plugin"
	"github.com/gin-gonic/gin"
)

func TestValidateConfigurationPublishedBindingsRequiresCurrentActiveOwnerSurface(t *testing.T) {
	gin.SetMode(gin.TestMode)
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	schema := contract.SchemaRef{ID: "sales.orders.summary", Version: 1}
	snapshot := contract.ModuleSnapshot{
		TenantID: "tenant-a", Revision: 7,
		Modules:       []contract.ModuleDescriptor{{ID: "sales.orders", Version: "1.0.0"}},
		Installations: []contract.ModuleInstallation{{TenantID: "tenant-a", ModuleID: "sales.orders", Version: "1.0.0", State: string(contract.ModuleActive)}},
	}
	services := NewSiteRuntimeServices()
	services.Replace("tenant-a", SiteRuntimeService{
		LoadModuleSnapshot: func() (contract.ModuleSnapshot, error) { return snapshot, nil },
		ResolveModuleBindings: func(got contract.BusinessScope) (plugin.BindingPlan, error) {
			if got != scope {
				t.Fatalf("resolved scope = %#v, want %#v", got, scope)
			}
			return plugin.BindingPlan{Revision: 7, Projections: map[plugin.PublishedProjectionKey]contract.ProjectionDefinition{
				{OwnerModuleID: "sales.orders", ID: "orders.summary"}: {ID: "orders.summary", OwnerModuleID: "sales.orders", ScopePolicy: contract.ScopeTenantShared, OutputSchema: schema},
			}}, nil
		},
	})
	h := &Handler{RuntimeServices: services}
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set("site_name", "tenant-a")
	binding := contract.ConfigurationSurfaceBinding{Kind: contract.ConfigurationProjection, ViewName: "Invoice Board", CatalogRevision: 7, BusinessScope: scope, OwnerModuleID: "sales.orders", Name: "orders.summary", OutputSchema: schema}
	views := []*doctype.View{{Name: "Invoice Board"}}
	canonical, blockers, issues := validateConfigurationPublishedBindings(h, ctx, []contract.ConfigurationSurfaceBinding{binding}, views)
	if len(canonical) != 1 || len(blockers) != 0 || len(issues) != 0 {
		t.Fatalf("active published surface validation = canonical %#v blockers %#v issues %#v", canonical, blockers, issues)
	}
	canonical, blockers, issues = validateConfigurationPublishedBindings(h, ctx, []contract.ConfigurationSurfaceBinding{binding}, nil)
	if len(canonical) != 0 || len(blockers) != 0 || len(issues) != 1 || !strings.Contains(issues[0], "missing generated view") {
		t.Fatalf("binding without its generated view was accepted: canonical %#v blockers %#v issues %#v", canonical, blockers, issues)
	}

	stale := binding
	stale.CatalogRevision--
	canonical, blockers, issues = validateConfigurationPublishedBindings(h, ctx, []contract.ConfigurationSurfaceBinding{stale}, views)
	if len(canonical) != 0 || len(blockers) != 0 || len(issues) != 1 {
		t.Fatalf("stale published surface was not rejected: canonical %#v blockers %#v issues %#v", canonical, blockers, issues)
	}

	snapshot.Installations[0].State = string(contract.ModuleDraining)
	canonical, blockers, issues = validateConfigurationPublishedBindings(h, ctx, []contract.ConfigurationSurfaceBinding{binding}, views)
	if len(canonical) != 0 || len(blockers) != 1 || len(issues) != 0 {
		t.Fatalf("draining published surface was not blocked: canonical %#v blockers %#v issues %#v", canonical, blockers, issues)
	}
}

func TestCheckoutViewMustBindItsOwnerPublishedCheckoutOperation(t *testing.T) {
	schema := contract.SchemaRef{ID: "sales.checkout", Version: 1}
	checkout := &contract.CheckoutOperationContract{Input: contract.CheckoutInputBinding{CartItems: "items", RequestedTotal: "total", Currency: "currency", PaymentMethod: "method"}, Outcomes: []contract.CheckoutOutcomeBinding{{Kind: contract.CheckoutInvoiceIssued, StatusField: "invoice_status", SuccessValue: "issued"}}}
	operation := contract.PublishedOperation{Capability: "sales.checkout", Operation: "submit", Checkout: checkout}
	module := contract.ModuleDescriptor{ID: "sales.orders", PublishedOperations: []contract.PublishedOperation{operation}}
	view := &doctype.View{Name: "Register", PageManifest: &doctype.PageManifest{APIVersion: "ui.kora.dev/v1", Kind: "Page", Metadata: doctype.PageManifestMetadata{Name: "Register", Version: "1", Package: "sales", Status: "active"}, Spec: doctype.PageManifestSpec{Layout: doctype.PageManifestLayout{Children: []doctype.PageComponent{{ID: "pay", Component: "payment_panel", Props: map[string]any{"bindings": map[string]any{"checkout_operation_ref": "sales.orders~sales.checkout~submit"}}}}}}}}
	binding := contract.ConfigurationSurfaceBinding{Kind: contract.ConfigurationOperation, ViewName: "Register", BusinessScope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}, CatalogRevision: 4, OwnerModuleID: "sales.orders", OperationRef: "sales.orders~sales.checkout~submit", InputSchema: &schema, OutputSchema: schema}
	modules := map[contract.ModuleID]contract.ModuleDescriptor{module.ID: module}
	views := []*doctype.View{view}

	if issues := validateCheckoutViewBindings(views, []contract.ConfigurationSurfaceBinding{binding}, modules); len(issues) != 0 {
		t.Fatalf("valid exact checkout binding rejected: %v", issues)
	}
	if issues := validateCheckoutViewBindings(views, nil, modules); len(issues) != 1 || !strings.Contains(issues[0], "current owner-published checkout operation") {
		t.Fatalf("unregistered checkout view binding accepted: %v", issues)
	}
	view.PageManifest.Spec.Layout.Children[0].Props["bindings"].(map[string]any)["checkout_operation_ref"] = "sales.orders~sales.checkout~other"
	if issues := validateCheckoutViewBindings(views, []contract.ConfigurationSurfaceBinding{binding}, modules); len(issues) != 1 {
		t.Fatalf("mismatched view operation ref accepted: %v", issues)
	}
}

func TestConfigurationCheckRejectsOversizedBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, includeImpact := range []bool{false, true} {
		t.Run(map[bool]string{false: "validate", true: "dry-run"}[includeImpact], func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/system/config/check", strings.NewReader(strings.Repeat(" ", configurationCheckBodyLimit+1)))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = request
			handleConfigurationCheck(ctx, &Handler{}, includeImpact)
			if recorder.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusRequestEntityTooLarge, recorder.Body.String())
			}
		})
	}
}

func TestConfigurationCheckViewRulesMatchOnboardingCandidateValidation(t *testing.T) {
	docNames := map[string]bool{"Invoice": true}
	views := []*doctype.View{
		{Name: "Invoice Board", Route: "/invoices", Type: "list", SourceDocType: "Invoice"},
		{Name: "Invoice List", Route: "/invoices", Type: "list", SourceDocType: "Invoice"},
	}
	messages := validateCandidateViews(views, docNames, nil)
	if len(messages) == 0 || !strings.Contains(strings.Join(messages, " "), "duplicated") {
		t.Fatalf("duplicate route was not rejected: %v", messages)
	}
	unknown := validateCandidateViews([]*doctype.View{{Name: "Invoice Board", Route: "/invoices", Type: "list", SourceDocType: "Unknown"}}, docNames, nil)
	if len(unknown) == 0 || !strings.Contains(strings.Join(unknown, " "), "unknown DocType") {
		t.Fatalf("unknown view DocType was not rejected: %v", unknown)
	}
}

func TestConfigurationCheckAllowsCompositeViewSourcesOnComponents(t *testing.T) {
	docNames := map[string]bool{"deal": true, "activity": true}
	view := &doctype.View{
		Name: "CRM Workspace", Route: "/crm", Type: "workspace",
		Components: []doctype.ViewComponent{
			{ID: "pipeline", Type: "record_table", SourceDocType: "Deal"},
			{ID: "follow_up", Type: "record_table", SourceDocType: "Activity"},
		},
	}
	if messages := validateCandidateViews([]*doctype.View{view}, docNames, nil); len(messages) != 0 {
		t.Fatalf("composite view with valid component DocTypes was rejected: %v", messages)
	}
	view.Components[1].SourceDocType = "Unknown"
	if messages := validateCandidateViews([]*doctype.View{view}, docNames, nil); len(messages) == 0 || !strings.Contains(strings.Join(messages, " "), `unknown DocType "Unknown"`) {
		t.Fatalf("unknown component DocType was not rejected: %v", messages)
	}
}

func TestConfigurationCheckValidatesViewTaskRecordTypes(t *testing.T) {
	view := &doctype.View{
		Name: "Cash-up", Route: "/cash-up", Type: "workflow", SourceDocType: "Sale",
		Task: &doctype.ViewTask{
			User: "Cashier", Trigger: "At shift close", Outcome: "Reconcile cash and card totals",
			Reads: []string{"Sale"}, Writes: []string{"Shift"}, CompletionSignal: "Shift marked reviewed",
		},
		Components: []doctype.ViewComponent{{ID: "cash_up", Type: "record_form", SourceDocType: "Sale"}},
	}
	messages := validateCandidateViews([]*doctype.View{view}, map[string]bool{"sale": true}, nil)
	if len(messages) == 0 || !strings.Contains(strings.Join(messages, " "), `task references unknown DocType "Shift"`) {
		t.Fatalf("unknown task write target was not rejected: %v", messages)
	}
	messages = validateCandidateViews([]*doctype.View{view}, map[string]bool{"sale": true, "shift": true}, nil)
	if len(messages) != 0 {
		t.Fatalf("task with declared read/write DocTypes was rejected: %v", messages)
	}
}

func TestConfigurationCheckDecodesLegacyDoctypeViewAlias(t *testing.T) {
	var req DraftConfigurationRequest
	body := []byte(`{"views":[{"name":"Invoice Board","route":"/invoices","type":"list","doctype":"Invoice"}]}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if err := decodeConfigurationCheckViews(body, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Views) != 1 || req.Views[0].SourceDocType != "Invoice" {
		t.Fatalf("decoded views = %#v", req.Views)
	}
}

func TestDecodeDraftConfigurationUsesSharedWireEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := `{"proposal_id":"proposal-1","conversation_id":"conversation-2","base_config_version_id":"config-3","created_by":"untrusted-legacy-actor","doctypes":[{"name":"Task"}],"roles":[{"name":"Manager"}],"permissions":[{"doctype":"Task","role":"Manager","read":true,"public":true}],"views":[{"name":"Tasks","doctype":"Task","route":"/tasks"}],"scripts":[{"name":"task_api","script_type":"api_method","source":"return true;"}],"scheduled_jobs":[{"name":"invoice-generation","type":"subscription_invoice","schedule":"0 0 * * *","config":{"due_days":14}}],"published_bindings":[{"kind":"projection","catalog_revision":7,"business_scope":{"tenant_id":"tenant-a","kind":"tenant"},"owner_module_id":"sales","name":"sales.orders","output_schema":{"id":"sales.orders.v1","version":1}}]}`
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/system/config/drafts", strings.NewReader(body))

	req, ok := decodeDraftConfigurationRequest(ctx)
	if !ok {
		t.Fatalf("shared configuration draft envelope was rejected: %s", recorder.Body.String())
	}
	if req.ProposalID != "proposal-1" || req.ConversationID != "conversation-2" || req.BaseConfigVersionID != "config-3" || req.CreatedBy != "untrusted-legacy-actor" {
		t.Fatalf("draft identity fields = %#v", req)
	}
	if len(req.DocTypes) != 1 || req.DocTypes[0].Name != "Task" || len(req.Roles) != 1 || req.Roles[0].Name != "Manager" {
		t.Fatalf("draft resources = doctypes %#v, roles %#v", req.DocTypes, req.Roles)
	}
	if len(req.Permissions) != 1 || !req.Permissions[0].Public || !req.Permissions[0].Read {
		t.Fatalf("draft permission = %#v", req.Permissions)
	}
	if len(req.Views) != 1 || req.Views[0].SourceDocType != "Task" || len(req.Scripts) != 1 || req.Scripts[0].Name != "task_api" {
		t.Fatalf("remaining draft resources = views %#v, scripts %#v", req.Views, req.Scripts)
	}
	if len(req.ScheduledJobs) != 1 || req.ScheduledJobs[0].Name != "invoice-generation" || req.ScheduledJobs[0].Type != "subscription_invoice" {
		t.Fatalf("scheduled jobs = %#v", req.ScheduledJobs)
	}
	if len(req.PublishedBindings) != 1 || req.PublishedBindings[0].CatalogRevision != 7 || req.PublishedBindings[0].OwnerModuleID != "sales" {
		t.Fatalf("published bindings = %#v", req.PublishedBindings)
	}
}

func TestDecodeDraftConfigurationUsesStrictEngineV2WorkflowDefinitions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	decode := func(body string) (DraftConfigurationRequest, *httptest.ResponseRecorder, bool) {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/api/system/config/drafts", strings.NewReader(body))
		req, ok := decodeDraftConfigurationRequest(ctx)
		return req, recorder, ok
	}

	valid := `{"proposal_id":"p1","workflow_definitions":[{"id":"record-created","version":1,"entry":"finish","trigger":{"kind":"record.created","table":"Task"},"steps":[{"id":"finish","kind":"end","end":{"outcome":"complete"}}]}]}`
	req, recorder, ok := decode(valid)
	if !ok || len(req.WorkflowDefinitions) != 1 || req.WorkflowDefinitions[0].ID != "record-created" || req.WorkflowDefinitions[0].Trigger == nil || req.WorkflowDefinitions[0].Trigger.Table != "Task" {
		t.Fatalf("valid V2 workflow was not decoded: ok=%t status=%d req=%#v body=%s", ok, recorder.Code, req, recorder.Body.String())
	}

	legacy := `{"proposal_id":"p1","workflow_definitions":[{"name":"Task Lifecycle","document_type":"Task","states":[],"transitions":[]}]}`
	if _, recorder, ok := decode(legacy); ok || recorder.Code != http.StatusBadRequest {
		t.Fatalf("legacy lifecycle object was accepted as V2: ok=%t status=%d body=%s", ok, recorder.Code, recorder.Body.String())
	}
	unknown := `{"proposal_id":"p1","workflow_definitions":[{"id":"record-created","version":1,"entry":"finish","unknown":true,"steps":[{"id":"finish","kind":"end","end":{"outcome":"complete"}}]}]}`
	if _, recorder, ok := decode(unknown); ok || recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown nested workflow property was accepted: ok=%t status=%d body=%s", ok, recorder.Code, recorder.Body.String())
	}
}

func TestCandidateWorkflowSchemasMergeInstalledAndProposedTables(t *testing.T) {
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Customer", Fields: []doctype.Field{{Fieldname: "email", Fieldtype: "Data"}}})
	proposed := &doctype.DocType{Name: "Task", Fields: []doctype.Field{{Fieldname: "status", Fieldtype: "Select"}, {Fieldname: "customer_email", Fieldtype: "Data", Options: "Email"}}}
	schemas := newCandidateWorkflowSchemas(registry, []*doctype.DocType{proposed})
	if !schemas.RecordTableExists("Customer") || !schemas.RecordFieldExists("Customer", "email") {
		t.Fatal("installed table schema was not retained")
	}
	if !schemas.RecordTableExists("Task") || !schemas.RecordFieldExists("Task", "status") {
		t.Fatal("proposed table schema was not added")
	}
	if fieldType, ok := schemas.RecordFieldType("Task", "customer_email"); !ok || fieldType != "Email" {
		t.Fatalf("proposed field type = %q, found=%t", fieldType, ok)
	}
}

func TestDecodeDraftConfigurationRejectsMalformedTypedResource(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/system/config/drafts", strings.NewReader(`{"doctypes":[42]}`))

	if _, ok := decodeDraftConfigurationRequest(ctx); ok {
		t.Fatal("malformed DocType resource was accepted")
	}
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestDecodeDraftConfigurationRejectsUnknownResourceField(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/system/config/drafts", strings.NewReader(`{"doctypes":[{"name":"Task"}],"unexpected":true}`))
	if _, ok := decodeDraftConfigurationRequest(ctx); ok {
		t.Fatal("unknown configuration draft field was accepted")
	}
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "unknown field") {
		t.Fatalf("unknown field rejection = HTTP %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestConfigurationDraftCanReuseIdenticalExistingDocType(t *testing.T) {
	handler, reg, mock, database := setupTestHandler(t)
	defer database.Close()
	reg.Register(&doctype.DocType{
		Name: "TestDoc", Module: "Testing", TitleField: "name", SortField: "modified", SortOrder: "DESC",
		Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data", Label: "Title"}},
	})

	currentSnapshot := doctype.ToSExpr(&doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{reg.Get("TestDoc")}})
	mock.ExpectQuery(`SELECT id, config FROM _kora_config_version WHERE site = \? AND status IN \('Draft', 'Active'\)`).
		WithArgs("test.local").WillReturnRows(sqlmock.NewRows([]string{"id", "config"}).AddRow("cv-test-1", currentSnapshot))
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(version\), 0\) FROM _kora_config_version WHERE site = \?`).
		WithArgs("test.local").WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(1))
	mock.ExpectQuery(`SELECT config, version FROM _kora_config_version WHERE site = \? AND id = \?`).
		WithArgs("test.local", "cv-test-1").WillReturnRows(sqlmock.NewRows([]string{"config", "version"}).AddRow(currentSnapshot, 1))
	mock.ExpectExec(`INSERT INTO _kora_config_version`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/system/config/drafts", strings.NewReader(`{"proposal_id":"reuse-existing","doctypes":[{"name":"TestDoc","module":"Testing","title_field":"name","sort_field":"modified","sort_order":"DESC","fields":[{"fieldname":"title","fieldtype":"Data","label":"Title"}]}]}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	injectContext(ctx)
	injectDB(ctx, database, reg)
	handler.HandleConfigurationDraft(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("identical existing DocType was not reusable: HTTP %d: %s", recorder.Code, recorder.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("draft persistence expectations: %v", err)
	}
}

func TestDraftPermissionPreservesUnsupportedPublicFlagForRejection(t *testing.T) {
	var req DraftConfigurationRequest
	if err := json.Unmarshal([]byte(`{"permissions":[{"doctype":"Invoice","role":"Finance","read":true,"public":true}]}`), &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Permissions) != 1 || req.Permissions[0] == nil || !req.Permissions[0].Public || req.Permissions[0].Doctype != "Invoice" || req.Permissions[0].Role != "Finance" {
		t.Fatalf("permission request = %#v", req.Permissions)
	}
	messages, _ := validateConfigurationCandidateRules(req, []*doctype.DocType{{Name: "Invoice"}}, nil, &Handler{})
	if len(messages) == 0 || !strings.Contains(strings.Join(messages, " "), "public permission grants are unsupported") {
		t.Fatalf("public permission was not rejected: %v", messages)
	}
}

func TestConfigurationCheckRejectsInvalidDoctypeRelationships(t *testing.T) {
	parent := &doctype.DocType{Name: "Invoice", Fields: []doctype.Field{{Fieldname: "items", Fieldtype: "Table", Options: "Customer"}, {Fieldname: "owner", Fieldtype: "Link", Options: "Finance"}}}
	customer := &doctype.DocType{Name: "Customer"}
	roles := map[string]bool{normalizeDocTypeName("Finance"): true}
	messages := validateCandidateDocTypeReferences([]*doctype.DocType{parent, customer}, roles, nil)
	text := strings.Join(messages, " ")
	if !strings.Contains(text, "cannot link to role") || !strings.Contains(text, "must reference a child DocType") {
		t.Fatalf("invalid relationships were not rejected: %v", messages)
	}
}

func TestConfigurationCheckAllowsDocTypeThatSharesANameWithRole(t *testing.T) {
	activity := &doctype.DocType{Name: "Activity", Fields: []doctype.Field{{Fieldname: "assigned_to", Fieldtype: "Link", Options: "Household Member"}}}
	householdMember := &doctype.DocType{Name: "Household Member"}
	roles := map[string]bool{normalizeDocTypeName("Household Member"): true}

	messages := validateCandidateDocTypeReferences([]*doctype.DocType{activity, householdMember}, roles, nil)
	if len(messages) != 0 {
		t.Fatalf("valid DocType target was rejected because it shares a role name: %v", messages)
	}

	existing := doctype.NewRegistry()
	existing.Register(householdMember)
	messages = validateCandidateDocTypeReferences([]*doctype.DocType{activity}, roles, existing)
	if len(messages) != 0 {
		t.Fatalf("valid existing DocType target was rejected because it shares a role name: %v", messages)
	}
}
