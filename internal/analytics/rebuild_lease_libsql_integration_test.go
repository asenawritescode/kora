//go:build integration

package analytics

import (
	"context"
	"database/sql"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/db"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func TestLibSQLRebuildLeaseFencesExpiredOwner(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KORA_ANALYTICS_DISPOSABLE_LIBSQL_DSN"))
	if dsn == "" {
		t.Skip("set KORA_ANALYTICS_DISPOSABLE_LIBSQL_DSN to a disposable loopback LibSQL HTTP endpoint")
	}
	endpoint, err := url.Parse(dsn)
	if err != nil || endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		t.Fatal("disposable LibSQL lease test requires an HTTP(S) endpoint")
	}
	host := endpoint.Hostname()
	if !strings.EqualFold(host, "localhost") {
		address, parseErr := netip.ParseAddr(host)
		if parseErr != nil || !address.IsLoopback() {
			t.Fatal("refusing analytics integration test against non-loopback LibSQL")
		}
	}
	if _, _, err := net.SplitHostPort(endpoint.Host); err != nil {
		t.Fatal("disposable LibSQL endpoint must include an explicit loopback port:", err)
	}
	database, err := sql.Open("libsql", dsn)
	if err != nil {
		t.Fatal("open disposable LibSQL:", err)
	}
	database.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Ping(); err != nil {
		t.Skipf("disposable LibSQL endpoint unavailable: %v", err)
	}
	dialect := db.Resolve("libsql")
	if err := BootstrapTables(database, dialect); err != nil {
		t.Fatal("bootstrap LibSQL analytics schema:", err)
	}
	suffix := strconv.FormatInt(time.Now().UTC().UnixNano(), 10)
	site := "tenant-lease-" + suffix
	job := &RebuildJob{
		ID: "lease-job-" + suffix, Site: site, Status: "queued", StartedAt: time.Now().UTC(),
		From: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := CreateRebuildJob(database, dialect, job); err != nil {
		t.Fatal("create LibSQL rebuild job:", err)
	}
	first, staleOwner := claimTwoRebuildRunners(t, database, dialect, job.Site)
	if _, err := database.Exec(db.Rebind(dialect, `UPDATE _kora_analytics_rebuild_lease
		SET lease_until = datetime(CURRENT_TIMESTAMP, '-1 second') WHERE site = ? AND job_id = ?`), job.Site, job.ID); err != nil {
		t.Fatal("expire first LibSQL runner lease:", err)
	}
	second, err := ClaimNextRebuildJob(context.Background(), database, dialect, job.Site, staleOwner)
	if err != nil || second == nil || second.FencingToken != first.FencingToken+1 {
		t.Fatalf("LibSQL takeover claim = (%#v, %v), want next fencing token", second, err)
	}
	if err := finishClaimedRebuild(context.Background(), database, dialect, first, "completed", 1, ""); err != ErrRebuildLeaseLost {
		t.Fatalf("stale LibSQL runner completion error = %v, want ErrRebuildLeaseLost", err)
	}
	if err := finishClaimedRebuild(context.Background(), database, dialect, second, "completed", 3, ""); err != nil {
		t.Fatal("current LibSQL runner completion:", err)
	}
	persisted, err := GetRebuildJob(database, dialect, job.Site, job.ID)
	if err != nil {
		t.Fatal("read completed LibSQL rebuild:", err)
	}
	if persisted.Status != "completed" || persisted.Metrics != 3 {
		t.Fatalf("completed LibSQL job = %#v, want completed metrics 3", persisted)
	}
}
