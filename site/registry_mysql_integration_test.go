package site

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/db"
)

// This live regression test covers MySQL's native upsert path, including the
// per-site revision contract that sqlmock cannot validate.
func TestLiveMySQLSiteRegistryUpsertAdvancesRevisionOnce(t *testing.T) {
	dsn := os.Getenv("KORA_SITE_DIRECTORY_LIVE_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_SITE_DIRECTORY_LIVE_MYSQL_DSN to a disposable MySQL platform registry")
	}
	database, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Ping(); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapPlatformRegistry(database, db.Resolve("mysql")); err != nil {
		t.Fatal("bootstrap platform registry:", err)
	}
	hostname := fmt.Sprintf("revision-test-%d.example.invalid", time.Now().UnixNano())
	registry := NewSQLSiteRegistry(database, "mysql")
	config := &SiteConfig{Hostname: hostname, DomainsList: []string{hostname}, DBType: "mysql", DBHost: "127.0.0.1", DBPort: 3306, DBName: hostname}
	if err := registry.Upsert(config); err != nil {
		t.Fatal("initial upsert:", err)
	}
	defer func() {
		if err := removePlatformSiteRegistration(database, "mysql", hostname); err != nil {
			t.Errorf("remove integration registration: %v", err)
		}
	}()
	first, err := registry.ResolveAlias(hostname)
	if err != nil {
		t.Fatal("resolve first revision:", err)
	}
	if first.ConfigRevision != 1 || first.SiteID == "" {
		t.Fatalf("new site revision = %d, id=%q; want revision 1 and canonical ID", first.ConfigRevision, first.SiteID)
	}
	if err := registry.Upsert(config); err != nil {
		t.Fatal("second upsert:", err)
	}
	second, err := registry.ResolveAlias(hostname)
	if err != nil {
		t.Fatal("resolve second revision:", err)
	}
	if second.SiteID != first.SiteID || second.ConfigRevision != first.ConfigRevision+1 {
		t.Fatalf("upsert identity/revision = %q/%d; want stable ID %q and revision %d", second.SiteID, second.ConfigRevision, first.SiteID, first.ConfigRevision+1)
	}
}

func TestLiveMySQLDirectoryConsumerLeaseFencesDuplicateOwner(t *testing.T) {
	dsn := os.Getenv("KORA_SITE_DIRECTORY_LIVE_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_SITE_DIRECTORY_LIVE_MYSQL_DSN to a disposable MySQL platform registry")
	}
	database, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Ping(); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapPlatformRegistry(database, db.Resolve("mysql")); err != nil {
		t.Fatal("bootstrap platform registry:", err)
	}
	registry := NewSQLSiteRegistry(database, "mysql")
	startCursor, err := registry.CurrentDirectoryRevision()
	if err != nil {
		t.Fatal("read test feed boundary:", err)
	}
	consumerID := fmt.Sprintf("integration:mysql-lease:%d", time.Now().UnixNano())
	oldOwner := &DirectoryConsumer{Registry: registry, ID: consumerID, OwnerID: "mysql-replica-old", InitialCursor: startCursor, Apply: func(context.Context, SiteDirectoryChange) error { return nil }}
	newOwner := &DirectoryConsumer{Registry: registry, ID: consumerID, OwnerID: "mysql-replica-new", InitialCursor: startCursor, Apply: func(context.Context, SiteDirectoryChange) error { return nil }}
	if count, err := oldOwner.ProcessBatch(context.Background()); err != nil || count != 0 {
		t.Fatalf("initial MySQL owner claim = %d, %v", count, err)
	}
	if _, err := newOwner.ProcessBatch(context.Background()); !errors.Is(err, ErrDirectoryConsumerLeaseHeld) {
		t.Fatalf("duplicate MySQL owner = %v, want lease-held", err)
	}
	if _, err := database.Exec(`UPDATE _kora_site_directory_consumers SET lease_expires_at = TIMESTAMPADD(SECOND, -1, CURRENT_TIMESTAMP(6)) WHERE consumer_id = ?`, consumerID); err != nil {
		t.Fatal("expire test lease:", err)
	}
	if count, err := newOwner.ProcessBatch(context.Background()); err != nil || count != 0 {
		t.Fatalf("new MySQL owner claim after expiry = %d, %v", count, err)
	}
	if err := registry.AdvanceConsumerCursor(consumerID, "mysql-replica-old", startCursor, startCursor+1); !errors.Is(err, ErrDirectoryConsumerLeaseLost) {
		t.Fatalf("stale MySQL cursor write = %v, want lease-lost", err)
	}
	if _, err := oldOwner.ProcessBatch(context.Background()); !errors.Is(err, ErrDirectoryConsumerLeaseLost) {
		t.Fatalf("old MySQL owner after takeover = %v, want lease-lost", err)
	}
	newOwner.releaseLease()
}

func TestLiveMySQLDirectoryConsumerLagReadOnly(t *testing.T) {
	dsn := os.Getenv("KORA_SITE_DIRECTORY_LIVE_MYSQL_DSN")
	consumerID := os.Getenv("KORA_SITE_DIRECTORY_LIVE_CONSUMER_ID")
	if dsn == "" || consumerID == "" {
		t.Skip("set KORA_SITE_DIRECTORY_LIVE_MYSQL_DSN and KORA_SITE_DIRECTORY_LIVE_CONSUMER_ID for read-only lag verification")
	}
	database, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Ping(); err != nil {
		t.Fatal("ping live platform registry:", err)
	}
	status, err := NewSQLSiteRegistry(database, "mysql").ConsumerLag(consumerID)
	if err != nil {
		t.Fatal("read live consumer lag:", err)
	}
	t.Logf("consumer cursor=%d latest=%d revision_lag=%d oldest_change_age=%s", status.ConsumerRevision, status.LatestRevision, status.RevisionLag, status.OldestChangeAge.Round(time.Millisecond))
}
