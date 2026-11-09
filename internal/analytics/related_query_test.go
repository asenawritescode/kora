package analytics

import (
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

func salesItemsTestRegistry() *doctype.Registry {
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
	registry.Register(&doctype.DocType{Name: "Product", Fields: []doctype.Field{
		{Fieldname: "product_name", Fieldtype: "Data"},
	}})
	return registry
}

func salesItemsTestReport() ReportDefinition {
	return ReportDefinition{
		Name:  "sales_by_item",
		Label: "Sales by item",
		Models: []SemanticModel{{
			Name: "sales_items", Label: "Sales items", SourceDoctype: "Sale", TimeDimension: "invoice_date",
			Relation: &SemanticRelation{ParentDoctype: "Sale", ChildDoctype: "Sale Item", ChildTableField: "items", ParentTimeField: "invoice_date", Filters: []SemanticRelationFilter{{Source: "parent", Field: "status", Operator: "=", Value: "Paid"}}},
			Dimensions: []SemanticDimension{
				{Name: "product", Label: "Product or service", Field: "product", Type: "category", Source: "child", LinkDoctype: "Product", LinkLabelField: "product_name"},
				{Name: "invoice_date", Label: "Sale date", Field: "invoice_date", Type: "time", Source: "parent"},
			},
			Measures: []SemanticMeasure{
				{Name: "quantity_sold", Label: "Quantity sold", Field: "quantity", Aggregation: "sum", Source: "child"},
				{Name: "sales_total", Label: "Sales total", Field: "line_total", Aggregation: "sum", Format: "currency", Source: "child"},
			},
		}},
		Queries: []ReportQuery{{ID: "items", Model: "sales_items", Measures: []string{"quantity_sold", "sales_total"}, Dimensions: []string{"product"}}},
	}
}

func TestReportModelValidatesDeclaredParentChildRelationship(t *testing.T) {
	report := salesItemsTestReport()
	if err := ValidateReportModels(salesItemsTestRegistry(), []ReportDefinition{report}); err != nil {
		t.Fatalf("valid report relationship rejected: %v", err)
	}

	report.Models[0].Relation.ChildTableField = "not_items"
	if err := ValidateReportModels(salesItemsTestRegistry(), []ReportDefinition{report}); err == nil {
		t.Fatal("expected invalid parent/child relationship to be rejected")
	}
}

func TestRelatedSemanticQueryUsesParentDateAndStatusForChildMeasures(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer database.Close()

	queryPattern := `SELECT .* FROM ` + regexp.QuoteMeta("`tabSale__items`")
	mock.ExpectQuery(queryPattern).
		WithArgs("Sale", "items", "2026-10-01", "2026-11-01", "Paid", MaxSemanticResultRows).
		WillReturnRows(sqlmock.NewRows([]string{"dimension", "value"}).AddRow("Coca-Cola", 6.0))
	mock.ExpectQuery(queryPattern).
		WithArgs("Sale", "items", "2026-10-01", "2026-11-01", "Paid", MaxSemanticResultRows).
		WillReturnRows(sqlmock.NewRows([]string{"dimension", "value"}).AddRow("Coca-Cola", 600.0))

	report := salesItemsTestReport()
	catalog := &SemanticCatalog{Models: []SemanticModel{}}
	if err := AddReportModels(catalog, []ReportDefinition{report}); err != nil {
		t.Fatalf("add report model: %v", err)
	}
	catalog.Reports = []ReportDefinition{report}
	if err := catalog.Validate(); err != nil {
		t.Fatalf("validate catalog: %v", err)
	}
	engine := &QueryEngine{DB: database, SiteName: "demo", Dialect: kdb.Resolve("mysql")}
	response, err := engine.ResolveSemanticQuery(catalog, AnalyticsQueryRequest{
		Queries: []ModelQuery{{Model: "sales_items", Measures: []string{"quantity_sold", "sales_total"}, Dimensions: []string{"product"}}},
		Time:    QueryTime{From: "2026-10-01", To: "2026-10-31", Granularity: "month"},
	}, salesItemsTestRegistry())
	if err != nil {
		t.Fatalf("resolve related query: %v", err)
	}
	if response.DataSource != "live_records" {
		t.Fatalf("data source = %q, want live_records", response.DataSource)
	}
	if len(response.Queries) != 1 || len(response.Queries[0].Rows) != 1 {
		t.Fatalf("unexpected query results: %#v", response.Queries)
	}
	row := response.Queries[0].Rows[0]
	if row["product"] != "Coca-Cola" || row["quantity_sold"] != 6.0 || row["sales_total"] != 600.0 {
		t.Fatalf("unexpected grouped result: %#v", row)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}
