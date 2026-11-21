//go:build integration

package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/facts"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// expenseRows are the source records of a record type with no pack facts. All are created on 18 November
// (09:00 UTC is 12:00 Nairobi) except EXP-4, created on 19 November.
var expenseRows = []struct{ name, created, status, supplier, amount string }{
	{"EXP-1", "2026-11-18 09:00:00", "Paid", "S1", "100.00"},
	{"EXP-2", "2026-11-18 09:30:00", "Paid", "S2", "50.00"},
	{"EXP-3", "2026-11-18 10:00:00", "Draft", "S1", "999.00"},
	{"EXP-4", "2026-11-19 09:00:00", "Paid", "S1", "25.50"},
}

// TestAdHocMeasuresOverAGeneratedModel rebuilds the fact table of a record type with no pack facts, then
// asks for unsaved measures of it. The values are a hand computation over expenseRows.
func TestAdHocMeasuresOverAGeneratedModel(t *testing.T) {
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
			ctx := context.Background()
			seedExpenses(t, database.DB, dialect)
			reg := doctype.NewRegistry()
			reg.Register(&doctype.DocType{Name: "Expense", Fields: []doctype.Field{
				{Fieldname: "amount", Label: "Amount", Fieldtype: "Currency"},
				{Fieldname: "supplier", Label: "Supplier", Fieldtype: "Link"},
				{Fieldname: "status", Fieldtype: "Select"},
			}})
			if dt := reg.Get("Expense"); dt.Facts == nil || !dt.Facts.Generated {
				t.Fatalf("Expense facts = %+v, want generated", dt.Facts)
			}
			if _, err := facts.RebuildModel(ctx, facts.RebuildRequest{DB: database.DB, Dialect: dialect, Registry: reg, Site: "test", Model: "Expense"}); err != nil {
				t.Fatalf("rebuild: %v", err)
			}
			refresher := &facts.Refresher{DB: database.DB, Dialect: dialect, Registry: reg, Admission: openLane{}, Owner: "explore-test"}
			if _, err := refresher.RefreshDue(ctx); err != nil {
				t.Fatalf("refresh summaries: %v", err)
			}
			cat, names, err := NewCatalog(nil, reg.All()).WithAdHoc([]AdHoc{
				{Model: "Expense", Agg: aggCount},
				{Model: "Expense", Agg: aggSum, Field: "amount"},
				{Model: "Expense", Agg: aggSum, Field: "amount", Filters: []Filter{{Field: "status", Op: "=", Value: "Paid"}}},
				{Model: "Expense", Agg: aggCountDistinct, Field: "supplier"},
			})
			if err != nil {
				t.Fatal(err)
			}
			engine := Engine{DB: database.DB, Dialect: dialect, Catalog: cat}
			res, err := engine.Query(ctx, Query{Metrics: names, From: tenanttime.Date{Year: 2026, Month: 11, Day: 18},
				To: tenanttime.Date{Year: 2026, Month: 11, Day: 19}, Grain: GrainDay})
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			// Per day: count, sum, paid sum, distinct suppliers. EXP-3 is a draft: it counts and sums,
			// but it is not Paid.
			want := [][]string{
				{"3", "1149", "150", "2"},
				{"1", "25.5", "25.5", "1"},
			}
			if len(res.Rows) != len(want) {
				t.Fatalf("rows = %d, want %d", len(res.Rows), len(want))
			}
			for i, row := range res.Rows {
				for j, v := range row.Values {
					if !v.Valid || !v.Decimal.Equal(decimal.RequireFromString(want[i][j])) {
						t.Errorf("day %d metric %s = %v, want %s", i, names[j], v, want[i][j])
					}
				}
			}
			if got := cat.Metrics[names[0]].Label; got != "Records of Expense" {
				t.Errorf("count label = %q", got)
			}
			if got := cat.Metrics[names[2]].Label; got != "Amount of Expense where status = Paid" {
				t.Errorf("filtered label = %q", got)
			}
		})
	}
}

// seedExpenses creates the source table and the site settings a rebuild reads, then the records.
func seedExpenses(t *testing.T, conn *sql.DB, dialect db.Dialect) {
	t.Helper()
	ctx := context.Background()
	q := dialect.QuoteIdent
	steps := []struct {
		query string
		args  []any
	}{
		{fmt.Sprintf("CREATE TABLE %s (name VARCHAR(140) NOT NULL PRIMARY KEY, owner VARCHAR(140) NOT NULL, creation TIMESTAMP NOT NULL, status VARCHAR(40) NOT NULL, supplier VARCHAR(140) NOT NULL, amount DECIMAL(21,9) NOT NULL)", q("tabExpense")), nil},
		{"CREATE TABLE _kora_site_setting (site VARCHAR(140) NOT NULL, setting_key VARCHAR(140) NOT NULL, setting_value VARCHAR(140) NOT NULL, PRIMARY KEY (site, setting_key))", nil},
		{db.Rebind(dialect, "INSERT INTO _kora_site_setting (site, setting_key, setting_value) VALUES (?, ?, ?)"), []any{"test", tenanttime.KeyTimeZone, "Africa/Nairobi"}},
		{"CREATE TABLE _kora_tenant_event_sequence (id INT PRIMARY KEY, last_sequence BIGINT NOT NULL DEFAULT 0)", nil},
		{"INSERT INTO _kora_tenant_event_sequence (id, last_sequence) VALUES (1, 0)", nil},
	}
	for _, s := range steps {
		if _, err := conn.ExecContext(ctx, s.query, s.args...); err != nil {
			t.Fatalf("setup %q: %v", s.query, err)
		}
	}
	insert := db.Rebind(dialect, fmt.Sprintf("INSERT INTO %s (name, owner, creation, status, supplier, amount) VALUES (?, 'u1', ?, ?, ?, ?)", q("tabExpense")))
	for _, r := range expenseRows {
		if _, err := conn.ExecContext(ctx, insert, r.name, r.created, r.status, r.supplier, r.amount); err != nil {
			t.Fatalf("insert %s: %v", r.name, err)
		}
	}
}
