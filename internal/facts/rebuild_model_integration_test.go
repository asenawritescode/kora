//go:build integration

package facts

import (
	"context"
	"os"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
)

// TestRebuildModelEndToEnd runs the command's core path (enqueue, claim, chunked run, status, cancel)
// against a real database. Platform site resolution belongs to the CLI and is not exercised here.
func TestRebuildModelEndToEnd(t *testing.T) {
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
			registry := doctype.NewRegistry()
			registry.Register(saleFacts())

			job, err := RebuildModel(ctx, RebuildRequest{DB: database.DB, Dialect: dialect, Registry: registry, Site: "test", Model: "Sale"})
			if err != nil {
				t.Fatal(err)
			}
			if job.Status != JobDone || job.Processed != rebuildSales {
				t.Fatalf("job = %+v; want done with %d processed", job, rebuildSales)
			}
			header := "SELECT COUNT(*) FROM " + dialect.QuoteIdent(factHeader) + " WHERE line_id = ''"
			if n := countRows(t, database.DB, dialect, header); n != rebuildSales {
				t.Fatalf("header rows = %d, want %d", n, rebuildSales)
			}

			// A second rebuild of the same source leaves the same rows.
			again, err := RebuildModel(ctx, RebuildRequest{DB: database.DB, Dialect: dialect, Registry: registry, Site: "test", Model: "Sale"})
			if err != nil || again.Status != JobDone {
				t.Fatalf("second rebuild = %+v, %v", again, err)
			}
			if n := countRows(t, database.DB, dialect, header); n != rebuildSales {
				t.Fatalf("header rows after second rebuild = %d, want %d", n, rebuildSales)
			}

			// A finished job cannot be cancelled; a queued one can.
			store := &JobStore{DB: database.DB, Dialect: dialect}
			if err = store.Cancel(ctx, job.ID); err == nil {
				t.Fatal("cancel of a done job was accepted")
			}
			queued, err := store.Enqueue(ctx, "Sale", Window{})
			if err != nil {
				t.Fatal(err)
			}
			if err = store.Cancel(ctx, queued.ID); err != nil {
				t.Fatalf("cancel queued: %v", err)
			}
		})
	}
}
