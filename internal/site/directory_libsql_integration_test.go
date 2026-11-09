package site

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	kdb "github.com/asenawritescode/kora/internal/db"
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
	var applied []DirectoryChange
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
		currentConsumer = &DirectoryConsumer{Registry: registry, ID: consumerID, InitialCursor: startCursor, Apply: func(_ context.Context, change DirectoryChange) error {
			applied = append(applied, change)
			return nil
		}}
		return currentConsumer
	}
	if count, err := newConsumer().ProcessBatch(context.Background()); err != nil || count != 0 {
		t.Fatalf("initialize consumer from snapshot = %d, %v", count, err)
	}
	if err := registry.Upsert(&Config{Hostname: hostname, DomainsList: []string{hostname, "alias." + hostname}, DBType: "mysql", DBHost: "127.0.0.1", DBPort: 3306, DBName: "unused", DBUser: "unused"}); err != nil {
		t.Fatal("upsert:", err)
	}
	info, err := registry.ResolveAlias("alias." + hostname)
	if err != nil {
		t.Fatal("resolve alias:", err)
	}
	if count, err := newConsumer().ProcessBatch(context.Background()); err != nil || count != 1 {
		t.Fatalf("initial consumer batch = %d, %v", count, err)
	}
	suspended, err := registry.SetStatusAtRevision(context.Background(), info.SiteID, "suspended", info.ConfigRevision)
	if err != nil || suspended.Status != "suspended" || suspended.ConfigRevision != info.ConfigRevision+1 {
		t.Fatalf("revision-fenced suspend = %+v, %v", suspended, err)
	}
	replayed, err := registry.SetStatusAtRevision(context.Background(), info.SiteID, "suspended", info.ConfigRevision)
	if err != nil || replayed.ConfigRevision != suspended.ConfigRevision {
		t.Fatalf("idempotent suspend replay = %+v, %v", replayed, err)
	}
	if _, err := registry.SetStatusAtRevision(context.Background(), info.SiteID, "active", info.ConfigRevision); !errors.Is(err, ErrSiteRevisionConflict) {
		t.Fatalf("stale resume = %v, want revision conflict", err)
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
		Apply:  func(context.Context, DirectoryChange) error { return nil },
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

func TestLiveLibSQLProvisioningRegistrationBackfillsOnlyUnassignedRows(t *testing.T) {
	dsn := os.Getenv("KORA_SITE_DIRECTORY_LIVE_LIBSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_SITE_DIRECTORY_LIVE_LIBSQL_DSN to a disposable LibSQL database")
	}
	database, err := sql.Open("libsql", dsn)
	if err != nil {
		t.Fatal("open disposable LibSQL database:", err)
	}
	defer database.Close()
	if err := database.PingContext(t.Context()); err != nil {
		t.Fatal("ping disposable LibSQL database:", err)
	}
	if err := BootstrapPlatformRegistryContext(t.Context(), database, kdb.Resolve("libsql")); err != nil {
		t.Fatal("bootstrap LibSQL platform registry:", err)
	}

	stamp := time.Now().UnixNano()
	hostname := fmt.Sprintf("provisioning-assignment-%d.example.invalid", stamp)
	tenantID := fmt.Sprintf("tenant-provisioning-assignment-%d", stamp)
	t.Cleanup(func() { _ = removePlatformSiteRegistration(database, "libsql", hostname) })
	config := &Config{Hostname: hostname, DBType: "mysql", DBHost: "127.0.0.1", DBPort: 3306, DBName: "tenant_db", DomainsList: []string{hostname}}
	register := func(status, assignment string) error {
		return ensurePlatformSiteRegistrationAssignmentContext(t.Context(), database, "libsql", config, status, assignment, tenantID)
	}
	if err := register("provisioning", ""); err != nil {
		t.Fatal("create legacy provisioning row:", err)
	}
	if err := register("provisioning", "deployment-recovery"); err != nil {
		t.Fatal("recover provisioning assignment:", err)
	}
	descriptor, err := NewSQLSiteRegistry(database, "libsql").GetDescriptorByIDContext(t.Context(), tenantID)
	if err != nil || descriptor.RuntimeCellID != "deployment-recovery" {
		t.Fatalf("recovered assignment = %q, %v; want deployment-recovery", descriptor.RuntimeCellID, err)
	}
	if err := register("active", "deployment-other"); err != nil {
		t.Fatal("activate existing assignment row:", err)
	}
	descriptor, err = NewSQLSiteRegistry(database, "libsql").GetDescriptorByIDContext(t.Context(), tenantID)
	if err != nil || descriptor.RuntimeCellID != "deployment-recovery" || descriptor.Status != "active" {
		t.Fatalf("active assignment changed = %q status=%q err=%v; want deployment-recovery/active", descriptor.RuntimeCellID, descriptor.Status, err)
	}
}
