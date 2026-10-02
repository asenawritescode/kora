package site

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	kdb "github.com/asenawritescode/kora/db"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

// Run against a disposable local sqld server with
// KORA_SITE_DIRECTORY_LIVE_LIBSQL_DSN=http://127.0.0.1:8080.
func TestLiveLibSQLSiteDirectoryLifecycle(t *testing.T) {
	dsn := os.Getenv("KORA_SITE_DIRECTORY_LIVE_LIBSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_SITE_DIRECTORY_LIVE_LIBSQL_DSN to a disposable LibSQL database")
	}
	database, err := sql.Open("libsql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Ping(); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := BootstrapPlatformRegistry(database, kdb.Resolve("libsql")); err != nil {
			t.Fatalf("bootstrap attempt %d: %v", attempt+1, err)
		}
	}

	hostname := fmt.Sprintf("directory-test-%d.example.invalid", time.Now().UnixNano())
	registry := NewSQLSiteRegistry(database, "libsql")
	startCursor, err := registry.CurrentDirectoryRevision()
	if err != nil {
		t.Fatal("read startup feed boundary:", err)
	}
	consumerID := fmt.Sprintf("integration:libsql-lifecycle:%d", time.Now().UnixNano())
	var applied []SiteDirectoryChange
	var currentConsumer *DirectoryConsumer
	defer func() {
		if currentConsumer != nil {
			currentConsumer.releaseLease()
		}
	}()
	newConsumer := func() *DirectoryConsumer {
		if currentConsumer != nil {
			currentConsumer.releaseLease()
		}
		currentConsumer = &DirectoryConsumer{Registry: registry, ID: consumerID, InitialCursor: startCursor, Apply: func(_ context.Context, change SiteDirectoryChange) error {
			applied = append(applied, change)
			return nil
		}}
		return currentConsumer
	}
	if count, err := newConsumer().ProcessBatch(context.Background()); err != nil || count != 0 {
		t.Fatalf("initialize consumer from snapshot = %d, %v", count, err)
	}
	if err := registry.Upsert(&SiteConfig{Hostname: hostname, DomainsList: []string{hostname, "alias." + hostname}, DBType: "mysql", DBHost: "127.0.0.1", DBPort: 3306, DBName: "unused", DBUser: "unused"}); err != nil {
		t.Fatal("upsert:", err)
	}
	info, err := registry.ResolveAlias("alias." + hostname)
	if err != nil {
		t.Fatal("resolve alias:", err)
	}
	if count, err := newConsumer().ProcessBatch(context.Background()); err != nil || count != 1 {
		t.Fatalf("initial consumer batch = %d, %v", count, err)
	}
	if err := registry.SetStatus(info.SiteID, "suspended"); err != nil {
		t.Fatal("set status:", err)
	}
	if count, err := newConsumer().ProcessBatch(context.Background()); err != nil || count != 1 {
		t.Fatalf("consumer after restart = %d, %v", count, err)
	}
	if err := removePlatformSiteRegistration(database, "libsql", hostname); err != nil {
		t.Fatal("delete site:", err)
	}
	if count, err := newConsumer().ProcessBatch(context.Background()); err != nil || count != 1 {
		t.Fatalf("delete consumer batch = %d, %v", count, err)
	}
	if len(applied) != 3 || applied[1].Descriptor.Status != "suspended" || applied[2].Descriptor.Status != "deleted" {
		t.Fatalf("applied descriptors = %#v", applied)
	}
	pruned, err := registry.PruneOutboxBefore(time.Now().Add(time.Hour))
	if err != nil || pruned == 0 {
		t.Fatalf("prune consumed history = %d, %v", pruned, err)
	}
	resynced := uint64(0)
	stale := &DirectoryConsumer{
		Registry: registry, ID: "integration:stale:" + hostname, InitialCursor: startCursor,
		Apply:  func(context.Context, SiteDirectoryChange) error { return nil },
		Resync: func(_ context.Context, revision uint64) error { resynced = revision; return nil },
	}
	if count, err := stale.ProcessBatch(context.Background()); err != nil || count != 0 {
		t.Fatalf("consumer after history expiration = %d, %v", count, err)
	}
	latest, err := registry.CurrentDirectoryRevision()
	if err != nil || resynced != latest {
		t.Fatalf("snapshot resync revision = %d, latest = %d, err = %v", resynced, latest, err)
	}
}
