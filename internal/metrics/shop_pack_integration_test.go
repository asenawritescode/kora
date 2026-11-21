//go:build integration

package metrics

import (
	"context"
	"fmt"
	"os"
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

// TestShopProfitByDayChart runs the shop_insights profit_by_day chart over selling-pack sales through
// the fact consumer and refresher. Day 18 Nov: two paid sales, profit 40 + 20, cost 60 + 30. Day 19 Nov:
// one paid sale (profit 10, cost 20) and one return (profit -8, cost -12, negative as the pack computes it).
// Expected: 18 Nov 60.00 profit, 90.00 cost; 19 Nov 2.00 profit, 8.00 cost.
func TestShopProfitByDayChart(t *testing.T) {
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
			if sale == nil || sale.Facts == nil {
				t.Fatal("selling pack has no Sale fact model")
			}
			defs, err := ParseDirectory(erpsellPackDir + "/metrics")
			if err != nil {
				t.Fatal(err)
			}
			if validateErr := Validate(defs, dts); validateErr != nil {
				t.Fatalf("selling pack metrics do not validate: %v", validateErr)
			}
			dashboards, err := ParseDashboardDirectory(erpsellPackDir + "/dashboards")
			if err != nil {
				t.Fatal(err)
			}
			var chart Chart
			for _, d := range dashboards {
				for _, ch := range d.Charts {
					if d.Name == "shop_insights" && ch.ID == "profit_by_day" {
						chart = ch
					}
				}
			}
			if chart.ID == "" {
				t.Fatal("shop_insights has no profit_by_day chart")
			}

			reg := doctype.NewRegistry()
			reg.Register(sale)
			builder := &facts.Builder{DB: database.DB, Dialect: dialect, Registry: reg}
			refresher := &facts.Refresher{DB: database.DB, Dialect: dialect, Registry: reg, Admission: openLane{}, Owner: "shop-pack-test"}
			seq := int64(0)
			write := func(record, status, saleDate, baseTotal, profit, cost string) {
				t.Helper()
				seq++
				data := map[string]any{
					"sale_date": saleDate, "status": status, "register": "R1", "location": "Main",
					"total": baseTotal, "base_total": baseTotal, "cost_total": cost, "profit": profit,
					"discount_amount": "0", "tax_amount": "0",
					"items": []any{map[string]any{"name": "L-" + record, "product": "P1", "quantity": "1", "line_total": baseTotal,
						"line_cost": cost, "line_profit": profit}},
				}
				payload := map[string]any{"table": "Sale", "record_id": record, "data": data}
				event := contract.EventEnvelope{
					ID: fmt.Sprintf("evt-%d", seq), Type: outbox.EventTypeName("Sale", eventstream.EventInsert),
					Sequence: contract.Revision(fmt.Sprint(seq)), Site: "test",
					OccurredAt: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC),
					Data:       contract.MustEncodeData(payload),
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
			write("SAL-P1", "Paid", "2026-11-18", "100.00", "40.00", "60.00")
			write("SAL-P2", "Paid", "2026-11-18", "50.00", "20.00", "30.00")
			write("SAL-P3", "Paid", "2026-11-19", "30.00", "10.00", "20.00")
			write("SAL-R1", "Return", "2026-11-19", "-20.00", "-8.00", "-12.00")
			if _, refreshErr := refresher.RefreshDue(ctx); refreshErr != nil {
				t.Fatalf("refresh: %v", refreshErr)
			}

			engine := Engine{DB: database.DB, Dialect: dialect, Catalog: NewCatalog(defs, dts)}
			day := func(d int) tenanttime.Date { return tenanttime.Date{Year: 2026, Month: 11, Day: d} }
			res, queryErr := engine.Query(ctx, Query{Metrics: chart.Metrics, From: day(18), To: day(19), Grain: GrainDay})
			if queryErr != nil {
				t.Fatalf("chart query: %v", queryErr)
			}
			want := []string{
				"2026-11-18||60.00,90.00",
				"2026-11-19||2.00,8.00",
			}
			if got := render(res.Rows); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("profit_by_day rows = %q; want %q", got, want)
			}
		})
	}
}
