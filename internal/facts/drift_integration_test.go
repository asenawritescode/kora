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
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// countingDrift is a driftCounter that remembers the total it was asked to add.
type countingDrift struct{ n int64 }

func (c *countingDrift) Add(_ context.Context, incr int64, _ ...metric.AddOption) { c.n += incr }

// TestDriftCheckerRepairsSourceDrift walks the drift checker through summary drift (marked, not repaired),
// source drift (repaired by a queued and drained rebuild), a source row with no facts (repaired the same
// way), a duplicate repair request (deduplicated), and a held lease (the check does not run).
func TestDriftCheckerRepairsSourceDrift(t *testing.T) {
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
			// Held sales leave the fact tables: SAL-1 moved to Held must have its live header tombstoned.
			sales := saleFacts()
			sales.Facts.ExcludeStatus = []string{"Held"}
			reg := doctype.NewRegistry()
			reg.Register(sales)
			builder := &Builder{DB: database.DB, Dialect: dialect, Registry: reg}
			refresher := &Refresher{DB: database.DB, Dialect: dialect, Registry: reg, Admission: openLane{}, Owner: "test-refresher"}
			counter := &countingDrift{}
			checker := &DriftChecker{DB: database.DB, Dialect: dialect, Registry: reg, Site: "test", Admission: openLane{}, Owner: "checker-a", Drifted: counter,
				Now: func() time.Time { return time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC) }}
			store := &JobStore{DB: database.DB, Dialect: dialect}
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
			refresh := func() {
				t.Helper()
				if _, err := refresher.RefreshDue(ctx); err != nil {
					t.Fatal(err)
				}
			}
			jobs := func(status string) int {
				t.Helper()
				return countRows(t, database.DB, dialect, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(jobTable)+" WHERE model = ? AND status = ?", "Sale", status)
			}
			liveHeaders := func(day string) int {
				t.Helper()
				return countRows(t, database.DB, dialect, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(factHeader)+" WHERE line_id = '' AND deleted = ? AND business_date = ?", db.BoolParam(dialect, false), day)
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
			refresh()
			run(0)

			// Summary drift is marked for the refresher, and queues no rebuild.
			exec("UPDATE " + dialect.QuoteIdent(summaryTable(saleFacts())) + " SET row_count = 9 WHERE grain = ''")
			run(1)
			if counter.n != 1 || countDirty(t, database.DB, dialect) != 1 || jobs("queued") != 0 {
				t.Fatalf("summary drift: counted %d, dirty %d, queued jobs %d; want 1, 1, 0", counter.n, countDirty(t, database.DB, dialect), jobs("queued"))
			}
			refresh()
			run(0)

			// Source drift: SAL-1 left its live status. The check queues a rebuild, runs it, and the facts follow.
			exec("UPDATE " + dialect.QuoteIdent("tabSale") + " SET status = 'Held' WHERE name = 'SAL-1'")
			run(1)
			if counter.n != 2 || jobs("done") != 1 || liveHeaders("2026-11-18") != 0 {
				t.Fatalf("source drift: counted %d, done jobs %d, live headers %d; want 2, 1, 0", counter.n, jobs("done"), liveHeaders("2026-11-18"))
			}
			refresh()
			run(0)

			// A source row with no facts at all is drift too, and the same repair creates its facts.
			exec("INSERT INTO " + dialect.QuoteIdent("tabSale") + " (name, owner, creation, posting_date, total, status, register) VALUES ('SAL-2', 'u1', '2026-11-01 10:00:00', '2026-11-18', 10, 'Paid', 'R1')")
			run(1)
			if counter.n != 3 || liveHeaders("2026-11-18") != 1 {
				t.Fatalf("missing facts: counted %d, live headers %d; want 3 and 1", counter.n, liveHeaders("2026-11-18"))
			}
			refresh()
			run(0)

			// A repair request for a day a queued job already covers is not queued again.
			if _, err = store.Enqueue(ctx, "Sale", Window{From: tenanttime.Date{Year: 2026, Month: 11, Day: 18}, To: tenanttime.Date{Year: 2026, Month: 11, Day: 18}}); err != nil {
				t.Fatal(err)
			}
			day := tenanttime.Date{Year: 2026, Month: 11, Day: 18}
			dt := reg.Get("Sale")
			if err = checker.queueRepair(ctx, dt, day); err != nil {
				t.Fatal(err)
			}
			if n := jobs("queued"); n != 1 {
				t.Fatalf("queued rebuilds for a covered day = %d, want 1", n)
			}
			run(0) // the queued job is drained, not left behind
			if n := jobs("queued"); n != 0 {
				t.Fatalf("queued rebuilds after the run = %d, want 0", n)
			}

			// Another replica's live lease stops the check; once it expires the check runs again.
			expr, arg := dbClock(dialect, 600)
			exec(fmt.Sprintf("UPDATE %s SET owner = 'other', lease_until = %s WHERE id = 1", dialect.QuoteIdent(driftLeaseTable), expr), arg)
			exec("UPDATE " + dialect.QuoteIdent("tabSale") + " SET total = 11 WHERE name = 'SAL-2'")
			run(0)
			if counter.n != 3 {
				t.Fatalf("drift counted while another replica held the lease: %d", counter.n)
			}
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
		"CREATE TABLE _kora_tenant_event_sequence (id INT PRIMARY KEY, last_sequence BIGINT NOT NULL DEFAULT 0)",
		"INSERT INTO _kora_tenant_event_sequence (id, last_sequence) VALUES (1, 41)",
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
}
