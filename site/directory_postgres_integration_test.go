package site

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/db"
	_ "github.com/lib/pq"
)

// Run against a disposable PostgreSQL database with
// KORA_SITE_DIRECTORY_LIVE_POSTGRES_DSN set. This exercises actual dialect
// behavior that sqlmock cannot validate (JSONB, transactions, and cursor CAS).
func TestLivePostgresSiteDirectoryLifecycle(t *testing.T) {
	dsn := os.Getenv("KORA_SITE_DIRECTORY_LIVE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_SITE_DIRECTORY_LIVE_POSTGRES_DSN to a disposable PostgreSQL database")
	}
	database, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Ping(); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := BootstrapPlatformRegistry(database, db.Resolve("postgres")); err != nil {
			t.Fatalf("bootstrap attempt %d: %v", attempt+1, err)
		}
	}

	hostname := fmt.Sprintf("directory-test-%d.example.invalid", time.Now().UnixNano())
	registry := NewSQLSiteRegistry(database, "postgres")
	startCursor, err := registry.CurrentDirectoryRevision()
	if err != nil {
		t.Fatal("read startup feed boundary:", err)
	}
	consumerID := fmt.Sprintf("integration:postgres-lifecycle:%d", time.Now().UnixNano())
	var applied []SiteDirectoryChange
	consumer := &DirectoryConsumer{Registry: registry, ID: consumerID, InitialCursor: startCursor, Apply: func(_ context.Context, change SiteDirectoryChange) error {
		applied = append(applied, change)
		return nil
	}}
	if count, err := consumer.ProcessBatch(context.Background()); err != nil || count != 0 {
		t.Fatalf("initialize consumer from snapshot = %d, %v", count, err)
	}
	// A second Engine replica has an independent stable consumer identity and
	// therefore its own durable cursor. Both replicas must converge from the
	// same snapshot boundary without advancing or masking one another.
	var replicaApplied []SiteDirectoryChange
	replica := &DirectoryConsumer{Registry: registry, ID: consumerID + ":replica-b", InitialCursor: startCursor, Apply: func(_ context.Context, change SiteDirectoryChange) error {
		replicaApplied = append(replicaApplied, change)
		return nil
	}}
	if count, err := replica.ProcessBatch(context.Background()); err != nil || count != 0 {
		t.Fatalf("initialize second replica from snapshot = %d, %v", count, err)
	}
	config := &SiteConfig{Hostname: hostname, DomainsList: []string{hostname, "alias." + hostname}, DBType: "mysql", DBHost: "127.0.0.1", DBPort: 3306, DBName: "unused", DBUser: "unused"}
	if err := registry.Upsert(config); err != nil {
		t.Fatal("upsert:", err)
	}
	info, err := registry.ResolveAlias("alias." + hostname)
	if err != nil {
		t.Fatal("resolve alias:", err)
	}
	if info.SiteID == "" || info.ConfigRevision == 0 {
		t.Fatalf("upsert produced incomplete identity/revision: %#v", info)
	}
	if count, err := consumer.ProcessBatch(context.Background()); err != nil || count != 1 {
		t.Fatalf("initial consumer batch = %d, %v", count, err)
	}
	duplicate := &DirectoryConsumer{Registry: registry, ID: consumerID, OwnerID: "integration:duplicate-process", Apply: func(context.Context, SiteDirectoryChange) error {
		t.Fatal("duplicate consumer identity applied a revision while another owner held the lease")
		return nil
	}}
	if _, err := duplicate.ProcessBatch(context.Background()); !errors.Is(err, ErrDirectoryConsumerLeaseHeld) {
		t.Fatalf("duplicate consumer identity error = %v, want active lease rejection", err)
	}
	if count, err := replica.ProcessBatch(context.Background()); err != nil || count != 1 {
		t.Fatalf("second replica initial batch = %d, %v", count, err)
	}
	if err := registry.SetStatus(info.SiteID, "suspended"); err != nil {
		t.Fatal("set status:", err)
	}
	// A new consumer object with the same identity models a process restart.
	consumer.releaseLease()
	restarted := &DirectoryConsumer{Registry: registry, ID: consumerID, Apply: func(_ context.Context, change SiteDirectoryChange) error {
		applied = append(applied, change)
		return nil
	}}
	if count, err := restarted.ProcessBatch(context.Background()); err != nil || count != 1 {
		t.Fatalf("consumer after restart = %d, %v", count, err)
	}
	if count, err := replica.ProcessBatch(context.Background()); err != nil || count != 1 {
		t.Fatalf("second replica status batch = %d, %v", count, err)
	}
	cursor, err := registry.LoadConsumerCursor(consumerID, startCursor)
	if err != nil || cursor != applied[1].Cursor {
		t.Fatalf("reloaded consumer cursor = %d, want %d: %v", cursor, applied[1].Cursor, err)
	}
	if len(applied) != 2 || applied[1].Descriptor.Status != "suspended" || applied[1].Descriptor.DirectoryRevision != applied[1].Cursor {
		t.Fatalf("applied descriptors = %#v", applied)
	}
	if err := removePlatformSiteRegistration(database, "postgres", hostname); err != nil {
		t.Fatal("delete site:", err)
	}
	if count, err := restarted.ProcessBatch(context.Background()); err != nil || count != 1 {
		t.Fatalf("delete consumer batch = %d, %v", count, err)
	}
	if count, err := replica.ProcessBatch(context.Background()); err != nil || count != 1 {
		t.Fatalf("second replica delete batch = %d, %v", count, err)
	}
	if len(applied) != 3 || applied[2].Descriptor.Status != "deleted" {
		t.Fatalf("delete tombstone = %#v", applied)
	}
	if len(replicaApplied) != 3 || replicaApplied[0].Descriptor.SiteID != info.SiteID || replicaApplied[1].Descriptor.Status != "suspended" || replicaApplied[2].Descriptor.Status != "deleted" {
		t.Fatalf("second replica did not independently converge create/status/delete: %#v", replicaApplied)
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

func TestLivePostgresSiteBootstrapWrites(t *testing.T) {
	dsn := os.Getenv("KORA_SITE_DIRECTORY_LIVE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_SITE_DIRECTORY_LIVE_POSTGRES_DSN to a disposable PostgreSQL database")
	}
	database, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	if err := database.Ping(); err != nil {
		t.Fatal(err)
	}
	schemaName := fmt.Sprintf("kora_site_bootstrap_%d", time.Now().UnixNano())
	if _, err := database.Exec(`CREATE SCHEMA "` + schemaName + `"`); err != nil {
		t.Fatal("create isolated schema:", err)
	}
	defer func() { _, _ = database.Exec(`DROP SCHEMA "` + schemaName + `" CASCADE`) }()
	if _, err := database.Exec(`SET search_path TO "` + schemaName + `"`); err != nil {
		t.Fatal("select isolated schema:", err)
	}
	dialect := db.Resolve("postgres")
	if err := BootstrapSystemTables(database, dialect); err != nil {
		t.Fatal("bootstrap Engine system tables:", err)
	}

	const siteName = "bootstrap-postgres.example.invalid"
	if err := createAdminUser(database, dialect, "owner@example.invalid", "", "Owner", siteName); err != nil {
		t.Fatal("create admin user:", err)
	}
	ensureConfigVersion(database, dialect, siteName, []string{siteName, "pos.example.invalid"})
	ensureConfigVersion(database, dialect, siteName, []string{siteName, "pos.example.invalid"})

	var users, versions, active int
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_user WHERE site = $1`, siteName).Scan(&users); err != nil {
		t.Fatal("count admin users:", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*), COALESCE(SUM(is_active), 0) FROM _kora_config_version WHERE site = $1`, siteName).Scan(&versions, &active); err != nil {
		t.Fatal("read starter config version:", err)
	}
	if users != 1 || versions != 1 || active != 1 {
		t.Fatalf("bootstrap rows: users=%d versions=%d active=%d; want 1/1/1", users, versions, active)
	}
}

func TestLivePostgresCreateSiteRegistersCanonicalDirectoryRecord(t *testing.T) {
	dsn := os.Getenv("KORA_SITE_DIRECTORY_LIVE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_SITE_DIRECTORY_LIVE_POSTGRES_DSN to a disposable PostgreSQL server")
	}
	connectionURL, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("parse PostgreSQL connection URL:", err)
	}
	if connectionURL.Scheme != "postgres" && connectionURL.Scheme != "postgresql" {
		t.Fatal("site creation acceptance requires a PostgreSQL URL DSN so it can derive the disposable server credentials safely")
	}
	port := 5432
	if connectionURL.Port() != "" {
		port, err = strconv.Atoi(connectionURL.Port())
		if err != nil {
			t.Fatal("parse PostgreSQL port:", err)
		}
	}
	user := connectionURL.User.Username()
	password, _ := connectionURL.User.Password()
	platformDB, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("open disposable PostgreSQL platform database:", err)
	}
	defer platformDB.Close()
	if err := platformDB.Ping(); err != nil {
		t.Fatal("ping PostgreSQL platform database:", err)
	}
	if err := BootstrapPlatformRegistry(platformDB, db.Resolve("postgres")); err != nil {
		t.Fatal("bootstrap platform registry:", err)
	}

	stamp := time.Now().UnixNano()
	hostname := fmt.Sprintf("create-site-%d.example.invalid", stamp)
	databaseName := fmt.Sprintf("kora_create_%d", stamp)
	var result *CreateSiteResult
	t.Cleanup(func() {
		if result != nil && result.DB != nil {
			_ = result.DB.Close()
		}
		_ = removePlatformSiteRegistration(platformDB, "postgres", hostname)
		quotedName := `"` + strings.ReplaceAll(databaseName, `"`, `""`) + `"`
		_, _ = platformDB.Exec(`DROP DATABASE IF EXISTS ` + quotedName)
	})
	result, err = CreateSite(CreateSiteInput{
		Hostname: hostname, DBType: "postgres", DBHost: connectionURL.Hostname(), DBPort: port,
		DBName: databaseName, DBUser: user, DBPassword: password,
		AdminEmail: "owner@" + hostname, PlatformDBType: "postgres", PlatformDB: platformDB,
	})
	if err != nil {
		t.Fatal("create PostgreSQL site:", err)
	}
	if result.SiteID == "" || result.Config == nil || result.Config.DBName != databaseName {
		t.Fatalf("created site result = %#v, want canonical ID and database %q", result, databaseName)
	}
	firstSiteID := result.SiteID
	if err := result.DB.Close(); err != nil {
		t.Fatal("close first tenant handle before retry:", err)
	}
	result, err = CreateSite(CreateSiteInput{
		Hostname: hostname, DBType: "postgres", DBHost: connectionURL.Hostname(), DBPort: port,
		DBName: databaseName, DBUser: user, DBPassword: password,
		AdminEmail: "owner@" + hostname, PlatformDBType: "postgres", PlatformDB: platformDB,
	})
	if err != nil {
		t.Fatal("retry PostgreSQL site creation:", err)
	}
	if result.SiteID != firstSiteID {
		t.Fatalf("retry changed canonical site ID from %q to %q", firstSiteID, result.SiteID)
	}
	byAlias, err := ResolveSiteAlias(platformDB, "postgres", hostname)
	if err != nil || byAlias != result.SiteID {
		t.Fatalf("canonical alias resolution = %q, %v; want site ID %q", byAlias, err, result.SiteID)
	}
	var admins, activeVersions int
	if err := result.DB.QueryRow(`SELECT COUNT(*) FROM _kora_user WHERE email = $1`, "owner@"+hostname).Scan(&admins); err != nil {
		t.Fatal("read created site's admin account:", err)
	}
	if err := result.DB.QueryRow(`SELECT COUNT(*) FROM _kora_config_version WHERE site = $1 AND is_active = 1`, hostname).Scan(&activeVersions); err != nil {
		t.Fatal("read created site's active config version:", err)
	}
	if admins != 1 || activeVersions != 1 {
		t.Fatalf("created site rows: admins=%d active_versions=%d; want 1/1", admins, activeVersions)
	}
}

func TestLivePostgresConsumerLeaseFencesExpiredOwner(t *testing.T) {
	dsn := os.Getenv("KORA_SITE_DIRECTORY_LIVE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_SITE_DIRECTORY_LIVE_POSTGRES_DSN to a disposable PostgreSQL database")
	}
	database, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Ping(); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapPlatformRegistry(database, db.Resolve("postgres")); err != nil {
		t.Fatal("bootstrap platform registry:", err)
	}
	id := fmt.Sprintf("integration:lease:%d", time.Now().UnixNano())
	registry := NewSQLSiteRegistry(database, "postgres")
	startCursor, err := registry.CurrentDirectoryRevision()
	if err != nil {
		t.Fatal("read test feed boundary:", err)
	}
	if _, err := registry.LoadConsumerCursor(id, startCursor); err != nil {
		t.Fatal("initialize consumer cursor:", err)
	}
	oldOwner := &DirectoryConsumer{Registry: registry, ID: id, OwnerID: "replica-old", InitialCursor: startCursor, Apply: func(context.Context, SiteDirectoryChange) error {
		t.Fatal("expired owner applied a change after losing its lease")
		return nil
	}}
	newOwner := &DirectoryConsumer{Registry: registry, ID: id, OwnerID: "replica-new", InitialCursor: startCursor, Apply: func(context.Context, SiteDirectoryChange) error { return nil }}
	if count, err := oldOwner.ProcessBatch(context.Background()); err != nil || count != 0 {
		t.Fatalf("initial owner claim = %d, %v", count, err)
	}
	if _, err := newOwner.ProcessBatch(context.Background()); !errors.Is(err, ErrDirectoryConsumerLeaseHeld) {
		t.Fatalf("second owner before expiry = %v, want lease-held", err)
	}
	if _, err := database.Exec(`UPDATE _kora_site_directory_consumers SET lease_expires_at = CURRENT_TIMESTAMP - INTERVAL '1 second' WHERE consumer_id = $1`, id); err != nil {
		t.Fatal("expire test lease:", err)
	}
	if count, err := newOwner.ProcessBatch(context.Background()); err != nil || count != 0 {
		t.Fatalf("new owner claim after expiry = %d, %v", count, err)
	}
	if err := registry.AdvanceConsumerCursor(id, "replica-old", startCursor, startCursor+1); !errors.Is(err, ErrDirectoryConsumerLeaseLost) {
		t.Fatalf("stale fenced cursor write = %v, want lease-lost", err)
	}
	if _, err := oldOwner.ProcessBatch(context.Background()); !errors.Is(err, ErrDirectoryConsumerLeaseLost) {
		t.Fatalf("old owner after takeover = %v, want lease-lost", err)
	}
	newOwner.releaseLease()
}
