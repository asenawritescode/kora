//go:build integration

package metrics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// TestHeaderTotalsAndGroupsAcrossDialects runs the accuracy rules over the selling pack's real Sale facts.
// A header total cannot be split by a line dimension. A status group that no metric condition matches
// (SAL-C, a Return) is not shown at zero.
func TestHeaderTotalsAndGroupsAcrossDialects(t *testing.T) {
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
			dts := erpsellDoctypes(t)
			seedSaleLines(t, database.DB, dialect)
			writeSaleLines(t, database.DB, dialect, dts)
			markReturn(t, database.DB, dialect, dts, "SAL-C")
			defs, err := ParseDirectory(erpsellPackDir + "/metrics")
			if err != nil {
				t.Fatal(err)
			}
			engine := Engine{DB: database.DB, Dialect: dialect, Catalog: NewCatalog(defs, dts)}
			runAccuracyCases(t, engine)
		})
	}
}

// markReturn turns one sale into a return in both its header and its line rows of the fact tables.
func markReturn(t *testing.T, conn *sql.DB, dialect db.Dialect, dts []*doctype.DocType, record string) {
	t.Helper()
	var sale *doctype.DocType
	for _, dt := range dts {
		if dt.Name == "Sale" {
			sale = dt
		}
	}
	if sale == nil || sale.Facts == nil {
		t.Fatal("selling pack has no Sale fact model")
	}
	for _, table := range []string{sale.Facts.TableName(sale), sale.Facts.LineTableName(sale)} {
		stmt := fmt.Sprintf("UPDATE %s SET status = ? WHERE record_id = ?", dialect.QuoteIdent(table))
		if _, err := conn.ExecContext(context.Background(), db.Rebind(dialect, stmt), "Return", record); err != nil {
			t.Fatalf("mark %s as a return in %s: %v", record, table, err)
		}
	}
}

// runAccuracyCases checks the header-total refusal, the line-total split, and the status groups.
func runAccuracyCases(t *testing.T, engine Engine) {
	t.Helper()
	ctx := context.Background()
	day := tenanttime.Date{Year: 2026, Month: 11, Day: 18}
	span := func(q Query) Query {
		q.From, q.To, q.Grain = day, tenanttime.Date{Year: 2026, Month: 11, Day: 20}, GrainRange
		return q
	}
	t.Run("header total split by a line dimension is refused", func(t *testing.T) {
		_, err := engine.Query(ctx, span(Query{Metrics: []string{"sales"}, Dimensions: []string{"items.product"}}))
		if !errors.Is(err, ErrMeasureNotOnLines) {
			t.Fatalf("err = %v, want ErrMeasureNotOnLines", err)
		}
	})
	t.Run("line total split by product is allowed", func(t *testing.T) {
		cat, names, err := engine.Catalog.WithAdHoc([]AdHoc{{Model: "Sale", Agg: aggSum, Field: "line_total"}})
		if err != nil {
			t.Fatal(err)
		}
		e := Engine{DB: engine.DB, Dialect: engine.Dialect, Catalog: cat}
		if _, err := e.Query(ctx, span(Query{Metrics: names, Dimensions: []string{"items.product"}})); err != nil {
			t.Fatalf("query: %v", err)
		}
	})
	t.Run("a status no metric matches is not shown", func(t *testing.T) {
		res, err := engine.Query(ctx, span(Query{Metrics: []string{"sales"}, Dimensions: []string{"status"}}))
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(res.Rows) != 1 || res.Rows[0].Group[0] != "Paid" {
			t.Fatalf("rows = %v, want the Paid group alone", res.Rows)
		}
		// SAL-A 150 + SAL-B 50 + SAL-D 100: SAL-C is a return and not a sale.
		if got := res.Rows[0].Values[0]; !got.Valid || !got.Decimal.Equal(decimal.NewFromInt(300)) {
			t.Fatalf("paid sales = %v, want 300", got)
		}
	})
}
