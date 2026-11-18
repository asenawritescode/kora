//go:build integration

package facts

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/eventstream"
)

// countingDrift is a driftCounter that remembers the total it was asked to add.
type countingDrift struct{ n int64 }

func (c *countingDrift) Add(_ context.Context, incr int64, _ ...metric.AddOption) { c.n += incr }

// TestDriftCheckerFindsAndRepairsDrift corrupts a summary, then a source record, and checks that each
// drifting day is found, counted and marked dirty, that a refresh repairs the summary, and that a held
// lease stops the check.
func TestDriftCheckerFindsAndRepairsDrift(t *testing.T) {
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
			setupSourceSales(t, database.DB, dialect)
			reg := doctype.NewRegistry()
			reg.Register(saleFacts())
			builder := &Builder{DB: database.DB, Dialect: dialect, Registry: reg}
			refresher := &Refresher{DB: database.DB, Dialect: dialect, Registry: reg, Admission: openLane{}, Owner: "test-refresher"}
			counter := &countingDrift{}
			checker := &DriftChecker{DB: database.DB, Dialect: dialect, Registry: reg, Site: "test", Admission: openLane{}, Owner: "checker-a", Drifted: counter,
				Now: func() time.Time { return time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC) }}
			exec := func(query string, args ...any) {
				t.Helper()
				if _, err := database.DB.ExecContext(ctx, db.Rebind(dialect, query), args...); err != nil {
					t.Fatalf("exec %q: %v", query, err)
				}
			}
			run := func(want int) {
				t.Helper()
				if n, err := checker.RunDue(ctx); err != nil || n != want {
					t.Fatalf("RunDue = %d, %v; want %d", n, err, want)
				}
			}

			tx, err := database.DB.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = builder.Consume(ctx, tx, saleEvent(1, eventstream.EventInsert, sale("2026-11-18", line("L1", "I1", "40.25")), nil)); err != nil {
				_ = tx.Rollback()
				t.Fatal(err)
			}
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if _, err = refresher.RefreshDue(ctx); err != nil {
				t.Fatal(err)
			}
			run(0)
			if counter.n != 0 {
				t.Fatalf("drift counted on a clean day: %d", counter.n)
			}

			// A summary that disagrees with the facts is found and its day is marked dirty.
			exec("UPDATE " + dialect.QuoteIdent(summaryTable(saleFacts())) + " SET row_count = 9 WHERE grain = ''")
			run(1)
			if counter.n != 1 || countDirty(t, database.DB, dialect) != 1 {
				t.Fatalf("after summary drift: counted %d, dirty %d; want 1 and 1", counter.n, countDirty(t, database.DB, dialect))
			}
			if _, err = refresher.RefreshDue(ctx); err != nil {
				t.Fatal(err)
			}
			run(0)

			// A source record that left its live status is found even though the summary still matches the facts.
			exec("UPDATE " + dialect.QuoteIdent("tabSale") + " SET status = 'Held' WHERE name = 'SAL-1'")
			run(1)
			if counter.n != 2 {
				t.Fatalf("after source drift: counted %d, want 2", counter.n)
			}

			// Another replica's live lease stops the check; once it expires the check runs again.
			expr, arg := dbClock(dialect, 600)
			exec(fmt.Sprintf("UPDATE %s SET owner = 'other', lease_until = %s WHERE id = 1", dialect.QuoteIdent(driftLeaseTable), expr), arg)
			run(0)
			expr, arg = dbClock(dialect, -1)
			exec(fmt.Sprintf("UPDATE %s SET lease_until = %s WHERE id = 1", dialect.QuoteIdent(driftLeaseTable), expr), arg)
			run(1)
		})
	}
}

// setupSourceSales creates the Sale source tables and one Paid sale, SAL-1, matching the events the test sends.
func setupSourceSales(t *testing.T, conn *sql.DB, dialect db.Dialect) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		"CREATE TABLE " + dialect.QuoteIdent("tabSale") + " (name VARCHAR(140) NOT NULL PRIMARY KEY, owner VARCHAR(140) NOT NULL, creation TIMESTAMP NOT NULL, posting_date DATE NOT NULL, total DECIMAL(21,9) NOT NULL, status VARCHAR(140) NOT NULL, register VARCHAR(140) NOT NULL)",
		"CREATE TABLE " + dialect.QuoteIdent("tabSale__items") + " (name VARCHAR(140) NOT NULL PRIMARY KEY, parent VARCHAR(140) NOT NULL, parentfield VARCHAR(140) NOT NULL, item VARCHAR(140) NOT NULL, amount DECIMAL(21,9) NOT NULL)",
		"CREATE TABLE _kora_site_setting (site VARCHAR(140) NOT NULL, setting_key VARCHAR(140) NOT NULL, setting_value VARCHAR(140) NOT NULL, PRIMARY KEY (site, setting_key))",
		"INSERT INTO " + dialect.QuoteIdent("tabSale") + " (name, owner, creation, posting_date, total, status, register) VALUES ('SAL-1', 'u1', '2026-11-01 10:00:00', '2026-11-18', 100.50, 'Paid', 'R1')",
		"INSERT INTO " + dialect.QuoteIdent("tabSale__items") + " (name, parent, parentfield, item, amount) VALUES ('L1', 'SAL-1', 'items', 'I1', 40.25)",
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
}
