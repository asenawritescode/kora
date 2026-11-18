//go:build integration

package facts

import (
	"context"
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

// TestDriftCheckerFindsAndRepairsSummaryDrift corrupts a summary and checks that the drifting day is found,
// counted and marked dirty, and that a refresh repairs it so the next check is clean.
func TestDriftCheckerFindsAndRepairsSummaryDrift(t *testing.T) {
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
			setupEmptySales(t, database.DB, dialect)
			reg := doctype.NewRegistry()
			reg.Register(saleFacts())
			builder := &Builder{DB: database.DB, Dialect: dialect, Registry: reg}
			refresher := &Refresher{DB: database.DB, Dialect: dialect, Registry: reg, Admission: openLane{}, Owner: "test-refresher"}
			counter := &countingDrift{}
			checker := &DriftChecker{DB: database.DB, Dialect: dialect, Registry: reg, Site: "test", Admission: openLane{}, Drifted: counter,
				Now: func() time.Time { return time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC) }}
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

			// A summary that disagrees with the facts is found, counted, and its day is marked dirty.
			if _, err = database.DB.ExecContext(ctx, db.Rebind(dialect, "UPDATE "+dialect.QuoteIdent(summaryTable(saleFacts()))+" SET row_count = 9 WHERE grain = ''")); err != nil {
				t.Fatal(err)
			}
			run(1)
			if counter.n != 1 || countDirty(t, database.DB, dialect) != 1 {
				t.Fatalf("after summary drift: counted %d, dirty %d; want 1 and 1", counter.n, countDirty(t, database.DB, dialect))
			}
			if _, err = refresher.RefreshDue(ctx); err != nil {
				t.Fatal(err)
			}
			run(0)
		})
	}
}
