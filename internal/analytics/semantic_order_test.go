package analytics

import (
	"database/sql/driver"
	"reflect"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

func taskRegistry() *doctype.Registry {
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Task", Fields: []doctype.Field{
		{Fieldname: "transaction_date", Fieldtype: "Date"},
		{Fieldname: "status", Fieldtype: "Select"},
		{Fieldname: "customer", Fieldtype: "Link", Options: "Customer"},
		{Fieldname: "amount", Fieldtype: "Currency"},
	}})
	registry.Register(&doctype.DocType{Name: "Customer", Fields: []doctype.Field{
		{Fieldname: "customer_name", Fieldtype: "Data"},
	}})
	return registry
}

func taskModel() SemanticModel {
	return SemanticModel{
		Name: "task", Label: "Tasks", SourceDoctype: "Task", TimeDimension: "transaction_date",
		Dimensions: []SemanticDimension{
			{Name: "transaction_date", Field: "transaction_date", Type: "time"},
			{Name: "customer", Field: "customer", Type: "category", LinkDoctype: "Customer", LinkLabelField: "customer_name"},
		},
		Measures: []SemanticMeasure{
			{Name: "count", Aggregation: "count"},
			{Name: "paid_amount", Aggregation: "sum", Field: "amount"},
		},
	}
}

func TestSemanticRequestValidatesQueryOrder(t *testing.T) {
	model := taskModel()
	cases := []struct {
		name       string
		order      string
		dimensions []string
		wantErr    bool
	}{
		{name: "no order", order: "", dimensions: []string{"customer"}},
		{name: "measure_desc on non-time dimension", order: OrderMeasureDesc, dimensions: []string{"customer"}},
		{name: "measure_desc without dimension", order: OrderMeasureDesc, wantErr: true},
		{name: "measure_desc on time dimension", order: OrderMeasureDesc, dimensions: []string{"transaction_date"}, wantErr: true},
		{name: "unknown order", order: "asc", dimensions: []string{"customer"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := SemanticRequest{
				Queries: []ModelQuery{{Model: "task", Measures: []string{"count"}, Dimensions: tc.dimensions, Order: tc.order}},
				Time:    QueryTime{From: "2026-01-01", To: "2026-01-31"},
				Limit:   2,
			}
			err := request.Validate(&SemanticCatalog{Models: []SemanticModel{model}})
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestSortRowsByMeasureRanksBeforeLimit(t *testing.T) {
	rows := []map[string]any{
		{"customer": "Beta", "count": "3"},
		{"customer": "Gamma", "count": nil},
		{"customer": "Alpha", "count": "10"},
		{"customer": "Delta", "count": "3"},
	}
	sortRowsByMeasure(rows, "customer", "count")
	var got []string
	for _, row := range rows {
		got = append(got, row["customer"].(string))
	}
	want := []string{"Alpha", "Beta", "Delta", "Gamma"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestRelatedOrderByKeepsMeasureOrder(t *testing.T) {
	quote := func(name string) string { return "`" + name + "`" }
	cases := []struct{ order, want string }{
		{order: "", want: "`dimension`"},
		{order: OrderMeasureDesc, want: "`value` DESC, `dimension`"},
	}
	for _, tc := range cases {
		if got := relatedOrderBy(quote, tc.order); got != tc.want {
			t.Fatalf("relatedOrderBy(%q) = %q, want %q", tc.order, got, tc.want)
		}
	}
}

func TestRelatedMeasureOrderRanksBeforeLimit(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("sqlmock:", err)
	}
	defer database.Close()
	queryPattern := regexp.QuoteMeta("ORDER BY `value` DESC, `dimension` LIMIT ?")
	args := []driver.Value{"Sale", "items", "2026-10-01", "2026-11-01", "Paid", MaxSemanticResultRows}
	mock.ExpectQuery(queryPattern).WithArgs(args...).WillReturnRows(sqlmock.NewRows([]string{"dimension", "value"}).AddRow("Fanta", 9.0).AddRow("Coca-Cola", 6.0).AddRow("Water", 1.0))
	mock.ExpectQuery(queryPattern).WithArgs(args...).WillReturnRows(sqlmock.NewRows([]string{"dimension", "value"}).AddRow("Water", 900.0).AddRow("Coca-Cola", 600.0).AddRow("Fanta", 50.0))

	report := salesItemsTestReport()
	catalog := &SemanticCatalog{Models: []SemanticModel{}}
	if addErr := AddReportModels(catalog, []ReportDefinition{report}); addErr != nil {
		t.Fatal("add report model:", addErr)
	}
	catalog.Reports = []ReportDefinition{report}
	engine := &QueryEngine{DB: database, SiteName: "demo", Dialect: kdb.Resolve("mysql")}
	response, err := engine.ResolveSemanticQuery(catalog, SemanticRequest{
		Queries: []ModelQuery{{Model: "sales_items", Measures: []string{"quantity_sold", "sales_total"}, Dimensions: []string{"product"}, Order: OrderMeasureDesc}},
		Time:    QueryTime{From: "2026-10-01", To: "2026-10-31"},
		Limit:   2,
	}, salesItemsTestRegistry())
	if err != nil {
		t.Fatal("resolve ranked related query:", err)
	}
	var got []any
	for _, row := range response.Queries[0].Rows {
		got = append(got, row["product"])
	}
	if !reflect.DeepEqual(got, []any{"Fanta", "Coca-Cola"}) {
		t.Fatalf("ranked products = %v, want [Fanta Coca-Cola]", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}
