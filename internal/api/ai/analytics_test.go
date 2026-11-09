package ai

import (
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/analytics"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/policy"
)

func TestBuildAnalyticsCatalogIncludesValidatedReportModels(t *testing.T) {
	registry := reportAnalyticsRegistry()
	reports := []analytics.ReportDefinition{reportAnalyticsDefinition()}
	catalog, err := buildAnalyticsCatalog(registry, reports)
	if err != nil {
		t.Fatalf("build report catalog: %v", err)
	}
	var model *analytics.SemanticModel
	for index := range catalog.Models {
		if catalog.Models[index].Name == "sales_items" {
			model = &catalog.Models[index]
			break
		}
	}
	if model == nil || model.Relation == nil || model.Relation.ChildDoctype != "Sale Item" {
		t.Fatalf("related report model missing from AI catalog: %#v", model)
	}
	if len(catalog.Reports) != 1 || catalog.Reports[0].Name != "sales_by_item" {
		t.Fatalf("configured report missing from AI catalog: %#v", catalog.Reports)
	}
}

func TestRelatedAnalyticsPermissionsCoverAllSources(t *testing.T) {
	registry := reportAnalyticsRegistry()
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Sale", Role: "Cashier", Read: true})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Sale Item", Role: "Cashier", Read: true})
	request := analytics.SemanticRequest{Queries: []analytics.ModelQuery{{Model: "sales_items"}}}
	reports := []analytics.ReportDefinition{reportAnalyticsDefinition()}
	cashier := analyticsTestExecution(t, "tenant-a", []string{"Cashier"}, nil)
	if err := checkRelatedAnalyticsPermissions(registry, reports, request, cashier); err == nil {
		t.Fatal("expected missing Product read permission to be rejected")
	}
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Product", Role: "Cashier", Read: true, IfOwner: true})
	if err := checkRelatedAnalyticsPermissions(registry, reports, request, cashier); err == nil {
		t.Fatal("expected owner-scoped permission to be rejected for grouped report")
	}
	admin := analyticsTestExecution(t, "tenant-a", []string{doctype.AdminRole}, nil)
	if err := checkRelatedAnalyticsPermissions(registry, reports, request, admin); err != nil {
		t.Fatalf("administrator should be permitted: %v", err)
	}
}

func TestRelatedAnalyticsPermissionsRequireExecutionAndHonorExactGrants(t *testing.T) {
	registry := reportAnalyticsRegistry()
	request := analytics.SemanticRequest{Queries: []analytics.ModelQuery{{Model: "sales_items"}}}
	reports := []analytics.ReportDefinition{reportAnalyticsDefinition()}
	if err := checkRelatedAnalyticsPermissions(registry, reports, request, policy.Execution{}); err == nil {
		t.Fatal("missing execution was authorized")
	}
	grants := []policy.Grant{
		{Resource: "Sale", Operation: "read"},
		{Resource: "Sale Item", Operation: "read"},
		{Resource: "Product", Operation: "read"},
	}
	service := analyticsTestExecution(t, "tenant-a", nil, grants)
	if err := checkRelatedAnalyticsPermissions(registry, reports, request, service); err != nil {
		t.Fatalf("exact verified grants should authorize source reads: %v", err)
	}
}

func TestRelatedAnalyticsPermissionsCheckOrdinaryModelSource(t *testing.T) {
	registry := reportAnalyticsRegistry()
	model := analytics.SemanticModel{Name: "sales", SourceDoctype: "Sale", Measures: []analytics.SemanticMeasure{{Name: "count", Aggregation: "count"}}}
	report := analytics.ReportDefinition{Name: "sales_report", Label: "Sales", Models: []analytics.SemanticModel{model}, Queries: []analytics.ReportQuery{{ID: "sales", Model: model.Name, Measures: []string{"count"}}}}
	request := analytics.SemanticRequest{Queries: []analytics.ModelQuery{{Model: "sales"}}}
	reader := analyticsTestExecution(t, "tenant-a", []string{"Cashier"}, nil)
	if err := checkRelatedAnalyticsPermissions(registry, []analytics.ReportDefinition{report}, request, reader); err == nil {
		t.Fatal("ordinary model bypassed its source DocType read permission")
	}
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Sale", Role: "Cashier", Read: true})
	if err := checkRelatedAnalyticsPermissions(registry, []analytics.ReportDefinition{report}, request, reader); err != nil {
		t.Fatalf("readable source DocType was denied: %v", err)
	}
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Sale", Role: "Cashier", Read: true, IfOwner: true})
	if err := checkRelatedAnalyticsPermissions(registry, []analytics.ReportDefinition{report}, request, reader); err == nil {
		t.Fatal("owner-scoped permission authorized an unscoped aggregate")
	}
}

func TestRelatedAnalyticsPermissionsCheckGeneratedModelWithoutReport(t *testing.T) {
	registry := reportAnalyticsRegistry()
	model := analytics.BuildSemanticModel(registry.Get("Sale"))
	if model.Name == "" {
		t.Fatal("generated semantic model has no name")
	}
	request := analytics.SemanticRequest{Queries: []analytics.ModelQuery{{Model: model.Name}}}
	reader := analyticsTestExecution(t, "tenant-a", []string{"Cashier"}, nil)
	if err := checkRelatedAnalyticsPermissions(registry, nil, request, reader); err == nil {
		t.Fatal("generated catalog model bypassed its source DocType read permission")
	}
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Sale", Role: "Cashier", Read: true})
	if err := checkRelatedAnalyticsPermissions(registry, nil, request, reader); err != nil {
		t.Fatalf("generated model with readable source was denied: %v", err)
	}
	if err := checkRelatedAnalyticsPermissions(registry, nil, analytics.SemanticRequest{Queries: []analytics.ModelQuery{{Model: "missing"}}}, reader); err == nil {
		t.Fatal("unknown analytics model was authorized")
	}
}

func TestAnalyticsInsightPermissionRequiresUnscopedSourceRead(t *testing.T) {
	registry := reportAnalyticsRegistry()
	execution := analyticsTestExecution(t, "tenant-a", []string{"Cashier"}, nil)
	if err := authorizeAnalyticsInsight(registry, execution, "Sale"); err == nil {
		t.Fatal("missing source permission authorized an analytics insight")
	}
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Sale", Role: "Cashier", Read: true, IfOwner: true})
	if err := authorizeAnalyticsInsight(registry, execution, "Sale"); err == nil {
		t.Fatal("owner-scoped source permission authorized an aggregate insight")
	}
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Sale", Role: "Cashier", Read: true})
	if err := authorizeAnalyticsInsight(registry, execution, "Sale"); err != nil {
		t.Fatalf("unscoped source read permission was denied: %v", err)
	}
}

func TestAnalyticsQueryToolArgsDecodeTopLevelDatesAndRejectUnknownSQLFields(t *testing.T) {
	parsed, err := decodeAnalyticsQueryToolArgs(map[string]any{
		"from": "2026-01-01", "to": "2026-02-01", "granularity": "month", "limit": float64(20),
		"queries": []any{map[string]any{"model": "sale", "measures": []any{"count"}, "dimensions": []any{"status"}}},
	})
	if err != nil {
		t.Fatal("decode valid analytics query:", err)
	}
	if parsed.From != "2026-01-01" || parsed.To != "2026-02-01" || parsed.Granularity != "month" || parsed.Limit != 20 {
		t.Fatalf("query options were not preserved: %#v", parsed)
	}
	for _, args := range []map[string]any{
		{"queries": []any{map[string]any{"model": "sale", "measures": []any{"count"}, "sql": "select * from _kora_user"}}},
		{"time": map[string]any{"from": "2026-01-01"}, "queries": []any{map[string]any{"model": "sale", "measures": []any{"count"}}}},
	} {
		if _, err := decodeAnalyticsQueryToolArgs(args); err == nil {
			t.Errorf("unknown/raw query fields were accepted: %#v", args)
		}
	}
}

func TestAnalyticsModelsExcludeHiddenPasswordAndOwnerScopedFields(t *testing.T) {
	registry := reportAnalyticsRegistry()
	for _, doctypeName := range []string{"Sale", "Sale Item", "Product"} {
		registry.Permissions.SetPermission(&doctype.Permission{Doctype: doctypeName, Role: "Cashier", Read: true})
	}
	execution := analyticsTestExecution(t, "tenant-a", []string{"Cashier"}, nil)
	model := reportAnalyticsDefinition().Models[0]
	if !analyticsModelReadable(registry, execution, model) {
		t.Fatal("model with visible fields and read access was denied")
	}
	registry.Get("Sale Item").GetField("line_total").Hidden = true
	if analyticsModelReadable(registry, execution, model) {
		t.Fatal("hidden measure was exposed to analytics")
	}
	registry.Get("Sale Item").GetField("line_total").Hidden = false
	registry.Get("Product").GetField("product_name").Fieldtype = "Password"
	if analyticsModelReadable(registry, execution, model) {
		t.Fatal("password link label was exposed to analytics")
	}
}

func TestAnalyticsQueryCarriesProjectionActorScope(t *testing.T) {
	registry := doctype.NewRegistry()
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.finance.ar.projection.ar_balance", Role: "Reader", Read: true, IfAssigned: []string{"assigned_to"}, Scope: scope})
	definition := analytics.ReportDefinition{Name: "ar_report", Label: "AR report", Models: []analytics.SemanticModel{{Name: "ar_balance", ProjectionOwner: "finance.ar", ProjectionID: "ar_balance", ProjectionOwnerField: "owner_id", PublishedQuery: "sales.orders/balances", ProjectionScopePolicy: contract.ScopeEntityOwned, AllowedFields: []string{"assigned_to", "owner_id", "amount"}, Measures: []analytics.SemanticMeasure{{Name: "count", Aggregation: "count"}}}}, Queries: []analytics.ReportQuery{{ID: "balance", Model: "ar_balance", Measures: []string{"count"}}}}
	execution := analyticsTestExecution(t, "tenant-a", []string{"Reader"}, nil)
	scopes, err := authorizeRelatedAnalytics(registry, []analytics.ReportDefinition{definition}, analytics.SemanticRequest{Queries: []analytics.ModelQuery{{Model: "ar_balance", Measures: []string{"count"}}}}, execution, scope)
	if err != nil || scopes["ar_balance"].SubjectID != "analytics-test" || len(scopes["ar_balance"].AssignedFields) != 1 || scopes["ar_balance"].AssignedFields[0] != "assigned_to" {
		t.Fatalf("analytics actor scopes = %#v, error=%v", scopes, err)
	}
}

func analyticsTestExecution(t *testing.T, tenant string, roles []string, grants []policy.Grant) policy.Execution {
	t.Helper()
	execution, err := policy.NewVerifiedExecutionWithGrants(contract.TenantID(tenant), contract.ActorContext{
		PrincipalID: "analytics-test", PrincipalType: contract.PrincipalService, Site: tenant, Roles: roles,
		AuthenticatedAt: time.Now().UTC(),
	}, grants)
	if err != nil {
		t.Fatal(err)
	}
	return execution
}

func reportAnalyticsRegistry() *doctype.Registry {
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Sale", Fields: []doctype.Field{
		{Fieldname: "invoice_date", Fieldtype: "Date"},
		{Fieldname: "status", Fieldtype: "Select"},
		{Fieldname: "items", Fieldtype: "Table", Options: "Sale Item"},
	}})
	registry.Register(&doctype.DocType{Name: "Sale Item", IsChildTable: true, Fields: []doctype.Field{
		{Fieldname: "product", Fieldtype: "Link", Options: "Product"},
		{Fieldname: "quantity", Fieldtype: "Float"},
		{Fieldname: "line_total", Fieldtype: "Currency"},
	}})
	registry.Register(&doctype.DocType{Name: "Product", Fields: []doctype.Field{{Fieldname: "product_name", Fieldtype: "Data"}}})
	return registry
}

func reportAnalyticsDefinition() analytics.ReportDefinition {
	return analytics.ReportDefinition{
		Name: "sales_by_item", Label: "Sales by item",
		Models: []analytics.SemanticModel{{
			Name: "sales_items", Label: "Sales items", SourceDoctype: "Sale", TimeDimension: "invoice_date",
			Relation: &analytics.SemanticRelation{ParentDoctype: "Sale", ChildDoctype: "Sale Item", ChildTableField: "items", ParentTimeField: "invoice_date"},
			Dimensions: []analytics.SemanticDimension{
				{Name: "product", Label: "Product", Field: "product", Type: "category", Source: "child", LinkDoctype: "Product", LinkLabelField: "product_name"},
				{Name: "invoice_date", Label: "Sale date", Field: "invoice_date", Type: "time", Source: "parent"},
			},
			Measures: []analytics.SemanticMeasure{{Name: "sales_total", Label: "Sales total", Field: "line_total", Aggregation: "sum", Source: "child"}},
		}},
		Queries: []analytics.ReportQuery{{ID: "items", Model: "sales_items", Measures: []string{"sales_total"}, Dimensions: []string{"product"}}},
	}
}
