package metrics

import (
	"errors"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

// totalFixture is a Sale with a header total and a line total, and the line dimension and metrics to split them by.
func totalFixture(t *testing.T) Catalog {
	t.Helper()
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Sale", Fields: []doctype.Field{
		{Fieldname: "status", Fieldtype: "Select"},
		{Fieldname: "total", Label: "Total", Fieldtype: "Currency"},
		{Fieldname: "items", Label: "Items", Fieldtype: "Table", Options: "Sale Item"},
	}, Facts: &doctype.FactModel{Time: "creation", StatusField: "status", Lines: "items",
		Measures:   []doctype.FactMeasure{{Field: "total"}, {Field: "items.line_total"}},
		Dimensions: []doctype.FactDimension{{Field: "items.product"}}}})
	reg.Register(&doctype.DocType{Name: "Sale Item", IsChildTable: true, Fields: []doctype.Field{
		{Fieldname: "product", Label: "Product", Fieldtype: "Link", Options: "Product"},
		{Fieldname: "line_total", Label: "Line total", Fieldtype: "Currency"},
	}})
	reg.Register(&doctype.DocType{Name: "Product", TitleField: "product_name", Fields: []doctype.Field{
		{Fieldname: "product_name", Label: "Product name", Fieldtype: "Data"},
	}})
	docs := reg.All()
	defs := []Metric{
		{Name: "sales", Label: "Sales", Type: KindSimple, Model: "Sale", Measure: &Measure{Agg: aggSum, Field: "total"},
			Filters: []Filter{{Field: "status", Op: "in", Value: []any{"Paid"}}}},
		{Name: "sales_by_line", Label: "Lines", Type: KindSimple, Model: "Sale", Measure: &Measure{Agg: aggSum, Field: "line_total"}},
		{Name: "bills", Label: "Bills", Type: KindSimple, Model: "Sale", Measure: &Measure{Agg: aggCount}},
	}
	return NewCatalog(defs, docs)
}

// TestPlanRefusesHeaderTotalSplitByLine is the rule that a header total cannot be split by a line dimension.
func TestPlanRefusesHeaderTotalSplitByLine(t *testing.T) {
	cat := totalFixture(t)
	tests := []struct {
		name    string
		metric  string
		dims    []string
		wantErr bool
	}{
		{"header total by a line dimension is refused", "sales", []string{"items.product"}, true},
		{"header total by a path through a line dimension is refused", "sales", []string{"items.product.product_name"}, true},
		{"header total by a header dimension is allowed", "sales", []string{"status"}, false},
		{"line total by a line dimension is allowed", "sales_by_line", []string{"items.product"}, false},
		{"count by a line dimension is allowed: it counts records", "bills", []string{"items.product"}, false},
		{"header total with no dimension is allowed", "sales", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := pathQuery(tc.dims, "Product")
			q.Metrics = []string{tc.metric}
			_, err := planQuery(cat, q)
			if tc.wantErr {
				if !errors.Is(err, ErrMeasureNotOnLines) {
					t.Fatalf("err = %v, want ErrMeasureNotOnLines", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
		})
	}
}

// TestGroupKeepClause checks the HAVING clause that drops groups no metric condition matches.
func TestGroupKeepClause(t *testing.T) {
	cat := totalFixture(t)
	tests := []struct {
		name     string
		metric   string
		dims     []string
		wantKeep bool
	}{
		{"a metric with a status condition keeps only groups it matches", "sales", []string{"status"}, true},
		{"a count keeps the groups that have a header row", "bills", []string{"status"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := pathQuery(tc.dims, "Product")
			q.Metrics = []string{tc.metric}
			p, err := planQuery(cat, q)
			if err != nil {
				t.Fatal(err)
			}
			stmt, _ := p.build(db.Resolve("mysql"))
			if got := strings.Contains(stmt, "HAVING MAX(CASE WHEN"); got != tc.wantKeep {
				t.Errorf("HAVING present = %v, want %v:\n%s", got, tc.wantKeep, stmt)
			}
		})
	}
}

// TestAdHocFormatFromField checks an unsaved measure carries the display format of its field.
func TestAdHocFormatFromField(t *testing.T) {
	cat := totalFixture(t)
	tests := []struct {
		name string
		a    AdHoc
		want string
	}{
		{"a sum of a Currency field is money", AdHoc{Model: "Sale", Agg: aggSum, Field: "total"}, "currency"},
		{"a sum of a line Currency field is money", AdHoc{Model: "Sale", Agg: aggSum, Field: "line_total"}, "currency"},
		{"a count is a plain number", AdHoc{Model: "Sale", Agg: aggCount}, "number"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := cat.adHocMetric(tc.a)
			if err != nil {
				t.Fatal(err)
			}
			if m.Format != tc.want {
				t.Errorf("format = %q, want %q", m.Format, tc.want)
			}
		})
	}
}

// TestExploreLevels checks measures and dimensions say whether they read header or line rows.
func TestExploreLevels(t *testing.T) {
	cat := totalFixture(t)
	rec, ok := cat.ExploreRecord("Sale", map[string]bool{"Product": true})
	if !ok {
		t.Fatal("Sale not offered")
	}
	levels := map[string]string{}
	for _, m := range rec.Measures {
		levels[m.Key] = m.Level
	}
	for _, d := range rec.Dimensions {
		levels[d.Path] = d.Level
	}
	want := map[string]string{"sum:total": levelHeader, "count": "", "status": levelHeader, "items.product": levelLine}
	for key, level := range want {
		if levels[key] != level {
			t.Errorf("level of %s = %q, want %q", key, levels[key], level)
		}
	}
	if levels["items.product.product_name"] != levelLine {
		t.Errorf("a path through a line dimension level = %q, want line", levels["items.product.product_name"])
	}
}
