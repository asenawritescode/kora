//go:build integration

package analytics

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/go-sql-driver/mysql"
)

func TestMySQLBackfillFailurePreservesPreviouslyCommittedRollups(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KORA_ANALYTICS_DISPOSABLE_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("set KORA_ANALYTICS_DISPOSABLE_MYSQL_DSN to a disposable loopback MySQL service")
	}
	rootConfig, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse disposable MySQL DSN:", err)
	}
	if rootConfig.Net != "tcp" {
		t.Fatal("disposable analytics test requires a loopback TCP MySQL DSN")
	}
	host, _, err := net.SplitHostPort(rootConfig.Addr)
	if err != nil {
		t.Fatal("parse disposable MySQL address:", err)
	}
	if !strings.EqualFold(host, "localhost") {
		address, parseErr := netip.ParseAddr(strings.Trim(host, "[]"))
		if parseErr != nil || !address.IsLoopback() {
			t.Fatal("refusing analytics integration test against non-loopback MySQL")
		}
	}

	root, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open disposable MySQL:", err)
	}
	if err := root.Ping(); err != nil {
		_ = root.Close()
		t.Skipf("disposable MySQL unavailable: %v", err)
	}
	databaseName := fmt.Sprintf("kora_analytics_rebuild_%d", time.Now().UnixNano())
	if _, err := root.Exec("CREATE DATABASE `" + databaseName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = root.Close()
		t.Fatal("create isolated analytics database:", err)
	}
	t.Cleanup(func() {
		_, _ = root.Exec("DROP DATABASE IF EXISTS `" + databaseName + "`")
		_ = root.Close()
	})

	databaseConfig := *rootConfig
	databaseConfig.DBName = databaseName
	database, err := sql.Open("mysql", databaseConfig.FormatDSN())
	if err != nil {
		t.Fatal("open isolated analytics database:", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, err := database.Exec(`CREATE TABLE tabTask (name VARCHAR(64) PRIMARY KEY, creation DATETIME NOT NULL, doc_status INT NOT NULL) ENGINE=InnoDB`); err != nil {
		t.Fatal("create source table:", err)
	}
	if _, err := database.Exec(`CREATE TABLE _kora_event_sequence (site VARCHAR(140) PRIMARY KEY, last_sequence BIGINT NOT NULL)`); err != nil {
		t.Fatal("create source sequence table:", err)
	}
	for _, query := range []string{
		`INSERT INTO tabTask (name, creation, doc_status) VALUES ('task-1', '2026-02-03 12:00:00', 1)`,
		`INSERT INTO _kora_analytics_daily (site, doctype, metric, dimension, date, value) VALUES ('tenant-a', 'Task', 'task_count', '', '2026-02-03', 91), ('tenant-a', 'Task', 'task_created_daily', '', '2026-02-03', 92), ('tenant-a', 'Task', 'task_sum_missing_amount', '', '2026-02-03', 93)`,
		`INSERT INTO _kora_analytics_monthly (site, doctype, metric, dimension, month, value) VALUES ('tenant-a', 'Task', 'task_count', '', '2026-02-01', 81), ('tenant-a', 'Task', 'task_created_daily', '', '2026-02-01', 82), ('tenant-a', 'Task', 'task_sum_missing_amount', '', '2026-02-01', 83)`,
	} {
		if strings.Contains(query, "_kora_analytics_") {
			if err = BootstrapTables(context.Background(), database, db.Resolve("mysql")); err != nil {
				t.Fatal("bootstrap analytics schema:", err)
			}
		}
		if _, err := database.Exec(query); err != nil {
			t.Fatal("seed analytics rebuild fixture:", err)
		}
	}
	projectionDefinition := contract.ProjectionDefinition{ID: "task_snapshot", OwnerModuleID: "analytics.task", ScopePolicy: contract.ScopeTenantShared, FactKey: "id", AllowedFields: []string{"id"}, Sources: []contract.ProjectionSource{{OwnerModuleID: "task.domain", QueryName: "tasks", OutputSchema: contract.SchemaRef{ID: "task.output", Version: 1}}}, OutputSchema: contract.SchemaRef{ID: "task.output", Version: 1}, RebuildStrategy: "consistent_snapshot"}
	projectionStore := PublishedProjectionStore{DB: database, Dialect: db.Resolve("mysql"), Site: "tenant-a"}
	projection := PublishedProjectionSnapshot{Definition: projectionDefinition, Scope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}, SourceID: "published_sources", SourceRevision: "task-r1", Cursor: `{"task.domain/tasks":"cursor-1"}`, AsOf: time.Date(2026, 2, 4, 0, 0, 0, 0, time.UTC), Freshness: contract.QueryFresh, Data: []byte(`[{"id":"task-1"}]`)}
	if err := projectionStore.Save(context.Background(), projection); err != nil {
		t.Fatal("save MySQL projection snapshot:", err)
	}
	if err := projectionStore.MarkStale(context.Background(), projectionDefinition, projection.Scope, "published_sources"); err != nil {
		t.Fatal("mark MySQL projection stale:", err)
	}
	loaded, err := projectionStore.Load(context.Background(), projectionDefinition, projection.Scope, "published_sources")
	if err != nil || string(loaded.Data) != string(projection.Data) || loaded.Cursor != projection.Cursor || loaded.Freshness != contract.QueryStale {
		t.Fatalf("MySQL retained projection = %#v, error=%v", loaded, err)
	}

	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Task", Fields: []doctype.Field{{Fieldname: "missing_amount", Fieldtype: "Currency"}}})
	if _, err := Backfill(context.Background(), database, db.Resolve("mysql"), "tenant-a", registry, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ""); err == nil {
		t.Fatal("expected source query failure for missing amount column")
	}
	var checkpoints int
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_analytics_rebuild_checkpoint WHERE site = ?`, "tenant-a").Scan(&checkpoints); err != nil {
		t.Fatal("check failed rebuild did not persist cutoff:", err)
	}
	if checkpoints != 0 {
		t.Fatalf("failed rebuild checkpoints = %d, want 0", checkpoints)
	}

	for _, table := range []struct {
		name  string
		field string
		month string
		want  map[string]float64
	}{
		{name: "_kora_analytics_daily", field: "date", month: "2026-02-03", want: map[string]float64{"task_count": 91, "task_created_daily": 92, "task_sum_missing_amount": 93}},
		{name: "_kora_analytics_monthly", field: "month", month: "2026-02-01", want: map[string]float64{"task_count": 81, "task_created_daily": 82, "task_sum_missing_amount": 83}},
	} {
		for metric, want := range table.want {
			var got float64
			query := fmt.Sprintf("SELECT value FROM %s WHERE site = ? AND doctype = ? AND metric = ? AND dimension = '' AND %s = ?", table.name, table.field)
			if err := database.QueryRow(query, "tenant-a", "Task", metric, table.month).Scan(&got); err != nil {
				t.Fatalf("read preserved %s/%s rollup: %v", table.name, metric, err)
			}
			if got != want {
				t.Errorf("%s/%s value = %v, want previously committed %v", table.name, metric, got, want)
			}
		}
	}

	verifyFlushFailureRetry(t, database, db.Resolve("mysql"), "tenant-flush-mysql", registry,
		func() error {
			_, err := database.Exec(`CREATE TRIGGER fail_analytics_flush BEFORE INSERT ON _kora_analytics_daily
				FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'injected analytics flush failure'`)
			return err
		},
		func() error {
			_, err := database.Exec(`DROP TRIGGER fail_analytics_flush`)
			return err
		},
	)
}
