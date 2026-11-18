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

// TestHeldSalesStayOutOfFacts drives a Sale held by exclude_status through the consumer and the refresher.
// A held sale has no live facts and no summary row; moving it to Paid writes its facts; summaries split by
// status, so a second sale in Void sits in its own rows.
func TestHeldSalesStayOutOfFacts(t *testing.T) {
	tests := []struct {
		driver, dialect, envVar string
	}{
		{"mysql", "mysql", "KORA_FACTS_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_FACTS_DISPOSABLE_POSTGRES_DSN"},
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
			held := saleFacts()
			held.Facts.ExcludeStatus = []string{"Held"}
			reg := doctype.NewRegistry()
			reg.Register(held)
			builder := &Builder{DB: database.DB, Dialect: dialect, Registry: reg}
			refresher := &Refresher{DB: database.DB, Dialect: dialect, Registry: reg, Admission: openLane{}, Owner: "test-refresher"}
			consume := func(record string, seq int64, op eventstream.EventOp, data, old map[string]any) {
				t.Helper()
				tx, err := database.DB.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := builder.Consume(ctx, tx, saleEventFor(record, seq, op, data, old)); err != nil {
					_ = tx.Rollback()
					t.Fatalf("consume %s seq %d: %v", record, seq, err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			refresh := func(t *testing.T, want int) {
				t.Helper()
				if n, err := refresher.RefreshDue(ctx); err != nil || n != want {
					t.Fatalf("RefreshDue = %d, %v; want %d, nil", n, err, want)
				}
			}
			liveHeaders := func(t *testing.T) int {
				t.Helper()
				return countRows(t, database.DB, dialect, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(factHeader)+" WHERE line_id = '' AND deleted = ?", db.BoolParam(dialect, false))
			}

			held1 := sale("2026-11-18", line("L1", "I1", "40.25"))
			held1["status"] = "Held"
			consume("SAL-1", 1, eventstream.EventInsert, held1, nil)
			refresh(t, 1)
			if n := liveHeaders(t); n != 0 {
				t.Fatalf("live fact headers for a held sale = %d, want 0", n)
			}
			if got := summaryOf(t, database.DB, dialect, "2026-11-18"); len(got) != 0 {
				t.Fatalf("summary for a held sale = %v, want none", got)
			}

			paid := sale("2026-11-18", line("L1", "I1", "40.25"))
			consume("SAL-1", 2, eventstream.EventUpdate, paid, held1)
			refresh(t, 1)
			if n := liveHeaders(t); n != 1 {
				t.Fatalf("live fact headers after Held -> Paid = %d, want 1", n)
			}
			paidOnly := map[string]string{
				"Paid///": "1|100.5|40.25", "Paid/item//": "1|100.5|0", "Paid/item/I1/": "0|0|40.25", "Paid/register/R1/": "1|100.5|40.25",
			}
			if got := summaryOf(t, database.DB, dialect, "2026-11-18"); !maps.Equal(got, paidOnly) {
				t.Fatalf("summary after Held -> Paid = %v, want %v", got, paidOnly)
			}

			void := sale("2026-11-18")
			void["status"] = "Void"
			void["total"] = "50"
			consume("SAL-2", 3, eventstream.EventInsert, void, nil)
			refresh(t, 1)
			split := summaryOf(t, database.DB, dialect, "2026-11-18")
			if split["Void///"] != "1|50|0" || split["Paid///"] != "1|100.5|40.25" {
				t.Fatalf("summary split by status = %v; want Paid and Void day totals", split)
			}
			for key := range split {
				if strings.HasPrefix(key, "Held") {
					t.Fatalf("held status leaked into summary key %q", key)
				}
			}
		})
	}
}
