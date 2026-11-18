//go:build integration

package facts

import (
	"context"
	"fmt"
	"maps"
	"os"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/eventstream"
)

// TestSummaryTableFromBeforeStatusHeals replaces a current summary table with one that predates the
// status column, holding a stale row. A fresh builder's ensure must drop it, mark the facts' days dirty,
// and the refresher must then rebuild the summaries to match the facts.
func TestSummaryTableFromBeforeStatusHeals(t *testing.T) {
	tests := []struct {
		driver, envVar string
	}{
		{"mysql", "KORA_FACTS_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_FACTS_DISPOSABLE_POSTGRES_DSN"},
	}
	for _, tc := range tests {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.envVar)
			if dsn == "" {
				t.Skipf("set %s to run", tc.envVar)
			}
			database := disposabledb.Open(t, tc.driver, dsn)
			dialect := db.Resolve(tc.driver)
			ctx := context.Background()
			for _, stmt := range []string{
				fmt.Sprintf("CREATE TABLE %s (name VARCHAR(140) NOT NULL PRIMARY KEY, owner VARCHAR(140) NOT NULL, creation TIMESTAMP NOT NULL)", dialect.QuoteIdent("tabSale")),
				"CREATE TABLE _kora_site_setting (site VARCHAR(140) NOT NULL, setting_key VARCHAR(140) NOT NULL, setting_value VARCHAR(140) NOT NULL, PRIMARY KEY (site, setting_key))",
				db.Rebind(dialect, fmt.Sprintf("INSERT INTO %s (name, owner, creation) VALUES (?, ?, ?)", dialect.QuoteIdent("tabSale"))),
			} {
				var args []any
				if strings.HasPrefix(stmt, "INSERT") {
					args = []any{"SAL-1", "u1", "2026-11-01 10:00:00"}
				}
				if _, err := database.DB.ExecContext(ctx, stmt, args...); err != nil {
					t.Fatalf("setup %q: %v", stmt, err)
				}
			}
			reg := doctype.NewRegistry()
			reg.Register(saleFacts())
			refresh := func(r *Refresher, want int) {
				t.Helper()
				if n, err := r.RefreshDue(ctx); err != nil || n != want {
					t.Fatalf("RefreshDue = %d, %v; want %d", n, err, want)
				}
			}
			refresher := func() *Refresher {
				return &Refresher{DB: database.DB, Dialect: dialect, Registry: reg, Admission: openLane{}, Owner: "test-refresher"}
			}

			// A current deploy builds the summary and refreshes it.
			first := &Builder{DB: database.DB, Dialect: dialect, Registry: reg}
			tx, err := database.DB.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = first.Consume(ctx, tx, saleEvent(1, eventstream.EventInsert, sale("2026-11-18", line("L1", "I1", "40.25")), nil)); err != nil {
				_ = tx.Rollback()
				t.Fatal(err)
			}
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
			refresh(refresher(), 1)

			// Simulate the table an older deploy left: no status column, a stale row, no dirty marker.
			summary := summaryTable(saleFacts())
			q := dialect.QuoteIdent
			for _, stmt := range []string{
				"DROP TABLE " + q(summary),
				fmt.Sprintf("CREATE TABLE %s (business_date DATE NOT NULL, legal_entity VARCHAR(140) NOT NULL, grain VARCHAR(140) NOT NULL, grain_value VARCHAR(140) NOT NULL, row_count BIGINT NOT NULL, total DECIMAL(21,9) NOT NULL, line_amount DECIMAL(21,9) NOT NULL, PRIMARY KEY (business_date, legal_entity, grain, grain_value))", q(summary)),
				fmt.Sprintf("INSERT INTO %s (business_date, legal_entity, grain, grain_value, row_count, total, line_amount) VALUES ('2026-11-18', '', '', '', 99, 1, 1)", q(summary)),
				"DELETE FROM " + q(dirtyTable),
			} {
				if _, err = database.DB.ExecContext(ctx, stmt); err != nil {
					t.Fatalf("simulate old table %q: %v", stmt, err)
				}
			}

			// A new process starts: ensure migrates the table and re-marks the day.
			restarted := &Builder{DB: database.DB, Dialect: dialect, Registry: reg}
			if err = restarted.ensure(ctx, saleFacts()); err != nil {
				t.Fatalf("ensure over old summary: %v", err)
			}
			refresh(refresher(), 1)
			want := map[string]string{
				"Paid///": "1|100.5|40.25", "Paid/item//": "1|100.5|0", "Paid/item/I1/": "0|0|40.25", "Paid/register/R1/": "1|100.5|40.25",
			}
			if got := summaryOf(t, database.DB, dialect, "2026-11-18"); !maps.Equal(got, want) {
				t.Fatalf("summary after migration = %v, want %v", got, want)
			}
			assertMatchesFacts(t, database.DB, dialect, "2026-11-18", summaryOf(t, database.DB, dialect, "2026-11-18"))

			// A third start finds the current table and does nothing.
			again := &Builder{DB: database.DB, Dialect: dialect, Registry: reg}
			if err = again.ensure(ctx, saleFacts()); err != nil {
				t.Fatal(err)
			}
			refresh(refresher(), 0)
		})
	}
}
