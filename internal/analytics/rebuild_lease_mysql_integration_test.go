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

	"github.com/asenawritescode/kora/internal/db"
	"github.com/go-sql-driver/mysql"
)

func TestMySQLRebuildLeaseFencesExpiredOwner(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KORA_ANALYTICS_DISPOSABLE_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("set KORA_ANALYTICS_DISPOSABLE_MYSQL_DSN to a disposable loopback MySQL service")
	}
	rootConfig, err := mysql.ParseDSN(dsn)
	if err != nil || rootConfig.Net != "tcp" {
		t.Fatal("disposable analytics test requires a TCP MySQL DSN:", err)
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
	databaseName := fmt.Sprintf("kora_analytics_lease_%d", time.Now().UnixNano())
	if _, err := root.Exec("CREATE DATABASE `" + databaseName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = root.Close()
		t.Fatal("create isolated analytics database:", err)
	}
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS `" + databaseName + "`"); _ = root.Close() })
	databaseConfig := *rootConfig
	databaseConfig.DBName = databaseName
	database, err := sql.Open("mysql", databaseConfig.FormatDSN())
	if err != nil {
		t.Fatal("open isolated analytics database:", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	dialect := db.Resolve("mysql")
	if err := BootstrapTables(database, dialect); err != nil {
		t.Fatal("bootstrap analytics schema:", err)
	}
	job := &RebuildJob{ID: "lease-job", Site: "tenant-a", Status: "queued", StartedAt: time.Now().UTC(), From: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := CreateRebuildJob(database, dialect, job); err != nil {
		t.Fatal("create analytics rebuild job:", err)
	}
	first, staleOwner := claimTwoRebuildRunners(t, database, dialect, job.Site)
	if _, err := database.Exec(`UPDATE _kora_analytics_rebuild_lease SET lease_until = DATE_SUB(CURRENT_TIMESTAMP, INTERVAL 1 SECOND) WHERE site = ? AND job_id = ?`, job.Site, job.ID); err != nil {
		t.Fatal("expire first runner lease:", err)
	}
	second, err := ClaimNextRebuildJob(context.Background(), database, dialect, job.Site, staleOwner)
	if err != nil || second == nil || second.FencingToken != 2 {
		t.Fatalf("takeover claim = (%#v, %v), want fence 2", second, err)
	}
	if err := finishClaimedRebuild(context.Background(), database, dialect, first, "completed", 1, ""); err != ErrRebuildLeaseLost {
		t.Fatalf("stale runner completion error = %v, want ErrRebuildLeaseLost", err)
	}
	if err := finishClaimedRebuild(context.Background(), database, dialect, second, "completed", 3, ""); err != nil {
		t.Fatal("current runner completion:", err)
	}
	persisted, err := GetRebuildJob(database, dialect, job.Site, job.ID)
	if err != nil {
		t.Fatal("read completed rebuild:", err)
	}
	if persisted.Status != "completed" || persisted.Metrics != 3 {
		t.Fatalf("completed job = %#v, want completed metrics 3", persisted)
	}
}
