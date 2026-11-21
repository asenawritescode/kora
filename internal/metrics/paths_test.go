package metrics

import (
	"errors"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// pathFixture is an Expense that links to Supplier, which links to Region. Supplier is titled by its
// supplier_name; Region has no title field, so its title is its name.
func pathFixture(t *testing.T) (Catalog, []*doctype.DocType) {
	t.Helper()
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Expense", Fields: []doctype.Field{
		{Fieldname: "amount", Label: "Amount", Fieldtype: "Currency"},
		{Fieldname: "supplier", Label: "Supplier", Fieldtype: "Link", Options: "Supplier"},
		{Fieldname: "status", Fieldtype: "Select"},
	}, Facts: &doctype.FactModel{Time: "creation", StatusField: "status",
		Measures:   []doctype.FactMeasure{{Field: "amount"}},
		Dimensions: []doctype.FactDimension{{Field: "supplier"}}}})
	reg.Register(&doctype.DocType{Name: "Supplier", TitleField: "supplier_name", Fields: []doctype.Field{
		{Fieldname: "supplier_name", Label: "Supplier name", Fieldtype: "Data"},
		{Fieldname: "town", Label: "Town", Fieldtype: "Data"},
		{Fieldname: "region", Label: "Region", Fieldtype: "Link", Options: "Region"},
	}})
	reg.Register(&doctype.DocType{Name: "Region", Fields: []doctype.Field{{Fieldname: "county", Label: "County", Fieldtype: "Data"}}})
	docs := reg.All()
	count := Metric{Name: "expenses", Type: KindSimple, Model: "Expense", Measure: &Measure{Agg: aggCount}}
	return NewCatalog([]Metric{count}, docs), docs
}

// pathQuery is one day of Expense counts, grouped by dims, for a scope that may join to joinable.
func pathQuery(dims []string, joinable ...string) Query {
	scope := Scope{Joinable: map[string]bool{}}
	for _, j := range joinable {
		scope.Joinable[j] = true
	}
	day := tenanttime.Date{Year: 2026, Month: 11, Day: 18}
	return Query{Metrics: []string{"expenses"}, Dimensions: dims, From: day, To: day, Grain: GrainDay, Scope: scope}
}

func TestPlanDimensionPaths(t *testing.T) {
	cat, _ := pathFixture(t)
	tests := []struct {
		name       string
		dims       []string
		joinable   []string
		wantJoins  int
		wantLeaf   string
		wantLabel  string // record type whose titles label the group; empty for none
		wantErr    string
		forbidden  bool
		wantSummry bool
	}{
		{name: "fact link without a path is labelled and stays on the summary", dims: []string{"supplier"},
			joinable: []string{"Supplier"}, wantLabel: "Supplier", wantSummry: true},
		{name: "fact link labels nothing when the record type is not joinable", dims: []string{"supplier"}, wantSummry: true},
		{name: "one hop reads a field of the linked record", dims: []string{"supplier.town"},
			joinable: []string{"Supplier"}, wantJoins: 1, wantLeaf: "town"},
		{name: "two hops reach through a second link", dims: []string{"supplier.region.county"},
			joinable: []string{"Supplier", "Region"}, wantJoins: 2, wantLeaf: "county"},
		{name: "a link leaf is labelled by its record type", dims: []string{"supplier.region"},
			joinable: []string{"Supplier", "Region"}, wantJoins: 1, wantLeaf: "region", wantLabel: "Region"},
		{name: "three links are refused", dims: []string{"supplier.region.county.x"}, joinable: []string{"Supplier", "Region"},
			wantErr: "at most 2"},
		{name: "a path needs every record type joinable", dims: []string{"supplier.region.county"}, joinable: []string{"Supplier"},
			forbidden: true},
		{name: "a path into a record type the caller may not read is refused", dims: []string{"supplier.town"}, forbidden: true},
		{name: "a path starts at a fact dimension", dims: []string{"amount.town"}, joinable: []string{"Supplier"},
			wantErr: "not a facts dimension"},
		{name: "the leaf must be a dimension field", dims: []string{"supplier.amount"}, joinable: []string{"Supplier"},
			wantErr: "not a dimension field"},
		{name: "the hop must be a Link field", dims: []string{"supplier.supplier_name.town"}, joinable: []string{"Supplier"},
			wantErr: "not a Link field"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := planQuery(cat, pathQuery(tc.dims, tc.joinable...))
			if tc.wantErr != "" || tc.forbidden {
				checkPlanError(t, err, tc.wantErr, tc.forbidden)
				return
			}
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			dim := p.dims[0]
			if len(dim.joins) != tc.wantJoins || dim.leaf != tc.wantLeaf {
				t.Errorf("joins = %d leaf = %q, want %d %q", len(dim.joins), dim.leaf, tc.wantJoins, tc.wantLeaf)
			}
			if got := labelName(dim); got != tc.wantLabel {
				t.Errorf("label = %q, want %q", got, tc.wantLabel)
			}
			if p.summary != tc.wantSummry {
				t.Errorf("summary = %v, want %v", p.summary, tc.wantSummry)
			}
		})
	}
}

// checkPlanError asserts a planning error: forbidden names ErrForbidden, otherwise the message holds want.
func checkPlanError(t *testing.T, err error, want string, forbidden bool) {
	t.Helper()
	if err == nil {
		t.Fatal("plan succeeded, want a refusal")
	}
	if forbidden && !errors.Is(err, ErrForbidden) {
		t.Errorf("error = %v, want ErrForbidden", err)
	}
	if want != "" && !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want it to mention %q", err, want)
	}
}

func labelName(dim dimension) string {
	if dim.label == nil {
		return ""
	}
	return dim.label.Name
}

func TestPathSQLJoinsTheLinkedRecords(t *testing.T) {
	cat, _ := pathFixture(t)
	p, err := planQuery(cat, pathQuery([]string{"supplier.region.county"}, "Supplier", "Region"))
	if err != nil {
		t.Fatal(err)
	}
	stmt, _ := p.build(db.Resolve("mysql"))
	for _, want := range []string{
		"LEFT JOIN `tabSupplier` AS _d0_0 ON _d0_0.`name` = t.`supplier`",
		"LEFT JOIN `tabRegion` AS _d0_1 ON _d0_1.`name` = _d0_0.`region`",
		"_d0_1.`county` AS `_path0`",
		"FROM `_kora_fact_expense` AS t",
	} {
		if !strings.Contains(stmt, want) {
			t.Errorf("statement lacks %q:\n%s", want, stmt)
		}
	}
	if strings.Contains(stmt, "_kora_fact_summary_") {
		t.Errorf("a path must not read the summary:\n%s", stmt)
	}
}

func TestExploreCatalogOffersLinkedDimensions(t *testing.T) {
	_, docs := pathFixture(t)
	read := readAll(docs)
	got := ExploreCatalog(docs, read, map[string]bool{"Supplier": true, "Region": true})
	var expense []string
	for _, rec := range got {
		if rec.RecordType != "Expense" {
			continue
		}
		for _, d := range rec.Dimensions {
			expense = append(expense, d.Path+"|"+itoa(d.Hops)+"|"+d.Group)
		}
	}
	want := []string{
		"supplier|0|",
		"supplier.supplier_name|1|Supplier ›",
		"supplier.town|1|Supplier ›",
		"supplier.region|1|Supplier ›",
		"supplier.region.county|2|Supplier › Region ›",
	}
	if strings.Join(expense, "\\n") != strings.Join(want, "\\n") {
		t.Errorf("dimensions =\\n%s\\nwant\\n%s", strings.Join(expense, "\\n"), strings.Join(want, "\\n"))
	}
	// Without Region joinable, the two-hop path is not offered; the one-hop Region link leaf still is.
	narrow := ExploreCatalog(docs, read, map[string]bool{"Supplier": true})
	for _, d := range narrow[0].Dimensions {
		if d.Hops == 2 {
			t.Errorf("two-hop %q offered without Region joinable", d.Path)
		}
	}
}

func itoa(n int) string {
	return string(rune('0' + n))
}

// lineFixture is a Sale whose child table Sale Item links to Product; Product is categorised by a Select.
func lineFixture(t *testing.T) (Catalog, []*doctype.DocType) {
	t.Helper()
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Sale", Fields: []doctype.Field{
		{Fieldname: "status", Fieldtype: "Select"},
		{Fieldname: "items", Label: "Items", Fieldtype: "Table", Options: "Sale Item"},
	}, Facts: &doctype.FactModel{Time: "creation", StatusField: "status", Lines: "items",
		Measures:   []doctype.FactMeasure{{Field: "items.line_total"}},
		Dimensions: []doctype.FactDimension{{Field: "items.product"}}}})
	reg.Register(&doctype.DocType{Name: "Sale Item", IsChildTable: true, Fields: []doctype.Field{
		{Fieldname: "product", Label: "Product", Fieldtype: "Link", Options: "Product"},
		{Fieldname: "line_total", Fieldtype: "Currency"},
	}})
	reg.Register(&doctype.DocType{Name: "Product", TitleField: "product_name", Fields: []doctype.Field{
		{Fieldname: "product_name", Label: "Product name", Fieldtype: "Data"},
		{Fieldname: "category", Label: "Category", Fieldtype: "Select"},
	}})
	docs := reg.All()
	count := Metric{Name: "expenses", Type: KindSimple, Model: "Sale", Measure: &Measure{Agg: aggCount}}
	return NewCatalog([]Metric{count}, docs), docs
}

func TestPlanLineDimensionPaths(t *testing.T) {
	cat, _ := lineFixture(t)
	tests := []struct {
		name      string
		dims      []string
		joinable  []string
		wantJoins int
		wantLeaf  string
		wantLabel string
		wantErr   string
		forbidden bool
	}{
		{name: "a line dimension is a plain dimension, labelled by its record", dims: []string{"items.product"},
			joinable: []string{"Product"}, wantLabel: "Product"},
		{name: "a path from a line dimension follows its link", dims: []string{"items.product.category"},
			joinable: []string{"Product"}, wantJoins: 1, wantLeaf: "category"},
		{name: "a line path needs the linked type joinable", dims: []string{"items.product.category"}, forbidden: true},
		{name: "a path ends on a dimension field", dims: []string{"items.product.product_name.x"}, joinable: []string{"Product"},
			wantErr: "not a Link field"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := planQuery(cat, pathQuery(tc.dims, tc.joinable...))
			if tc.wantErr != "" || tc.forbidden {
				checkPlanError(t, err, tc.wantErr, tc.forbidden)
				return
			}
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			dim := p.dims[0]
			if len(dim.joins) != tc.wantJoins || dim.leaf != tc.wantLeaf || !dim.line {
				t.Errorf("joins = %d leaf = %q line = %v, want %d %q true", len(dim.joins), dim.leaf, dim.line, tc.wantJoins, tc.wantLeaf)
			}
			if got := labelName(dim); got != tc.wantLabel {
				t.Errorf("label = %q, want %q", got, tc.wantLabel)
			}
			if p.summary {
				t.Error("a line dimension must not read the summary")
			}
		})
	}
}

func TestLineDimensionReadsLinesOnly(t *testing.T) {
	cat, _ := lineFixture(t)
	p, err := planQuery(cat, pathQuery([]string{"items.product.category"}, "Product"))
	if err != nil {
		t.Fatal(err)
	}
	stmt, _ := p.build(db.Resolve("mysql"))
	if strings.Contains(stmt, "`_kora_fact_sale` AS t") || !strings.Contains(stmt, "`_kora_fact_sale_line` AS t") {
		t.Errorf("a line dimension must read the line table only:\n%s", stmt)
	}
}

func TestExploreCatalogOffersLineDimensions(t *testing.T) {
	_, docs := lineFixture(t)
	got := ExploreCatalog(docs, readAll(docs), map[string]bool{"Product": true})
	var paths []string
	for _, rec := range got {
		if rec.RecordType != "Sale" {
			continue
		}
		for _, d := range rec.Dimensions {
			paths = append(paths, d.Path+"|"+itoa(d.Hops)+"|"+d.Group)
		}
	}
	want := "items.product|0|,items.product.product_name|1|Product ›,items.product.category|1|Product ›"
	if strings.Join(paths, ",") != want {
		t.Errorf("dimensions = %v, want %s", paths, want)
	}
}
