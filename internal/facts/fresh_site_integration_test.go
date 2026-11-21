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
	"github.com/asenawritescode/kora/internal/site"
)

// TestFreshSiteWorkersReadTablesAfterSync starts a fresh database the way tenant start does: the model sync runs
// first, and the summary refresher and the drift check then run with no event consumed yet. Neither may fail on
// a missing dirty table or fact table.
func TestFreshSiteWorkersReadTablesAfterSync(t *testing.T) {
	for _, tc := range []struct{ driver, dialect, envVar string }{
		{"mysql", "mysql", "KORA_FACTS_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_FACTS_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.envVar)
			if dsn == "" {
				t.Skipf("set %s to run", tc.envVar)
			}
			ctx := context.Background()
			database := disposabledb.Open(t, tc.driver, dsn)
			d := db.Resolve(tc.dialect)
			if err := site.BootstrapSystemTables(ctx, database.DB, d); err != nil {
				t.Fatalf("bootstrap system tables: %v", err)
			}
			// The record table exists on a fresh site: the schema migration creates it at setup.
			if _, err := database.DB.ExecContext(ctx, "CREATE TABLE "+d.QuoteIdent("tabSale")+" (name VARCHAR(140) NOT NULL PRIMARY KEY, owner VARCHAR(140) NOT NULL, creation TIMESTAMP NOT NULL, posting_date DATE NULL, total DECIMAL(21,9) NULL, status VARCHAR(140) NULL, register VARCHAR(140) NULL)"); err != nil {
				t.Fatalf("record table: %v", err)
			}
			reg := doctype.NewRegistry()
			reg.Register(saleFacts())
			if err := (&ModelSync{DB: database.DB, Dialect: d, Registry: reg, Owner: "fresh-test"}).Sync(ctx); err != nil {
				t.Fatalf("sync: %v", err)
			}
			refresher := &Refresher{DB: database.DB, Dialect: d, Registry: reg, Admission: openLane{}, Owner: "fresh-test"}
			if _, err := refresher.RefreshDue(ctx); err != nil {
				t.Fatalf("refresh before any event: %v", err)
			}
			drift := &DriftChecker{DB: database.DB, Dialect: d, Registry: reg, Site: "fresh", Admission: openLane{}, Owner: "fresh-test"}
			if _, err := drift.RunDue(ctx); err != nil {
				t.Fatalf("drift check before any event: %v", err)
			}
		})
	}
}

// TestDirtyColumnsAddedOncePerRacingStart simulates a database created by the previous version, whose dirty
// table had no version or lease columns, and starts several processes at once. Every start must succeed: a
// process that loses the race to add a column finds it already added.
func TestDirtyColumnsAddedOncePerRacingStart(t *testing.T) {
	for _, tc := range []struct{ driver, dialect, envVar string }{
		{"mysql", "mysql", "KORA_FACTS_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_FACTS_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.envVar)
			if dsn == "" {
				t.Skipf("set %s to run", tc.envVar)
			}
			ctx := context.Background()
			database := disposabledb.Open(t, tc.driver, dsn)
			d := db.Resolve(tc.dialect)
			if _, err := database.DB.ExecContext(ctx, dirtyDDL(d)); err != nil {
				t.Fatalf("previous-version dirty table: %v", err)
			}
			var wg sync.WaitGroup
			errs := make(chan error, 8)
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					errs <- ensureDirtyTable(ctx, database.DB, d)
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatalf("racing start: %v", err)
				}
			}
			if err := ensureDirtyTable(ctx, database.DB, d); err != nil {
				t.Fatalf("second start: %v", err)
			}
			if !hasColumn(ctx, database.DB, d, dirtyTable, "version") {
				t.Fatal("version column missing after starts")
			}
		})
	}
}
