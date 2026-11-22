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

// TestOlderShapeGenerationRecreatesAndRebuildsOnce stores a shape fingerprint from before the current
// generation, as an upgraded site has, and syncs. The model's tables are recreated (the stale summary row
// is gone), exactly one whole-model rebuild is queued, and a later sync of the same generation keeps it.
func TestOlderShapeGenerationRecreatesAndRebuildsOnce(t *testing.T) {
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
			reg := doctype.NewRegistry()
			reg.Register(saleFacts())
			syncModels := func() error {
				return (&ModelSync{DB: database.DB, Dialect: dialect, Registry: reg, Owner: "worker-1"}).Sync(ctx)
			}
			mustSync(t, syncModels())
			assertOpenJobs(t, database.DB, dialect, 1)

			// Stand-in for a summary row written by the older build; the recreate must drop it.
			summary := dialect.QuoteIdent(summaryTable(saleFacts()))
			exec(t, database.DB, dialect, "INSERT INTO "+summary+
				" (business_date, legal_entity, status, grain, grain_value, row_count, total, line_amount) VALUES ('2026-01-01', '', '', '', '', 7, 0, 0)")
			exec(t, database.DB, dialect, "UPDATE "+dialect.QuoteIdent(shapeTable)+" SET fingerprint = 'older-generation' WHERE model = 'Sale'")

			mustSync(t, syncModels())
			if n := countRows(t, database.DB, dialect, "SELECT COUNT(*) FROM "+summary); n != 0 {
				t.Fatalf("summary rows after recreate = %d, want 0", n)
			}
			assertOpenJobs(t, database.DB, dialect, 1)
			stored := "SELECT COUNT(*) FROM " + dialect.QuoteIdent(shapeTable) + " WHERE model = 'Sale' AND fingerprint LIKE 'g2:%'"
			if n := countRows(t, database.DB, dialect, stored); n != 1 {
				t.Fatalf("stored fingerprint not at the current generation")
			}

			// The same generation again: nothing is recreated and no second rebuild is queued.
			exec(t, database.DB, dialect, "INSERT INTO "+summary+
				" (business_date, legal_entity, status, grain, grain_value, row_count, total, line_amount) VALUES ('2026-01-02', '', '', '', '', 3, 0, 0)")
			mustSync(t, syncModels())
			if n := countRows(t, database.DB, dialect, "SELECT COUNT(*) FROM "+summary); n != 1 {
				t.Fatalf("summary rows on a same-generation sync = %d, want 1", n)
			}
			assertOpenJobs(t, database.DB, dialect, 1)
		})
	}
}
