//go:build integration

package analytics

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	_ "github.com/mattn/go-sqlite3"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func TestLibSQLRebuildCutoffAndLaterEvents(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KORA_ANALYTICS_DISPOSABLE_LIBSQL_DSN"))
	if dsn == "" {
		localDriver := false
		for _, name := range sql.Drivers() {
			if name == "sqlite" || name == "sqlite3" {
				localDriver = true
				break
			}
		}
		if !localDriver {
			t.Skip("set KORA_ANALYTICS_DISPOSABLE_LIBSQL_DSN or register sqlite/sqlite3 for the local LibSQL transport")
		}
		dsn = "file:" + filepath.Join(t.TempDir(), "analytics.db") + "?mode=rwc"
	}
	database, err := sql.Open("libsql", dsn)
	if err != nil {
		t.Fatal("open disposable local LibSQL database:", err)
	}
	database.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Ping(); err != nil {
		t.Fatal("ping disposable local LibSQL database:", err)
	}
	dialect := db.Resolve("libsql")
	if err := BootstrapTables(database, dialect); err != nil {
		t.Fatal("bootstrap analytics schema:", err)
	}
	projectionDefinition := contract.ProjectionDefinition{
		ID: "ar_balance", OwnerModuleID: "finance.ar", ScopePolicy: contract.ScopeEntityOwned, FactKey: "period", AllowedFields: []string{"period", "amount"},
		Sources:      []contract.ProjectionSource{{OwnerModuleID: "sales.orders", QueryName: "balances", OutputSchema: contract.SchemaRef{ID: "balance.output", Version: 2}}},
		OutputSchema: contract.SchemaRef{ID: "balance.output", Version: 2}, RebuildStrategy: "consistent_snapshot",
	}
	projectionScope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	projectionStore := PublishedProjectionStore{DB: database, Dialect: dialect, Site: "tenant-a"}
	projectionData := []byte(`[{"period":"2026-10","amount":21}]`)
	projectionSnapshot := PublishedProjectionSnapshot{Definition: projectionDefinition, Scope: projectionScope, SourceID: "published_sources", SourceRevision: "source-r1", Cursor: `{"sales.orders/balances":"log-8"}`, AsOf: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), Freshness: contract.QueryFresh, Data: projectionData}
	if err := projectionStore.Save(context.Background(), projectionSnapshot); err != nil {
		t.Fatal("save complete LibSQL projection:", err)
	}
	loadedProjection, err := projectionStore.Load(context.Background(), projectionDefinition, projectionScope, "published_sources")
	if err != nil || string(loadedProjection.Data) != string(projectionData) || loadedProjection.Cursor != projectionSnapshot.Cursor || loadedProjection.Freshness != contract.QueryFresh {
		t.Fatalf("load LibSQL projection = %#v, error=%v", loadedProjection, err)
	}
	if err := projectionStore.MarkStale(context.Background(), projectionDefinition, projectionScope, "published_sources"); err != nil {
		t.Fatal("mark retained LibSQL projection stale:", err)
	}
	staleProjection, err := projectionStore.Load(context.Background(), projectionDefinition, projectionScope, "published_sources")
	if err != nil || string(staleProjection.Data) != string(projectionData) || staleProjection.Freshness != contract.QueryStale {
		t.Fatalf("stale LibSQL projection = %#v, error=%v", staleProjection, err)
	}
	secondProjection := projectionSnapshot
	secondProjection.SourceRevision = "source-r2"
	secondProjection.Data = []byte(`[{"period":"2026-10","amount":24}]`)
	if err := projectionStore.Save(context.Background(), secondProjection); err != nil {
		t.Fatal("save second LibSQL projection revision:", err)
	}
	latestProjection, err := projectionStore.Load(context.Background(), projectionDefinition, projectionScope, "published_sources")
	if err != nil || latestProjection.SourceRevision != "source-r2" || latestProjection.Freshness != contract.QueryFresh {
		t.Fatalf("latest LibSQL projection = %#v, error=%v", latestProjection, err)
	}
	var projectionHistory int
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_analytics_module_projection WHERE site = ? AND projection_id = ?`, "tenant-a", projectionDefinition.ID).Scan(&projectionHistory); err != nil || projectionHistory != 2 {
		t.Fatalf("LibSQL projection history count = %d, error=%v", projectionHistory, err)
	}
	wrongScope := projectionScope
	wrongScope.LegalEntityID = "entity-b"
	if _, err := projectionStore.Load(context.Background(), projectionDefinition, wrongScope, "published_sources"); err == nil {
		t.Fatal("LibSQL projection snapshot crossed legal-entity scope")
	}
	if _, err := database.Exec(`CREATE TABLE _kora_event_sequence (site TEXT PRIMARY KEY, last_sequence INTEGER NOT NULL)`); err != nil {
		t.Fatal("create source sequence table:", err)
	}
	if _, err := database.Exec(`INSERT INTO _kora_event_sequence (site, last_sequence) VALUES ('tenant-a', 1)`); err != nil {
		t.Fatal("seed source sequence:", err)
	}
	if _, err := database.Exec(`CREATE TABLE IF NOT EXISTS _kora_tenant_event_sequence (id INTEGER PRIMARY KEY CHECK (id = 1), last_sequence INTEGER NOT NULL)`); err != nil {
		t.Fatal("create tenant source sequence:", err)
	}
	if _, err := database.Exec(`CREATE TABLE tabCustomer (name TEXT PRIMARY KEY, creation TEXT NOT NULL)`); err != nil {
		t.Fatal("create source table:", err)
	}
	if _, err := database.Exec(`INSERT INTO tabCustomer (name, creation) VALUES ('CUST-1', '2026-10-14 00:00:00')`); err != nil {
		t.Fatal("seed source row:", err)
	}
	registry := testRegistry(buildTestDocType("Customer", false, field("name", "Data", "Name", true)))
	if _, err := Backfill(database, dialect, "tenant-a", registry, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ""); err != nil {
		t.Fatal("rebuild analytics snapshot:", err)
	}
	worker := NewWorker(newFakeBus(), database, dialect, registry, "tenant-a", LoadConfig())
	worker.process(ChangeEvent{ID: "libsql-event-1", Sequence: "1", Site: "tenant-a", Doctype: "Customer", Operation: EventInsert,
		Timestamp: time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC), Data: map[string]any{"name": "CUST-1"}})
	if err := worker.Flush(); err != nil {
		t.Fatal("flush event represented by rebuild:", err)
	}
	var storedSequence sql.NullInt64
	if err := database.QueryRow(`SELECT source_sequence FROM _kora_analytics_event_receipt WHERE site = ? AND event_id = ?`, "tenant-a", "libsql-event-1").Scan(&storedSequence); err != nil || !storedSequence.Valid || storedSequence.Int64 != 1 {
		t.Fatalf("LibSQL analytics receipt source sequence = %+v, error=%v; want 1", storedSequence, err)
	}
	var value float64
	query := `SELECT value FROM _kora_analytics_daily WHERE site = ? AND doctype = ? AND metric = ? AND dimension = '' AND date = ?`
	if err := database.QueryRow(query, "tenant-a", "Customer", "customer_count", "2026-10-14").Scan(&value); err != nil {
		t.Fatal("read rollup after cutoff-filtered event:", err)
	}
	if value != 1 {
		t.Fatalf("rollup after cutoff-filtered event = %v, want 1", value)
	}

	mutation, err := database.Begin()
	if err != nil {
		t.Fatal("begin later source mutation:", err)
	}
	if _, err := mutation.Exec(`INSERT INTO tabCustomer (name, creation) VALUES ('CUST-2', '2026-10-14 01:00:00')`); err != nil {
		_ = mutation.Rollback()
		t.Fatal("insert later source row:", err)
	}
	if _, err := mutation.Exec(`UPDATE _kora_event_sequence SET last_sequence = 2 WHERE site = 'tenant-a'`); err != nil {
		_ = mutation.Rollback()
		t.Fatal("advance later source sequence:", err)
	}
	if err := mutation.Commit(); err != nil {
		t.Fatal("commit later source mutation:", err)
	}
	worker.process(ChangeEvent{ID: "libsql-event-2", Sequence: "2", Site: "tenant-a", Doctype: "Customer", Operation: EventInsert,
		Timestamp: time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC), Data: map[string]any{"name": "CUST-2"}})
	// Keep the same tenant rebuild lock used by Backfill while a concurrent
	// flush sees the event. The snapshot and cutoff commit together; flush must
	// either wait or retain its batch for retry, never add the event twice.
	rebuildTx, err := database.Begin()
	if err != nil {
		t.Fatal("begin LibSQL rebuild coordination:", err)
	}
	if _, err := lockRebuildStateTx(context.Background(), rebuildTx, dialect, "tenant-a"); err != nil {
		_ = rebuildTx.Rollback()
		t.Fatal("lock LibSQL rebuild state:", err)
	}
	if _, err := rebuildTx.Exec(`INSERT INTO _kora_analytics_daily (site, doctype, metric, dimension, date, scope_kind, legal_entity_id, value)
		VALUES ('tenant-a', 'Customer', 'customer_count', '', '2026-10-14', 'tenant', '', 2)
		ON CONFLICT (site, doctype, metric, dimension, date, scope_kind, legal_entity_id) DO UPDATE SET value = excluded.value`); err != nil {
		_ = rebuildTx.Rollback()
		t.Fatal("stage LibSQL rebuild rollup:", err)
	}
	if err := saveRebuildCheckpointTx(context.Background(), rebuildTx, dialect, "tenant-a", "", "2026-01-01", 2); err != nil {
		_ = rebuildTx.Rollback()
		t.Fatal("stage LibSQL rebuild checkpoint:", err)
	}
	flushDone := make(chan error, 1)
	go func() { flushDone <- worker.Flush() }()
	var earlyFlushErr error
	select {
	case earlyFlushErr = <-flushDone:
		if earlyFlushErr == nil {
			_ = rebuildTx.Rollback()
			t.Fatal("LibSQL flush crossed active rebuild transaction")
		}
	case <-time.After(150 * time.Millisecond):
	}
	if err := rebuildTx.Commit(); err != nil {
		t.Fatal("commit LibSQL rebuild snapshot and checkpoint:", err)
	}
	if earlyFlushErr == nil {
		select {
		case err := <-flushDone:
			if err != nil {
				t.Fatal("LibSQL flush after rebuild commit:", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("LibSQL flush did not resume after rebuild commit")
		}
	} else if err := worker.Flush(); err != nil {
		t.Fatalf("retry retained LibSQL flush batch after rebuild commit: %v", err)
	}
	if err := database.QueryRow(query, "tenant-a", "Customer", "customer_count", "2026-10-14").Scan(&value); err != nil {
		t.Fatal("read rollup after concurrent post-cutoff event:", err)
	}
	if value != 2 {
		t.Fatalf("rollup after concurrent rebuild/flush = %v, want 2", value)
	}

	// Make the source unavailable so the rebuild fails after it has deleted the
	// old daily/monthly rows in its transaction.
	var monthlyBefore float64
	if err := database.QueryRow(`SELECT value FROM _kora_analytics_monthly WHERE site = ? AND doctype = ? AND metric = ? AND dimension = '' AND month = ?`, "tenant-a", "Customer", "customer_count", "2026-10-01").Scan(&monthlyBefore); err != nil {
		t.Fatal("read monthly rollup before failed rebuild:", err)
	}
	if _, err := database.Exec(`ALTER TABLE tabCustomer RENAME TO tabCustomer_unavailable`); err != nil {
		t.Fatal("temporarily hide LibSQL source table:", err)
	}
	_, rebuildErr := Backfill(database, dialect, "tenant-a", registry, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "")
	_, restoreErr := database.Exec(`ALTER TABLE tabCustomer_unavailable RENAME TO tabCustomer`)
	if restoreErr != nil {
		t.Fatal("restore LibSQL source table:", restoreErr)
	}
	if rebuildErr == nil {
		t.Fatal("expected rebuild failure for unavailable source table")
	}
	if err := database.QueryRow(query, "tenant-a", "Customer", "customer_count", "2026-10-14").Scan(&value); err != nil {
		t.Fatal("read daily rollup after failed rebuild:", err)
	}
	if value != 2 {
		t.Fatalf("daily rollup after failed rebuild = %v, want previously committed 2", value)
	}
	var monthly float64
	if err := database.QueryRow(`SELECT value FROM _kora_analytics_monthly WHERE site = ? AND doctype = ? AND metric = ? AND dimension = '' AND month = ?`, "tenant-a", "Customer", "customer_count", "2026-10-01").Scan(&monthly); err != nil {
		t.Fatal("read monthly rollup after failed rebuild:", err)
	}
	if monthly != monthlyBefore {
		t.Fatalf("monthly rollup after failed rebuild = %v, want previously committed %v", monthly, monthlyBefore)
	}
	var checkpoints int
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_analytics_rebuild_checkpoint WHERE site = ?`, "tenant-a").Scan(&checkpoints); err != nil {
		t.Fatal("read rebuild checkpoint after failed rebuild:", err)
	}
	if checkpoints != 1 {
		t.Fatalf("rebuild checkpoints after failed rebuild = %d, want prior committed checkpoint", checkpoints)
	}
	verifyFlushFailureRetry(t, database, dialect, "tenant-flush-libsql", registry,
		func() error {
			_, err := database.Exec(`CREATE TRIGGER fail_analytics_flush BEFORE INSERT ON _kora_analytics_daily
				BEGIN SELECT RAISE(ABORT, 'injected analytics flush failure'); END`)
			return err
		},
		func() error {
			_, err := database.Exec(`DROP TRIGGER fail_analytics_flush`)
			return err
		},
	)
}
