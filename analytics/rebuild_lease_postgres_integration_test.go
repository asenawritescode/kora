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

	"github.com/asenawritescode/kora/db"
	_ "github.com/lib/pq"
)

func TestPostgresRebuildLeaseFencesExpiredOwner(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KORA_ANALYTICS_DISPOSABLE_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set KORA_ANALYTICS_DISPOSABLE_POSTGRES_DSN to a disposable loopback PostgreSQL database")
	}
	rootURL, err := url.Parse(dsn)
	if err != nil || (rootURL.Scheme != "postgres" && rootURL.Scheme != "postgresql") {
		t.Fatal("disposable analytics test requires a PostgreSQL DSN")
	}
	host := rootURL.Hostname()
	if !strings.EqualFold(host, "localhost") {
		address, parseErr := netip.ParseAddr(host)
		if parseErr != nil || !address.IsLoopback() {
			t.Fatal("refusing analytics integration test against non-loopback PostgreSQL")
		}
	}
	if _, _, err := net.SplitHostPort(rootURL.Host); err != nil {
		t.Fatal("disposable PostgreSQL DSN must include an explicit port:", err)
	}
	root, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("open disposable PostgreSQL:", err)
	}
	if err := root.Ping(); err != nil {
		_ = root.Close()
		t.Skipf("disposable PostgreSQL unavailable: %v", err)
	}
	schema := fmt.Sprintf("kora_analytics_lease_%x", time.Now().UnixNano())
	if _, err := root.Exec(`CREATE SCHEMA "` + schema + `"`); err != nil {
		_ = root.Close()
		t.Fatal("create isolated analytics schema:", err)
	}
	t.Cleanup(func() { _, _ = root.Exec(`DROP SCHEMA "` + schema + `" CASCADE`); _ = root.Close() })
	isolatedURL := *rootURL
	query := isolatedURL.Query()
	query.Set("search_path", schema)
	isolatedURL.RawQuery = query.Encode()
	database, err := sql.Open("postgres", isolatedURL.String())
	if err != nil {
		t.Fatal("open isolated PostgreSQL schema:", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	dialect := db.Resolve("postgres")
	if err := BootstrapTables(database, dialect); err != nil {
		t.Fatal("bootstrap analytics schema:", err)
	}
	if _, err := database.Exec(`UPDATE _kora_analytics_bootstrap SET version = 3 WHERE id = 1`); err != nil {
		t.Fatal("set pre-lease analytics version:", err)
	}
	if _, err := database.Exec(`DROP TABLE _kora_analytics_rebuild_lease`); err != nil {
		t.Fatal("remove v4 rebuild lease table:", err)
	}
	if err := BootstrapTables(database, dialect); err != nil {
		t.Fatal("upgrade analytics v3 schema:", err)
	}
	job := &RebuildJob{ID: "lease-job", Site: "tenant-a", Status: "queued", StartedAt: time.Now().UTC(), From: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := CreateRebuildJob(database, dialect, job); err != nil {
		t.Fatal("create analytics rebuild job:", err)
	}
	first, staleOwner := claimTwoRebuildRunners(t, database, dialect, job.Site)
	if _, err := database.Exec(`UPDATE _kora_analytics_rebuild_lease SET lease_until = CURRENT_TIMESTAMP - INTERVAL '1 second' WHERE site = $1 AND job_id = $2`, job.Site, job.ID); err != nil {
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
