package metrics

import (
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

// exploreFixture is a declared Sale with a child table and a generated Expense.
func exploreFixture(t *testing.T) (Catalog, []*doctype.DocType) {
	t.Helper()
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Expense", Fields: []doctype.Field{
		{Fieldname: "amount", Label: "Amount", Fieldtype: "Currency"},
		{Fieldname: "supplier", Label: "Supplier", Fieldtype: "Link"},
		{Fieldname: "status", Fieldtype: "Select"},
	}})
	reg.Register(&doctype.DocType{Name: "Sale", Description: "Till sales", Facts: &doctype.FactModel{
		Time: "creation", StatusField: "status", Measures: []doctype.FactMeasure{{Field: "total"}},
		Dimensions: []doctype.FactDimension{{Field: "register"}, {Field: "items.item"}},
	}, Fields: []doctype.Field{{Fieldname: "total", Label: "Total", Fieldtype: "Currency"}, {Fieldname: "register", Label: "Register", Fieldtype: "Data"}}})
	reg.Register(&doctype.DocType{Name: "Line", IsChildTable: true, Fields: []doctype.Field{{Fieldname: "amount", Fieldtype: "Currency"}}})
	docs := reg.All()
	return NewCatalog(nil, docs), docs
}

func TestExploreCatalogListsFactRecordTypes(t *testing.T) {
	_, docs := exploreFixture(t)
	got := ExploreCatalog(docs, readAll(docs), nil)
	if len(got) != 2 || got[0].RecordType != "Expense" || got[1].RecordType != "Sale" {
		t.Fatalf("record types = %+v, want Expense and Sale", got)
	}
	expense := got[0]
	var keys []string
	for _, m := range expense.Measures {
		keys = append(keys, m.Key)
	}
	if strings.Join(keys, ",") != "count,sum:amount,distinct:supplier" {
		t.Errorf("expense measures = %v", keys)
	}
	if expense.Measures[1].Format != "currency" || expense.Measures[1].Label != "Amount" {
		t.Errorf("amount measure = %+v, want currency labelled Amount", expense.Measures[1])
	}
	// The status field is offered once, as the status dimension, after the declared dimensions.
	if len(expense.Dimensions) != 2 || expense.Dimensions[0].Kind != "Link" || expense.Dimensions[1].Path != "status" {
		t.Errorf("expense dimensions = %+v", expense.Dimensions)
	}
	// Sale's child-table dimension is left out; its header dimension, status and measure stay.
	sale := got[1]
	if len(sale.Dimensions) != 2 || sale.Dimensions[0].Path != "register" || sale.Dimensions[1].Path != "status" {
		t.Errorf("sale dimensions = %+v, want register and status", sale.Dimensions)
	}
	if sale.Description != "Till sales" || sale.TimeField != "creation" {
		t.Errorf("sale = %+v", sale)
	}
}

func TestWithAdHocBuildsValidatedUnverifiedMetrics(t *testing.T) {
	tests := []struct {
		name      string
		measures  []AdHoc
		wantNames []string
		wantLabel string
		wantErr   string
	}{
		{name: "count of a generated record type", measures: []AdHoc{{Model: "Expense", Agg: aggCount}},
			wantNames: []string{"adhoc_expense_count"}, wantLabel: "Records of Expense"},
		{name: "sum of a header measure with a filter", measures: []AdHoc{{Model: "Expense", Agg: aggSum, Field: "amount",
			Filters: []Filter{{Field: "status", Op: "=", Value: "Paid"}}}},
			wantNames: []string{"adhoc_expense_sum_amount"}, wantLabel: "Amount of Expense where status = Paid"},
		{name: "count_distinct of a dimension on a declared model", measures: []AdHoc{{Model: "Sale", Agg: aggCountDistinct, Field: "register"}},
			wantNames: []string{"adhoc_sale_count_distinct_register"}, wantLabel: "Distinct Register of Sale"},
		{name: "a repeated measure is one metric", measures: []AdHoc{{Model: "Expense", Agg: aggCount}, {Model: "Expense", Agg: aggCount}},
			wantNames: []string{"adhoc_expense_count"}, wantLabel: "Records of Expense"},
		{name: "a filtered measure keeps its own name and label", measures: []AdHoc{{Model: "Expense", Agg: aggSum, Field: "amount"},
			{Model: "Expense", Agg: aggSum, Field: "amount", Filters: []Filter{{Field: "status", Op: "=", Value: "Paid"}}}},
			wantNames: []string{"adhoc_expense_sum_amount", "adhoc_expense_sum_amount_2"}, wantLabel: "Amount of Expense"},
		{name: "unknown record type", measures: []AdHoc{{Model: "Nope", Agg: aggCount}}, wantErr: "not defined"},
		{name: "child record type has no facts", measures: []AdHoc{{Model: "Line", Agg: aggCount}}, wantErr: "not a record type with facts"},
		{name: "sum of a field that is not a measure", measures: []AdHoc{{Model: "Expense", Agg: aggSum, Field: "supplier"}}, wantErr: "not a facts measure"},
		{name: "count_distinct of a measure", measures: []AdHoc{{Model: "Expense", Agg: aggCountDistinct, Field: "amount"}}, wantErr: "not a facts dimension"},
		{name: "unknown aggregate", measures: []AdHoc{{Model: "Expense", Agg: "avg", Field: "amount"}}, wantErr: "must be sum, count or count_distinct"},
		{name: "filter on a field that is not a dimension", measures: []AdHoc{{Model: "Expense", Agg: aggCount,
			Filters: []Filter{{Field: "amount", Op: "=", Value: "1"}}}}, wantErr: "neither the status field nor a facts dimension"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cat, _ := exploreFixture(t)
			out, names, err := cat.WithAdHoc(tc.measures)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(names, ",") != strings.Join(tc.wantNames, ",") {
				t.Fatalf("names = %v, want %v", names, tc.wantNames)
			}
			m := out.Metrics[names[0]]
			if m.Label != tc.wantLabel || m.Verified || m.Type != KindSimple {
				t.Fatalf("metric = %+v, want label %q, simple and unverified", m, tc.wantLabel)
			}
			if _, leaked := cat.Metrics[names[0]]; leaked {
				t.Fatal("WithAdHoc changed the catalog it was given")
			}
		})
	}
}

func TestWithAdHocSuffixesANameTakenByADeclaredMetric(t *testing.T) {
	cat, _ := exploreFixture(t)
	cat.Metrics = map[string]Metric{"adhoc_expense_count": {Name: "adhoc_expense_count", Label: "Pack", Type: KindSimple, Model: "Expense", Measure: &Measure{Agg: aggCount}}}
	out, names, err := cat.WithAdHoc([]AdHoc{{Model: "Expense", Agg: aggCount}})
	if err != nil || len(names) != 1 || names[0] != "adhoc_expense_count_2" {
		t.Fatalf("names = %v, %v; want adhoc_expense_count_2", names, err)
	}
	if out.Metrics["adhoc_expense_count"].Label != "Pack" {
		t.Fatal("the declared metric was replaced")
	}
}

// readAll marks every record type as readable, for tests that do not exercise permissions.
func readAll(docs []*doctype.DocType) map[string]bool {
	out := make(map[string]bool, len(docs))
	for _, dt := range docs {
		out[dt.Name] = true
	}
	return out
}

// A status field adds a Status dimension to the catalog, whether or not the record type declares measures.
func TestExploreCatalogOffersStatusWhenThereIsAStatusField(t *testing.T) {
	cases := []struct {
		name        string
		statusField string
		measures    []doctype.FactMeasure
		want        bool
	}{
		{"status field and no measures", "status", nil, true},
		{"status field and a measure", "status", []doctype.FactMeasure{{Field: "amount"}}, true},
		{"no status field", "", []doctype.FactMeasure{{Field: "amount"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dt := &doctype.DocType{Name: "Expense", Fields: []doctype.Field{
				{Fieldname: "amount", Label: "Amount", Fieldtype: "Currency"},
				{Fieldname: "status", Label: "Status", Fieldtype: "Select"},
			}, Facts: &doctype.FactModel{Time: "creation", StatusField: tc.statusField, Measures: tc.measures}}
			cat := ExploreCatalog([]*doctype.DocType{dt}, map[string]bool{"Expense": true}, nil)
			if len(cat) != 1 {
				t.Fatalf("catalog = %d record types, want 1", len(cat))
			}
			got := false
			for _, d := range cat[0].Dimensions {
				if d.Path == "status" {
					got = d.Label == "Status" && d.Kind == "Select"
				}
			}
			if got != tc.want {
				t.Fatalf("status dimension offered = %v, want %v", got, tc.want)
			}
		})
	}
}
