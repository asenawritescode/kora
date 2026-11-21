//go:build integration

package metrics

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/facts"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// TestStatusGroupsAndSnapshotCountsAcrossDialects runs against a declared fact model with no measures: a
// record type with no number field. It checks grouping by status, a count of all records, and a balance
// count, which is the number of records in a status as of the end of the range, history included.
func TestStatusGroupsAndSnapshotCountsAcrossDialects(t *testing.T) {
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
			expense := &doctype.DocType{Name: "Expense", Fields: []doctype.Field{
				{Fieldname: "amount", Label: "Amount", Fieldtype: "Currency"},
				{Fieldname: "supplier", Label: "Supplier", Fieldtype: "Link"},
				{Fieldname: "status", Fieldtype: "Select", Options: "Draft\nPaid"},
			}, Facts: &doctype.FactModel{Time: "creation", StatusField: "status", Dimensions: []doctype.FactDimension{{Field: "supplier"}}}}
			if problems := expense.Facts.Validate(expense, map[string]*doctype.DocType{"Expense": expense}); len(problems) != 0 {
				t.Fatalf("zero-measure model rejected: %v", problems)
			}
			reg := doctype.NewRegistry()
			reg.Register(expense)
			ctx := context.Background()
			if _, err := facts.RebuildModel(ctx, facts.RebuildRequest{DB: database.DB, Dialect: dialect, Registry: reg, Site: "test", Model: "Expense"}); err != nil {
				t.Fatalf("rebuild: %v", err)
			}
			refresher := &facts.Refresher{DB: database.DB, Dialect: dialect, Registry: reg, Admission: openLane{}, Owner: "status-test"}
			if _, err := refresher.RefreshDue(ctx); err != nil {
				t.Fatalf("refresh summaries: %v", err)
			}
			defs := []Metric{
				{Name: "expenses", Label: "Expenses", Type: KindSimple, Model: "Expense", Measure: &Measure{Agg: aggCount}},
				{Name: "paid_now", Label: "Paid now", Type: KindBalance, Model: "Expense", Measure: &Measure{Agg: aggCount},
					Filters: []Filter{{Field: "status", Op: "=", Value: "Paid"}}},
			}
			if err := Validate(defs, []*doctype.DocType{expense}); err != nil {
				t.Fatalf("metrics do not validate: %v", err)
			}
			engine := Engine{DB: database.DB, Dialect: dialect, Catalog: NewCatalog(defs, []*doctype.DocType{expense})}
			day := func(d int) tenanttime.Date { return tenanttime.Date{Year: 2026, Month: 11, Day: d} }
			tests := []struct {
				name  string
				query Query
				want  []string
			}{
				{"total is summed over the range", Query{Metrics: []string{"expenses"}, From: day(18), To: day(19), Grain: GrainRange},
					[]string{"2026-11-18||4"}},
				{"grouped by status", Query{Metrics: []string{"expenses"}, Dimensions: []string{"status"}, From: day(18), To: day(19), Grain: GrainRange},
					[]string{"2026-11-18|Draft|1", "2026-11-18|Paid|3"}},
				{"balance by day counts history", Query{Metrics: []string{"paid_now"}, From: day(18), To: day(19), Grain: GrainDay},
					[]string{"2026-11-18||2", "2026-11-19||3"}},
				{"balance with no days in range counts history", Query{Metrics: []string{"paid_now"}, From: day(19), To: day(19), Grain: GrainRange},
					[]string{"2026-11-19||3"}},
			}
			for _, tc := range tests {
				t.Run(tc.name, func(t *testing.T) {
					res, err := engine.Query(ctx, tc.query)
					if err != nil {
						t.Fatalf("query: %v", err)
					}
					got := renderStatusRows(res.Rows)
					if !slices.Equal(got, tc.want) {
						t.Fatalf("rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
					}
				})
			}
		})
	}
}

// renderStatusRows formats each bucket as period|group|value, sorted.
func renderStatusRows(rows []Row) []string {
	var out []string
	for _, row := range rows {
		for _, v := range row.Values {
			out = append(out, fmt.Sprintf("%s|%s|%s", row.Period.String(), strings.Join(row.Group, ","), v.Decimal.String()))
		}
	}
	slices.Sort(out)
	return out
}
