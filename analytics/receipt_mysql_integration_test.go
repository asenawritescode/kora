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

	"github.com/asenawritescode/kora/db"
	"github.com/go-sql-driver/mysql"
)

func TestMySQLAnalyticsReceiptDeduplicatesReplayAfterCommit(t *testing.T) {
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
	databaseName := fmt.Sprintf("kora_analytics_receipt_%d", time.Now().UnixNano())
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
	if err := BootstrapTables(database, db.Resolve("mysql")); err != nil {
		t.Fatal("bootstrap analytics tables:", err)
	}
	registry := testRegistry(buildTestDocType("Customer", false, field("name", "Data", "Name", true)))
	if _, err := database.Exec(`CREATE TABLE tabCustomer (name VARCHAR(64) PRIMARY KEY, creation DATETIME NOT NULL)`); err != nil {
		t.Fatal("create source table:", err)
	}
	if _, err := database.Exec(`INSERT INTO tabCustomer (name, creation) VALUES ('CUST-1', '2026-10-14 00:00:00')`); err != nil {
		t.Fatal("seed source row:", err)
	}
	if _, err := database.Exec(`CREATE TABLE _kora_event_sequence (site VARCHAR(140) PRIMARY KEY, last_sequence BIGINT NOT NULL)`); err != nil {
		t.Fatal("create source sequence table:", err)
	}
	if _, err := database.Exec(`CREATE TABLE _kora_tenant_event_sequence (id INTEGER PRIMARY KEY CHECK (id = 1), last_sequence BIGINT NOT NULL)`); err != nil {
		t.Fatal("create tenant source sequence table:", err)
	}
	if _, err := database.Exec(`INSERT INTO _kora_event_sequence (site, last_sequence) VALUES ('tenant-a', 1)`); err != nil {
		t.Fatal("seed source sequence:", err)
	}
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := Backfill(database, db.Resolve("mysql"), "tenant-a", registry, from, ""); err != nil {
		t.Fatal("rebuild analytics snapshot:", err)
	}
	event := ChangeEvent{
		ID: "event-committed-before-wal-ack", Site: "tenant-a", Doctype: "Customer", DocName: "CUST-1",
		Sequence: "1", Operation: EventInsert, Timestamp: time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC),
		Data: map[string]any{"name": "Acme"},
	}
	first := NewWorker(newFakeBus(), database, db.Resolve("mysql"), registry, "tenant-a", LoadConfig())
	first.process(event)
	if err := first.Flush(); err != nil {
		t.Fatal("flush first delivery:", err)
	}
	var storedSequence sql.NullInt64
	if err := database.QueryRow(`SELECT source_sequence FROM _kora_analytics_event_receipt WHERE site = ? AND event_id = ?`, "tenant-a", event.ID).Scan(&storedSequence); err != nil || !storedSequence.Valid || storedSequence.Int64 != 1 {
		t.Fatalf("MySQL analytics receipt source sequence = %+v, error=%v; want 1", storedSequence, err)
	}

	// A new worker models process restart after the SQL commit but before the
	// WAL acknowledgment reached durable storage. The same event is already
	// represented by the rebuild snapshot, then by its committed receipt.
	restarted := NewWorker(newFakeBus(), database, db.Resolve("mysql"), registry, "tenant-a", LoadConfig())
	restarted.process(event)
	secondEvent := ChangeEvent{
		ID: "event-after-rebuild-cutoff", Sequence: "2", Site: "tenant-a", Doctype: "Customer", DocName: "CUST-2",
		Operation: EventInsert, Timestamp: time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC),
		Data: map[string]any{"name": "Beta"},
	}
	insertTx, err := database.Begin()
	if err != nil {
		t.Fatal("begin source mutation:", err)
	}
	if _, err := insertTx.Exec(`INSERT INTO tabCustomer (name, creation) VALUES ('CUST-2', '2026-10-14 01:00:00')`); err != nil {
		_ = insertTx.Rollback()
		t.Fatal("insert source mutation:", err)
	}
	if _, err := insertTx.Exec(`UPDATE _kora_event_sequence SET last_sequence = 2 WHERE site = 'tenant-a'`); err != nil {
		_ = insertTx.Rollback()
		t.Fatal("advance source sequence:", err)
	}
	if err := insertTx.Commit(); err != nil {
		t.Fatal("commit source mutation:", err)
	}
	restarted.process(secondEvent)

	// Hold the same transaction-scoped tenant lock used by Backfill while a
	// worker tries to flush. The staged snapshot and cutoff become visible
	// together; the waiting flush must then skip that covered event.
	rebuildTx, err := database.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		t.Fatal("begin rebuild coordination transaction:", err)
	}
	if _, err := lockRebuildStateTx(context.Background(), rebuildTx, db.Resolve("mysql"), "tenant-a"); err != nil {
		_ = rebuildTx.Rollback()
		t.Fatal("lock rebuild state:", err)
	}
	if _, err := rebuildTx.Exec(`INSERT INTO _kora_analytics_daily (site, doctype, metric, dimension, date, value)
		VALUES ('tenant-a', 'Customer', 'customer_count', '', '2026-10-14', 2)
		ON DUPLICATE KEY UPDATE value = 2`); err != nil {
		_ = rebuildTx.Rollback()
		t.Fatal("stage rebuild rollup:", err)
	}
	if err := saveRebuildCheckpointTx(context.Background(), rebuildTx, db.Resolve("mysql"), "tenant-a", "", "2026-01-01", 2); err != nil {
		_ = rebuildTx.Rollback()
		t.Fatal("stage rebuild checkpoint:", err)
	}
	flushDone := make(chan error, 1)
	go func() { flushDone <- restarted.Flush() }()
	select {
	case err := <-flushDone:
		_ = rebuildTx.Rollback()
		t.Fatalf("worker flush crossed an active rebuild lock: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if err := rebuildTx.Commit(); err != nil {
		t.Fatal("commit rebuild snapshot and checkpoint:", err)
	}
	select {
	case err := <-flushDone:
		if err != nil {
			t.Fatal("flush after rebuild lock release:", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("worker flush did not resume after rebuild commit")
	}
	var rollup, receipts int
	if err := database.QueryRow(`SELECT value FROM _kora_analytics_daily WHERE site = ? AND doctype = ? AND metric = ? AND dimension = '' AND date = ?`, "tenant-a", "Customer", "customer_count", "2026-10-14").Scan(&rollup); err != nil {
		t.Fatal("read rollup after replay:", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_analytics_event_receipt WHERE site = ?`, "tenant-a").Scan(&receipts); err != nil {
		t.Fatal("read event receipt:", err)
	}
	if rollup != 2 || receipts != 2 {
		t.Fatalf("rollup/receipt counts = %d/%d, want 2/2", rollup, receipts)
	}
}
