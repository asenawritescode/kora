//go:build integration

package facts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
)

// TestModelSyncEvolvesAndQueuesOneRebuild walks a Sale fact model through sync, a measure added in place,
// a measure removed (recreate), and a lease held by another replica, on each dialect. It checks that each
// change queues exactly one whole-model rebuild, and that progress follows the rebuild.
func TestModelSyncEvolvesAndQueuesOneRebuild(t *testing.T) {
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
			syncWith := func(reg *doctype.Registry, owner string) error {
				return (&ModelSync{DB: database.DB, Dialect: dialect, Registry: reg, Owner: owner}).Sync(ctx)
			}

			// First sync: the model has no stored shape, so one rebuild is queued. A second sync queues nothing.
			v1 := doctype.NewRegistry()
			v1.Register(saleFacts())
			mustSync(t, syncWith(v1, "worker-1"))
			mustSync(t, syncWith(v1, "worker-1"))
			assertOpenJobs(t, database.DB, dialect, 1)

			// A measure added in place: the column is added, the open job is replaced, and still one is open.
			// A sentinel summary day stands in for a dashboard's numbers: an additive change must keep it.
			exec(t, database.DB, dialect, "INSERT INTO "+dialect.QuoteIdent(summaryTable(saleFacts()))+
				" (business_date, legal_entity, status, grain, grain_value, row_count, total, line_amount) VALUES ('2026-01-01', '', '', '', '', 7, 0, 0)")
			v2 := doctype.NewRegistry()
			v2.Register(withSaleMeasure(saleFacts(), doctype.FactMeasure{Field: "items.amount", Name: "line_copy"}))
			mustSync(t, syncWith(v2, "worker-2"))
			assertOpenJobs(t, database.DB, dialect, 1)
			if err := probeColumn(database.DB, dialect, factHeader, "line_copy"); err != nil {
				t.Fatalf("added column missing: %v", err)
			}
			if err := probeColumn(database.DB, dialect, summaryTable(saleFacts()), "line_copy"); err != nil {
				t.Fatalf("added summary column missing: %v", err)
			}
			if n := countRows(t, database.DB, dialect, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(summaryTable(saleFacts()))+" WHERE row_count = 7"); n != 1 {
				t.Fatalf("additive change dropped the summary: sentinel rows = %d, want 1", n)
			}

			// Progress while queued, then part-way through a rebuild of 1000 of rebuildSales records.
			progress := func() Progress {
				t.Helper()
				p, err := store.Progress(ctx, []string{"Sale"})
				if err != nil {
					t.Fatal(err)
				}
				return p["Sale"]
			}
			if p := progress(); !p.Building || p.Fraction != 0 {
				t.Fatalf("queued progress = %+v, want building at 0", p)
			}
			job, err := store.ReplaceWhole(ctx, "Sale")
			if err != nil {
				t.Fatal(err)
			}
			// The replaced job is cancelled, so the new one is the only open job.
			assertOpenJobs(t, database.DB, dialect, 1)
			claimed, err := store.Claim(ctx, "Sale", "worker-3")
			if err != nil || claimed == nil || claimed.ID != job.ID {
				t.Fatalf("claim = %+v, %v", claimed, err)
			}
			cancelCtx, cancel := context.WithCancel(ctx)
			rebuilder := &Rebuilder{DB: database.DB, Dialect: dialect, Registry: v2, Site: "test", Store: store,
				chunkSize: 1000, afterChunk: cancel}
			if err = rebuilder.Run(cancelCtx, claimed); !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted run = %v, want context.Canceled", err)
			}
			cancel()
			if got := mustGetJob(t, store, job.ID); got.Total != rebuildSales || got.Processed != 1000 {
				t.Fatalf("interrupted job total/processed = %d/%d, want %d/1000", got.Total, got.Processed, rebuildSales)
			}
			if p := progress(); !p.Building || math.Abs(p.Fraction-1000.0/rebuildSales) > 1e-9 {
				t.Fatalf("mid-rebuild progress = %+v, want building at %v", p, 1000.0/rebuildSales)
			}

			// A lease held by another replica stops a sync from touching the model.
			expireLease(t, database.DB, dialect, job.ID)
			resumed, err := store.Claim(ctx, "Sale", "worker-3")
			if err != nil || resumed == nil {
				t.Fatalf("resume claim = %+v, %v", resumed, err)
			}
			if err = rebuilder.Run(ctx, resumed); err != nil {
				t.Fatalf("resumed run: %v", err)
			}
			if p := progress(); p.Building || p.Fraction != 1 {
				t.Fatalf("finished progress = %+v, want ready", p)
			}
			// Another replica takes the lease now: it is held until 300 seconds have passed.
			exec(t, database.DB, dialect, fmt.Sprintf("UPDATE %s SET lease_owner = 'other', lease_started = %s WHERE model = 'Sale'",
				dialect.QuoteIdent(shapeTable), dialect.NowMinusSeconds()), 0)
			mustSync(t, syncWith(v1, "worker-4"))
			assertOpenJobs(t, database.DB, dialect, 0)
			// Expire the other replica's lease: it started 300 seconds ago.
			exec(t, database.DB, dialect, fmt.Sprintf("UPDATE %s SET lease_started = %s WHERE model = 'Sale'",
				dialect.QuoteIdent(shapeTable), dialect.NowMinusSeconds()), 300)

			// The first rebuild is done and its guard points at it, so only a changed shape may queue another.
			if n := countRows(t, database.DB, dialect, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(guardTable)+" WHERE model = 'Sale' AND job_id = ?", job.ID); n != 1 {
				t.Fatalf("guard of Sale does not point at the finished rebuild (%d rows)", n)
			}

			// The measure removed again: its column is gone, so the tables are recreated and the rebuild refills them.
			// The guard was done, yet the change queues one rebuild, and a backfill queue meanwhile adds none.
			mustSync(t, syncWith(v1, "worker-4"))
			assertOpenJobs(t, database.DB, dialect, 1)
			if queued, err := store.EnqueueOnce(ctx, "Sale"); err != nil || queued != nil {
				t.Fatalf("EnqueueOnce while a rebuild is queued = %+v, %v; want nil", queued, err)
			}
			if err := probeColumn(database.DB, dialect, factHeader, "line_copy"); err == nil {
				t.Fatal("removed column still present after recreate")
			}
			if n := countRows(t, database.DB, dialect, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(factHeader)); n != 0 {
				t.Fatalf("recreated fact table has %d rows, want 0 before the rebuild", n)
			}
			if n := countRows(t, database.DB, dialect, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(summaryTable(saleFacts()))); n != 0 {
				t.Fatalf("recreated summary has %d rows, want 0", n)
			}
		})
	}
}

func withSaleMeasure(dt *doctype.DocType, m doctype.FactMeasure) *doctype.DocType {
	dt.Facts.Measures = append(dt.Facts.Measures, m)
	return dt
}

func mustSync(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
}

// assertOpenJobs checks the number of queued or running whole-model jobs of Sale.
func assertOpenJobs(t *testing.T, database *sql.DB, dialect db.Dialect, want int) {
	t.Helper()
	query := "SELECT COUNT(*) FROM " + dialect.QuoteIdent(jobTable) + " WHERE model = 'Sale' AND scope_from IS NULL AND status IN ('queued', 'running')"
	if n := countRows(t, database, dialect, query); n != want {
		t.Fatalf("open whole-model jobs = %d, want %d", n, want)
	}
}

// probeColumn returns nil when column exists on table.
func probeColumn(database *sql.DB, dialect db.Dialect, table, column string) error {
	_, err := database.ExecContext(context.Background(), fmt.Sprintf("SELECT %s FROM %s WHERE 1 = 0", dialect.QuoteIdent(column), dialect.QuoteIdent(table)))
	return err
}

func exec(t *testing.T, database *sql.DB, dialect db.Dialect, query string, args ...any) {
	t.Helper()
	if _, err := database.ExecContext(context.Background(), db.Rebind(dialect, query), args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
