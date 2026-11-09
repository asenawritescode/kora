//go:build integration

package analytics

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	_ "github.com/lib/pq"
)

func TestPostgresRebuildCutoffSerializesWithFlush(t *testing.T) {
	rootDSN := strings.TrimSpace(os.Getenv("KORA_ANALYTICS_DISPOSABLE_POSTGRES_DSN"))
	if rootDSN == "" {
		t.Skip("set KORA_ANALYTICS_DISPOSABLE_POSTGRES_DSN to a disposable loopback PostgreSQL database")
	}
	rootURL, err := url.Parse(rootDSN)
	if err != nil || rootURL.Scheme != "postgres" && rootURL.Scheme != "postgresql" {
		t.Fatal("disposable PostgreSQL test requires a postgres:// or postgresql:// DSN")
	}
	host := rootURL.Hostname()
	if !strings.EqualFold(host, "localhost") {
		address, parseErr := netip.ParseAddr(host)
		if parseErr != nil || !address.IsLoopback() {
			t.Fatal("refusing analytics integration test against non-loopback PostgreSQL")
		}
	}
	if _, _, err := net.SplitHostPort(rootURL.Host); err != nil {
		t.Fatal("disposable PostgreSQL DSN must include an explicit loopback port:", err)
	}
	root, err := sql.Open("postgres", rootDSN)
	if err != nil {
		t.Fatal("open disposable PostgreSQL:", err)
	}
	if err := root.Ping(); err != nil {
		_ = root.Close()
		t.Skipf("disposable PostgreSQL unavailable: %v", err)
	}
	schema := fmt.Sprintf("kora_analytics_%x", time.Now().UnixNano())
	if _, err := root.Exec(`CREATE SCHEMA "` + schema + `"`); err != nil {
		_ = root.Close()
		t.Fatal("create isolated PostgreSQL schema:", err)
	}
	t.Cleanup(func() {
		_, _ = root.Exec(`DROP SCHEMA "` + schema + `" CASCADE`)
		_ = root.Close()
	})
	testURL := *rootURL
	query := testURL.Query()
	query.Set("search_path", schema)
	testURL.RawQuery = query.Encode()
	database, err := sql.Open("postgres", testURL.String())
	if err != nil {
		t.Fatal("open isolated PostgreSQL schema:", err)
	}
	database.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = database.Close() })
	dialect := db.Resolve("postgres")
	if err = BootstrapTables(context.Background(), database, dialect); err != nil {
		t.Fatal("bootstrap analytics schema:", err)
	}
	projectionDefinition := contract.ProjectionDefinition{ID: "task_snapshot", OwnerModuleID: "analytics.task", ScopePolicy: contract.ScopeTenantShared, FactKey: "id", AllowedFields: []string{"id"}, Sources: []contract.ProjectionSource{{OwnerModuleID: "task.domain", QueryName: "tasks", OutputSchema: contract.SchemaRef{ID: "task.output", Version: 1}}}, OutputSchema: contract.SchemaRef{ID: "task.output", Version: 1}, RebuildStrategy: "consistent_snapshot"}
	projection := PublishedProjectionSnapshot{Site: "tenant-a", Definition: projectionDefinition, Scope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}, SourceID: "published_sources", SourceRevision: "task-r1", Cursor: `{"task.domain/tasks":"cursor-1"}`, AsOf: time.Date(2026, 2, 4, 0, 0, 0, 0, time.UTC), Freshness: contract.QueryFresh, Data: []byte(`[{"id":"task-1"}]`)}
	if err := SavePublishedProjection(context.Background(), database, dialect, projection); err != nil {
		t.Fatal("save PostgreSQL projection snapshot:", err)
	}
	if err := MarkPublishedProjectionStale(context.Background(), database, dialect, "tenant-a", projectionDefinition, projection.Scope, "published_sources"); err != nil {
		t.Fatal("mark PostgreSQL projection stale:", err)
	}
	loadedProjection, err := LoadPublishedProjection(context.Background(), database, dialect, "tenant-a", projectionDefinition, projection.Scope, "published_sources")
	if err != nil || string(loadedProjection.Data) != string(projection.Data) || loadedProjection.Cursor != projection.Cursor || loadedProjection.Freshness != contract.QueryStale {
		t.Fatalf("PostgreSQL retained projection = %#v, error=%v", loadedProjection, err)
	}
	for _, query := range []string{
		`CREATE TABLE _kora_event_sequence (site VARCHAR(140) PRIMARY KEY, last_sequence BIGINT NOT NULL)`,
		`INSERT INTO _kora_event_sequence (site, last_sequence) VALUES ('tenant-a', 1)`,
		`CREATE TABLE _kora_tenant_event_sequence (id INTEGER PRIMARY KEY CHECK (id = 1), last_sequence BIGINT NOT NULL)`,
		`INSERT INTO _kora_tenant_event_sequence (id, last_sequence) VALUES (1, 1)`,
		`CREATE TABLE "tabCustomer" (name VARCHAR(64) PRIMARY KEY, creation TIMESTAMP NOT NULL)`,
		`INSERT INTO "tabCustomer" (name, creation) VALUES ('CUST-1', '2026-10-14 00:00:00')`,
	} {
		if _, err := database.Exec(query); err != nil {
			t.Fatal("seed isolated PostgreSQL source:", err)
		}
	}
	registry := testRegistry(buildTestDocType("Customer", false, field("name", "Data", "Name", true)))
	if _, err = Backfill(context.Background(), database, dialect, "tenant-a", registry, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ""); err != nil {
		t.Fatal("rebuild PostgreSQL analytics snapshot:", err)
	}
	worker := NewWorker(newFakeBus(), database, dialect, registry, "tenant-a", LoadConfig())
	worker.process(ChangeEvent{ID: "pg-event-1", Sequence: "1", Site: "tenant-a", Doctype: "Customer", Operation: EventInsert,
		Timestamp: time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC), Data: map[string]any{"name": "CUST-1"}})
	if err := worker.Flush(); err != nil {
		t.Fatal("flush PostgreSQL event represented by snapshot:", err)
	}
	var storedSequence sql.NullInt64
	if err := database.QueryRow(`SELECT source_sequence FROM _kora_analytics_event_receipt WHERE site = $1 AND event_id = $2`, "tenant-a", "pg-event-1").Scan(&storedSequence); err != nil || !storedSequence.Valid || storedSequence.Int64 != 1 {
		t.Fatalf("PostgreSQL analytics receipt source sequence = %+v, error=%v; want 1", storedSequence, err)
	}

	mutation, err := database.Begin()
	if err != nil {
		t.Fatal("begin PostgreSQL source mutation:", err)
	}
	if _, err := mutation.Exec(`INSERT INTO "tabCustomer" (name, creation) VALUES ('CUST-2', '2026-10-14 01:00:00')`); err != nil {
		_ = mutation.Rollback()
		t.Fatal("insert PostgreSQL source row:", err)
	}
	if _, err := mutation.Exec(`UPDATE _kora_event_sequence SET last_sequence = 2 WHERE site = 'tenant-a'`); err != nil {
		_ = mutation.Rollback()
		t.Fatal("advance PostgreSQL source sequence:", err)
	}
	if _, err := mutation.Exec(`UPDATE _kora_tenant_event_sequence SET last_sequence = 2 WHERE id = 1`); err != nil {
		_ = mutation.Rollback()
		t.Fatal("advance PostgreSQL tenant source sequence:", err)
	}
	if err := mutation.Commit(); err != nil {
		t.Fatal("commit PostgreSQL source mutation:", err)
	}
	secondEvent := ChangeEvent{ID: "pg-event-2", Sequence: "2", Site: "tenant-a", Doctype: "Customer", DocName: "CUST-2",
		Operation: EventInsert, Timestamp: time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC), Data: map[string]any{"name": "CUST-2"}}
	worker.process(secondEvent)
	rebuildTx, err := database.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		t.Fatal("begin PostgreSQL rebuild coordination:", err)
	}
	if _, err := lockRebuildStateTx(context.Background(), rebuildTx, dialect, "tenant-a"); err != nil {
		_ = rebuildTx.Rollback()
		t.Fatal("lock PostgreSQL rebuild state:", err)
	}
	if _, err := rebuildTx.Exec(`INSERT INTO _kora_analytics_daily (site, doctype, metric, dimension, date, scope_kind, legal_entity_id, value)
		VALUES ('tenant-a', 'Customer', 'customer_count', '', '2026-10-14', 'tenant', '', 2)
		ON CONFLICT (site, doctype, metric, dimension, date, scope_kind, legal_entity_id) DO UPDATE SET value = EXCLUDED.value`); err != nil {
		_ = rebuildTx.Rollback()
		t.Fatal("stage PostgreSQL rebuild rollup:", err)
	}
	if err := saveRebuildCheckpointTx(context.Background(), rebuildTx, dialect, "tenant-a", "", "2026-01-01", 2); err != nil {
		_ = rebuildTx.Rollback()
		t.Fatal("stage PostgreSQL rebuild checkpoint:", err)
	}
	flushDone := make(chan error, 1)
	go func() { flushDone <- worker.Flush() }()
	select {
	case err := <-flushDone:
		_ = rebuildTx.Rollback()
		t.Fatalf("PostgreSQL flush crossed active rebuild lock: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if err := rebuildTx.Commit(); err != nil {
		t.Fatal("commit PostgreSQL rebuild:", err)
	}
	select {
	case err := <-flushDone:
		if err != nil {
			t.Fatal("PostgreSQL flush after rebuild:", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("PostgreSQL flush did not resume after rebuild commit")
	}
	var rollup float64
	if err := database.QueryRow(`SELECT value FROM _kora_analytics_daily WHERE site = $1 AND doctype = $2 AND metric = $3 AND dimension = '' AND date = $4`, "tenant-a", "Customer", "customer_count", "2026-10-14").Scan(&rollup); err != nil {
		t.Fatal("read PostgreSQL rollup:", err)
	}
	if rollup != 2 {
		t.Fatalf("PostgreSQL rollup after cutoff handoff = %v, want 2", rollup)
	}

	// A source-column failure after the daily delete/count stages must roll back
	// the entire rebuild, including monthly materialization and its checkpoint.
	brokenRegistry := testRegistry(buildTestDocType("Customer", false,
		field("name", "Data", "Name", true),
		field("missing_amount", "Currency", "Missing Amount", false),
	))
	var monthlyBefore float64
	if err := database.QueryRow(`SELECT value FROM _kora_analytics_monthly WHERE site = $1 AND doctype = $2 AND metric = $3 AND dimension = '' AND month = $4`, "tenant-a", "Customer", "customer_count", "2026-10-01").Scan(&monthlyBefore); err != nil {
		t.Fatal("read PostgreSQL monthly rollup before failed rebuild:", err)
	}
	if _, err := Backfill(context.Background(), database, dialect, "tenant-a", brokenRegistry, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ""); err == nil {
		t.Fatal("expected PostgreSQL rebuild failure for missing source column")
	}
	if err := database.QueryRow(`SELECT value FROM _kora_analytics_daily WHERE site = $1 AND doctype = $2 AND metric = $3 AND dimension = '' AND date = $4`, "tenant-a", "Customer", "customer_count", "2026-10-14").Scan(&rollup); err != nil {
		t.Fatal("read PostgreSQL daily rollup after failed rebuild:", err)
	}
	if rollup != 2 {
		t.Fatalf("PostgreSQL daily rollup after failed rebuild = %v, want previously committed 2", rollup)
	}
	var monthly float64
	if err := database.QueryRow(`SELECT value FROM _kora_analytics_monthly WHERE site = $1 AND doctype = $2 AND metric = $3 AND dimension = '' AND month = $4`, "tenant-a", "Customer", "customer_count", "2026-10-01").Scan(&monthly); err != nil {
		t.Fatal("read PostgreSQL monthly rollup after failed rebuild:", err)
	}
	if monthly != monthlyBefore {
		t.Fatalf("PostgreSQL monthly rollup after failed rebuild = %v, want previously committed %v", monthly, monthlyBefore)
	}
	var checkpoints int
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_analytics_rebuild_checkpoint WHERE site = $1`, "tenant-a").Scan(&checkpoints); err != nil {
		t.Fatal("read PostgreSQL checkpoint after failed rebuild:", err)
	}
	if checkpoints != 1 {
		t.Fatalf("PostgreSQL checkpoints after failed rebuild = %d, want prior committed checkpoint", checkpoints)
	}
	verifyFlushFailureRetry(t, database, dialect, "tenant-flush-postgres", registry,
		func() error {
			if _, err := database.Exec(`CREATE FUNCTION fail_analytics_flush() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN RAISE EXCEPTION 'injected analytics flush failure'; END; $$`); err != nil {
				return err
			}
			_, err := database.Exec(`CREATE TRIGGER fail_analytics_flush BEFORE INSERT ON _kora_analytics_daily
				FOR EACH ROW EXECUTE FUNCTION fail_analytics_flush()`)
			return err
		},
		func() error {
			if _, err := database.Exec(`DROP TRIGGER fail_analytics_flush ON _kora_analytics_daily`); err != nil {
				return err
			}
			_, err := database.Exec(`DROP FUNCTION fail_analytics_flush()`)
			return err
		},
	)
}
