//go:build integration

package facts

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
)

// TestRebuildRunnerStartsQueuedJobWithoutDrift queues a rebuild with a sync and runs it with the runner alone:
// once with a single pass, then with the Run loop after a second queue. The drift checker never runs.
func TestRebuildRunnerStartsQueuedJobWithoutDrift(t *testing.T) {
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
			seedRebuildSource(t, database.DB, dialect)
			store := &JobStore{DB: database.DB, Dialect: dialect}

			v1 := doctype.NewRegistry()
			v1.Register(saleFacts())
			mustSync(t, (&ModelSync{DB: database.DB, Dialect: dialect, Registry: v1, Owner: "sync-1"}).Sync(ctx))
			queued := queuedSaleJob(t, database.DB, dialect)

			// One pass runs the queued job to done.
			runner := &RebuildRunner{DB: database.DB, Dialect: dialect, Registry: v1, Site: "test", Owner: "runner-1"}
			if n, err := runner.Drain(ctx); err != nil || n != 1 {
				t.Fatalf("drain = %d, %v; want 1 job run", n, err)
			}
			if job := mustGetJob(t, store, queued); job.Status != JobDone || job.Processed != rebuildSales {
				t.Fatalf("drained job = %+v; want done with %d processed", job, rebuildSales)
			}

			// A second queue (a changed shape) is run by the Run loop, with no drift check involved.
			v2 := doctype.NewRegistry()
			v2.Register(withSaleMeasure(saleFacts(), doctype.FactMeasure{Field: "items.amount", Name: "line_copy"}))
			mustSync(t, (&ModelSync{DB: database.DB, Dialect: dialect, Registry: v2, Owner: "sync-2"}).Sync(ctx))
			next := queuedSaleJob(t, database.DB, dialect)
			loopCtx, stop := context.WithCancel(ctx)
			defer stop()
			loop := &RebuildRunner{DB: database.DB, Dialect: dialect, Registry: v2, Site: "test", Owner: "runner-2", Every: 10 * time.Millisecond}
			go loop.Run(loopCtx)
			deadline := time.Now().Add(2 * time.Minute)
			for mustGetJob(t, store, next).Status != JobDone {
				if time.Now().After(deadline) {
					t.Fatal("the Run loop did not finish the queued job")
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

// queuedSaleJob returns the id of the one queued whole-model job of Sale.
func queuedSaleJob(t *testing.T, database *sql.DB, dialect db.Dialect) string {
	t.Helper()
	query := "SELECT id FROM " + dialect.QuoteIdent(jobTable) + " WHERE model = 'Sale' AND status = 'queued' AND scope_from IS NULL"
	var id string
	if err := database.QueryRowContext(context.Background(), db.Rebind(dialect, query)).Scan(&id); err != nil {
		t.Fatalf("find queued Sale job: %v", err)
	}
	return id
}
