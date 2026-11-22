//go:build integration

package facts

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
)

// TestEnqueueGeneratedQueuesOncePerModel checks the startup backfill on each dialect: a generated model gets
// one whole-model job, a second start queues nothing, a declared model is never queued, a cancelled job
// lets the next start queue again, and a finished backfill is never queued again.
func TestEnqueueGeneratedQueuesOncePerModel(t *testing.T) {
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
			if _, err := database.DB.ExecContext(ctx, "CREATE TABLE _kora_tenant_event_sequence (id INT PRIMARY KEY, last_sequence BIGINT NOT NULL DEFAULT 0)"); err != nil {
				t.Fatal(err)
			}
			if _, err := database.DB.ExecContext(ctx, "INSERT INTO _kora_tenant_event_sequence (id, last_sequence) VALUES (1, 7)"); err != nil {
				t.Fatal(err)
			}
			reg := doctype.NewRegistry()
			reg.Register(&doctype.DocType{Name: "Expense", Fields: []doctype.Field{{Fieldname: "amount", Fieldtype: "Currency"}}})
			reg.Register(&doctype.DocType{Name: "Sale", Facts: &doctype.FactModel{Time: "creation", Measures: []doctype.FactMeasure{{Field: "total"}}}})
			store := &JobStore{DB: database.DB, Dialect: dialect}

			if n, err := EnqueueGenerated(ctx, database.DB, dialect, reg); err != nil || n != 1 {
				t.Fatalf("first start queued %d (%v), want 1", n, err)
			}
			if n, err := EnqueueGenerated(ctx, database.DB, dialect, reg); err != nil || n != 0 {
				t.Fatalf("second start queued %d (%v), want 0", n, err)
			}
			var id string
			if err := database.DB.QueryRowContext(ctx, db.Rebind(dialect, "SELECT id FROM "+jobTable+" WHERE model = ?"), "Expense").Scan(&id); err != nil {
				t.Fatal(err)
			}
			if err := store.Cancel(ctx, id); err != nil {
				t.Fatal(err)
			}
			if n, err := EnqueueGenerated(ctx, database.DB, dialect, reg); err != nil || n != 1 {
				t.Fatalf("start after cancel queued %d (%v), want 1", n, err)
			}
			// Run the queued backfill to the end: a finished model is never queued again.
			claimed, err := store.Claim(ctx, "Expense", "worker-a")
			if err != nil || claimed == nil {
				t.Fatalf("claim = %v, %v; want the queued backfill", claimed, err)
			}
			if err := store.Finish(ctx, claimed, JobDone, ""); err != nil {
				t.Fatal(err)
			}
			if n, err := EnqueueGenerated(ctx, database.DB, dialect, reg); err != nil || n != 0 {
				t.Fatalf("start after done queued %d (%v), want 0", n, err)
			}
		})
	}
}

// TestEnqueueOnceAcrossConcurrentCallers races several callers, standing in for replicas, for one model's
// backfill: exactly one of them queues a job, and the table holds one row for it.
func TestEnqueueOnceAcrossConcurrentCallers(t *testing.T) {
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
			if _, err := database.DB.ExecContext(ctx, "CREATE TABLE _kora_tenant_event_sequence (id INT PRIMARY KEY, last_sequence BIGINT NOT NULL DEFAULT 0)"); err != nil {
				t.Fatal(err)
			}
			if _, err := database.DB.ExecContext(ctx, "INSERT INTO _kora_tenant_event_sequence (id, last_sequence) VALUES (1, 0)"); err != nil {
				t.Fatal(err)
			}
			store := &JobStore{DB: database.DB, Dialect: dialect}
			const callers = 8
			var wg sync.WaitGroup
			queued := make(chan bool, callers)
			errs := make(chan error, callers)
			for i := 0; i < callers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					job, err := store.EnqueueOnce(ctx, "Expense")
					if err != nil {
						errs <- err
						return
					}
					queued <- job != nil
				}()
			}
			wg.Wait()
			close(queued)
			close(errs)
			for err := range errs {
				t.Fatalf("concurrent enqueue: %v", err)
			}
			won := 0
			for q := range queued {
				if q {
					won++
				}
			}
			if won != 1 {
				t.Fatalf("%d callers queued a job, want exactly 1", won)
			}
			if n := countRows(t, database.DB, dialect, "SELECT COUNT(*) FROM "+jobTable+" WHERE model = ?", "Expense"); n != 1 {
				t.Fatalf("job rows = %d, want 1", n)
			}
		})
	}
}
