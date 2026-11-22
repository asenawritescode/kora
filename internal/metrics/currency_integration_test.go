//go:build integration

package metrics

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/eventstream"
	"github.com/asenawritescode/kora/internal/facts"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// TestCurrenciesInRangeAcrossDialects seeds selling-pack sales in three currencies, one of them outside the
// range, and checks that Currencies lists the two inside it: the base currency (empty) and KES, and that a
// record type without a currency field lists none.
func TestCurrenciesInRangeAcrossDialects(t *testing.T) {
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
			for _, stmt := range []string{
				fmt.Sprintf("CREATE TABLE %s (name VARCHAR(140) NOT NULL PRIMARY KEY, owner VARCHAR(140) NOT NULL, creation TIMESTAMP NOT NULL)", dialect.QuoteIdent("tabSale")),
				"CREATE TABLE _kora_site_setting (site VARCHAR(140) NOT NULL, setting_key VARCHAR(140) NOT NULL, setting_value VARCHAR(140) NOT NULL, PRIMARY KEY (site, setting_key))",
			} {
				if _, err := database.DB.ExecContext(ctx, stmt); err != nil {
					t.Fatalf("setup %q: %v", stmt, err)
				}
			}
			dts := erpsellDoctypes(t)
			var sale *doctype.DocType
			for _, dt := range dts {
				if dt.Name == "Sale" {
					sale = dt
				}
			}
			if sale == nil || sale.Facts == nil || sale.Facts.CurrencyField != "currency" {
				t.Fatal("selling pack Sale has no currency field")
			}
			defs, err := ParseDirectory(erpsellPackDir + "/metrics")
			if err != nil {
				t.Fatal(err)
			}
			reg := doctype.NewRegistry()
			reg.Register(sale)
			builder := &facts.Builder{DB: database.DB, Dialect: dialect, Registry: reg}
			refresher := &facts.Refresher{DB: database.DB, Dialect: dialect, Registry: reg, Admission: openLane{}, Owner: "currency-test"}
			seq := int64(0)
			write := func(record, saleDate, currency string) {
				t.Helper()
				seq++
				data := map[string]any{
					"sale_date": saleDate, "status": "Paid", "register": "R1", "location": "Main", "currency": currency,
					"total": "10.00", "base_total": "10.00", "cost_total": "4.00", "profit": "6.00",
					"discount_amount": "0", "tax_amount": "0",
					"items": []any{map[string]any{"name": "L-" + record, "product": "P1", "quantity": "1", "line_total": "10.00",
						"line_cost": "4.00", "line_profit": "6.00"}},
				}
				event := contract.EventEnvelope{
					ID: fmt.Sprintf("evt-%d", seq), Type: outbox.EventTypeName("Sale", eventstream.EventInsert),
					Sequence: contract.Revision(fmt.Sprint(seq)), Site: "test",
					OccurredAt: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC),
					Data:       contract.MustEncodeData(map[string]any{"table": "Sale", "record_id": record, "data": data}),
				}
				tx, txErr := database.DB.BeginTx(ctx, nil)
				if txErr != nil {
					t.Fatal(txErr)
				}
				if consumeErr := builder.Consume(ctx, tx, event); consumeErr != nil {
					_ = tx.Rollback()
					t.Fatalf("consume %s: %v", record, consumeErr)
				}
				if commitErr := tx.Commit(); commitErr != nil {
					t.Fatal(commitErr)
				}
			}
			write("SAL-1", "2026-11-18", "")
			write("SAL-2", "2026-11-19", "KES")
			write("SAL-3", "2026-11-20", "USD")
			write("SAL-4", "2026-11-25", "EUR")
			if _, refreshErr := refresher.RefreshDue(ctx); refreshErr != nil {
				t.Fatalf("refresh: %v", refreshErr)
			}

			engine := Engine{DB: database.DB, Dialect: dialect, Catalog: NewCatalog(defs, dts)}
			day := func(d int) tenanttime.Date { return tenanttime.Date{Year: 2026, Month: 11, Day: d} }
			got, err := engine.Currencies(ctx, Query{Metrics: []string{"sales"}, From: day(18), To: day(20), Grain: GrainRange})
			if err != nil {
				t.Fatalf("currencies: %v", err)
			}
			if want := []string{"", "KES", "USD"}; !slices.Equal(got, want) {
				t.Fatalf("currencies = %q; want %q", got, want)
			}
			none, err := engine.Currencies(ctx, Query{Metrics: []string{"stock_adjusted"}, From: day(18), To: day(20), Grain: GrainRange})
			if err != nil {
				t.Fatalf("currencies of stock: %v", err)
			}
			if none != nil {
				t.Fatalf("stock currencies = %q; want none", none)
			}
		})
	}
}
