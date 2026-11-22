//go:build integration

package metrics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/facts"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// wantGroupRow is one expected bucket: its group values, its day, and the count and sum of the two
// ad-hoc measures. label is the title of the group's record, when the query labels it.
type wantGroupRow struct {
	group, day, count, sum, label string
}

// TestDimensionPathsAcrossDialects groups the Expense fact rows through the Supplier and Region records they link
// to. The values are a hand computation over expenseRows: S1 is Nairobi in Kiambu, S2 is Mombasa.
func TestDimensionPathsAcrossDialects(t *testing.T) {
	tests := []struct {
		driver, dialect, envVar string
	}{
		{"mysql", "mysql", "KORA_METRICS_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_METRICS_DISPOSABLE_POSTGRES_DSN"},
	}
	for _, tc := range tests {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.envVar)
			if dsn == "" {
				t.Skipf("set %s to run", tc.envVar)
			}
			database := disposabledb.Open(t, tc.driver, dsn)
			dialect := db.Resolve(tc.dialect)
			seedExpenses(t, database.DB, dialect)
			seedSuppliers(t, database.DB, dialect)
			engine, names := pathEngine(t, database.DB, dialect)
			runPathCases(t, engine, names)
		})
	}
}

// pathEngine rebuilds the Expense facts and returns an engine over the catalog with the ad-hoc count
// and the amount sum.
func pathEngine(t *testing.T, conn *sql.DB, dialect db.Dialect) (Engine, []string) {
	t.Helper()
	ctx := context.Background()
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Expense", Fields: []doctype.Field{
		{Fieldname: "amount", Label: "Amount", Fieldtype: "Currency"},
		{Fieldname: "supplier", Label: "Supplier", Fieldtype: "Link", Options: "Supplier"},
		{Fieldname: "status", Fieldtype: "Select"},
	}})
	reg.Register(&doctype.DocType{Name: "Supplier", TitleField: "supplier_name", Fields: []doctype.Field{
		{Fieldname: "supplier_name", Label: "Supplier name", Fieldtype: "Data"},
		{Fieldname: "town", Label: "Town", Fieldtype: "Data"},
		{Fieldname: "region", Label: "Region", Fieldtype: "Link", Options: "Region"},
	}})
	reg.Register(&doctype.DocType{Name: "Region", Fields: []doctype.Field{{Fieldname: "county", Label: "County", Fieldtype: "Data"}}})
	if _, err := facts.RebuildModel(ctx, facts.RebuildRequest{DB: conn, Dialect: dialect, Registry: reg, Site: "test", Model: "Expense"}); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	refresher := &facts.Refresher{DB: conn, Dialect: dialect, Registry: reg, Admission: openLane{}, Owner: "explore-test"}
	if _, err := refresher.RefreshDue(ctx); err != nil {
		t.Fatalf("refresh summaries: %v", err)
	}
	cat, names, err := NewCatalog(nil, reg.All()).WithAdHoc([]AdHoc{
		{Model: "Expense", Agg: aggCount},
		{Model: "Expense", Agg: aggSum, Field: "amount"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return Engine{DB: conn, Dialect: dialect, Catalog: cat}, names
}

// runPathCases runs each grouping and checks every bucket against the hand computation.
func runPathCases(t *testing.T, engine Engine, names []string) {
	t.Helper()
	ctx := context.Background()
	both := map[string]bool{"Supplier": true, "Region": true}
	tests := []struct {
		name     string
		dim      string
		joinable map[string]bool
		want     []wantGroupRow
	}{
		{
			name: "one hop groups by the linked record's town", dim: "supplier.town", joinable: map[string]bool{"Supplier": true},
			want: []wantGroupRow{
				{"Mombasa", "18", "1", "50", ""}, {"Mombasa", "19", "0", "0", ""},
				{"Nairobi", "18", "2", "1099", ""}, {"Nairobi", "19", "1", "25.5", ""},
			},
		},
		{
			name: "two hops group by the county of the supplier's region", dim: "supplier.region.county", joinable: both,
			want: []wantGroupRow{
				{"Kiambu", "18", "2", "1099", ""}, {"Kiambu", "19", "1", "25.5", ""},
				{"Mombasa County", "18", "1", "50", ""}, {"Mombasa County", "19", "0", "0", ""},
			},
		},
		{
			name: "a Link dimension shows the linked record's title", dim: "supplier", joinable: map[string]bool{"Supplier": true},
			want: []wantGroupRow{
				{"S1", "18", "2", "1099", "Acme Ltd"}, {"S1", "19", "1", "25.5", "Acme Ltd"},
				{"S2", "18", "1", "50", "Beta Ltd"}, {"S2", "19", "0", "0", "Beta Ltd"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := engine.Query(ctx, pathQueryFor(names, tc.dim, tc.joinable))
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			checkGroupRows(t, res, tc.want)
		})
	}
	t.Run("a record type the caller may not join is refused", func(t *testing.T) {
		_, err := engine.Query(ctx, pathQueryFor(names, "supplier.town", nil))
		if !errors.Is(err, ErrForbidden) {
			t.Errorf("error = %v, want ErrForbidden", err)
		}
	})
	t.Run("a Link dimension without joins has no labels", func(t *testing.T) {
		res, err := engine.Query(ctx, pathQueryFor(names, "supplier", nil))
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if res.Rows[0].Labels != nil {
			t.Errorf("labels = %v, want none", res.Rows[0].Labels)
		}
	})
	t.Run("three links are refused", func(t *testing.T) {
		_, err := engine.Query(ctx, pathQueryFor(names, "supplier.region.county.name", both))
		if err == nil || !strings.Contains(err.Error(), "at most 2") {
			t.Errorf("error = %v, want the hop limit", err)
		}
	})
}

// pathQueryFor is the two-day query of one grouping, for the scope that may join to joinable.
func pathQueryFor(names []string, dim string, joinable map[string]bool) Query {
	return Query{Metrics: names, Dimensions: []string{dim},
		From: tenanttime.Date{Year: 2026, Month: 11, Day: 18}, To: tenanttime.Date{Year: 2026, Month: 11, Day: 19},
		Grain: GrainDay, Scope: Scope{Joinable: joinable}}
}

// checkGroupRows compares every bucket, in order, with its expectation.
func checkGroupRows(t *testing.T, res Result, want []wantGroupRow) {
	t.Helper()
	if len(res.Rows) != len(want) {
		t.Fatalf("rows = %d, want %d", len(res.Rows), len(want))
	}
	for i, row := range res.Rows {
		w := want[i]
		got := []string{row.Group[0], row.Period.String()[8:], valueText(row.Values[0]), valueText(row.Values[1]), labelText(row)}
		exp := []string{w.group, w.day, w.count, w.sum, w.label}
		if strings.Join(got, "|") != strings.Join(exp, "|") {
			t.Errorf("row %d = %v, want %v", i, got, exp)
		}
	}
}

// valueText renders a value for comparison, as a decimal that equals its expectation.
func valueText(v decimal.NullDecimal) string {
	if !v.Valid {
		return "null"
	}
	return v.Decimal.String()
}

// labelText is the title of the row's first group, or empty when the row is unlabelled.
func labelText(row Row) string {
	if len(row.Labels) == 0 {
		return ""
	}
	return row.Labels[0]
}

// seedSuppliers creates the Supplier and Region tables and their records. S1 is Acme Ltd in Nairobi,
// in the region R1 (Kiambu); S2 is Beta Ltd in Mombasa, in R2 (Mombasa County).
func seedSuppliers(t *testing.T, conn *sql.DB, dialect db.Dialect) {
	t.Helper()
	ctx := context.Background()
	q := dialect.QuoteIdent
	ddl := []string{
		fmt.Sprintf("CREATE TABLE %s (name VARCHAR(140) NOT NULL PRIMARY KEY, supplier_name VARCHAR(140) NOT NULL, town VARCHAR(140) NOT NULL, region VARCHAR(140) NOT NULL)", q("tabSupplier")),
		fmt.Sprintf("CREATE TABLE %s (name VARCHAR(140) NOT NULL PRIMARY KEY, county VARCHAR(140) NOT NULL)", q("tabRegion")),
	}
	for _, stmt := range ddl {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("create %q: %v", stmt, err)
		}
	}
	inserts := []struct {
		query string
		args  []any
	}{
		{fmt.Sprintf("INSERT INTO %s (name, supplier_name, town, region) VALUES (?, ?, ?, ?)", q("tabSupplier")), []any{"S1", "Acme Ltd", "Nairobi", "R1"}},
		{fmt.Sprintf("INSERT INTO %s (name, supplier_name, town, region) VALUES (?, ?, ?, ?)", q("tabSupplier")), []any{"S2", "Beta Ltd", "Mombasa", "R2"}},
		{fmt.Sprintf("INSERT INTO %s (name, county) VALUES (?, ?)", q("tabRegion")), []any{"R1", "Kiambu"}},
		{fmt.Sprintf("INSERT INTO %s (name, county) VALUES (?, ?)", q("tabRegion")), []any{"R2", "Mombasa County"}},
	}
	for _, in := range inserts {
		if _, err := conn.ExecContext(ctx, db.Rebind(dialect, in.query), in.args...); err != nil {
			t.Fatalf("insert %q: %v", in.query, err)
		}
	}
}
