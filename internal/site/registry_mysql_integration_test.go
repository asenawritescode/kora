package site

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"

	"github.com/asenawritescode/kora/internal/db"
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
	config := &Config{Hostname: hostname, DomainsList: []string{hostname}, DBType: "mysql", DBHost: "127.0.0.1", DBPort: 3306, DBName: hostname}
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

func TestLiveMySQLAliasOwnershipConflictRollsBackRegistryUpdate(t *testing.T) {
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
	stamp := time.Now().UnixNano()
	firstName := fmt.Sprintf("alias-owner-%d.example.invalid", stamp)
	secondName := fmt.Sprintf("alias-conflict-%d.example.invalid", stamp)
	sharedAlias := fmt.Sprintf("shared-%d.example.invalid", stamp)
	registry := NewSQLSiteRegistry(database, "mysql")
	first := &Config{Hostname: firstName, DomainsList: []string{sharedAlias}, DBType: "mysql", DBHost: "127.0.0.1", DBPort: 3306, DBName: firstName}
	second := &Config{Hostname: secondName, DomainsList: []string{sharedAlias}, DBType: "mysql", DBHost: "127.0.0.1", DBPort: 3306, DBName: secondName}
	if err := registry.Upsert(first); err != nil {
		t.Fatal("register first alias owner:", err)
	}
	defer func() {
		if err := removePlatformSiteRegistration(database, "mysql", firstName); err != nil {
			t.Errorf("remove first disposable registration: %v", err)
		}
		if err := removePlatformSiteRegistration(database, "mysql", secondName); err != nil {
			t.Errorf("remove second disposable registration: %v", err)
		}
	}()
	if err := registry.Upsert(second); err == nil {
		t.Fatal("second tenant acquired an alias already owned by another tenant")
	}
	if got, err := registry.ResolveAlias(sharedAlias); err != nil || got.Name != firstName {
		t.Fatalf("shared alias resolves to %+v, %v; want first owner %q", got, err, firstName)
	}
	var secondRows int
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_site_registry WHERE site = ?`, secondName).Scan(&secondRows); err != nil {
		t.Fatal("check rolled back registry row:", err)
	}
	if secondRows != 0 {
		t.Fatalf("conflicting registry update left %d row(s), want rollback", secondRows)
	}
}

func TestLiveMySQLSiteStateTransitionIsRevisionFencedAndIdempotent(t *testing.T) {
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
	hostname := fmt.Sprintf("state-test-%d.example.invalid", time.Now().UnixNano())
	registry := NewSQLSiteRegistry(database, "mysql")
	config := &Config{Hostname: hostname, DomainsList: []string{hostname}, DBType: "mysql", DBHost: "127.0.0.1", DBPort: 3306, DBName: hostname, DBUser: "unused"}
	if err := registry.Upsert(config); err != nil {
		t.Fatal("create disposable directory record:", err)
	}
	defer func() {
		if err := removePlatformSiteRegistration(database, "mysql", hostname); err != nil {
			t.Errorf("remove integration registration: %v", err)
		}
	}()
	info, err := registry.ResolveAlias(hostname)
	if err != nil {
		t.Fatal("resolve created tenant:", err)
	}
	directoryRevision, err := registry.CurrentDirectoryRevision()
	if err != nil {
		t.Fatal("read directory revision before state change:", err)
	}
	suspended, err := registry.SetStatusAtRevision(context.Background(), info.SiteID, "suspended", info.ConfigRevision)
	if err != nil || suspended.Status != "suspended" || suspended.ConfigRevision != info.ConfigRevision+1 {
		t.Fatalf("revision-fenced suspend = %+v, %v", suspended, err)
	}
	committedRevision, err := registry.CurrentDirectoryRevision()
	if err != nil || committedRevision != directoryRevision+1 {
		t.Fatalf("directory revision after suspend = %d, %v; want %d", committedRevision, err, directoryRevision+1)
	}
	replayed, err := registry.SetStatusAtRevision(context.Background(), info.SiteID, "suspended", info.ConfigRevision)
	if err != nil || replayed.ConfigRevision != suspended.ConfigRevision {
		t.Fatalf("idempotent suspend replay = %+v, %v", replayed, err)
	}
	if got, err := registry.CurrentDirectoryRevision(); err != nil || got != committedRevision {
		t.Fatalf("directory revision after replay = %d, %v; want unchanged %d", got, err, committedRevision)
	}
	if _, err := registry.SetStatusAtRevision(context.Background(), info.SiteID, "active", info.ConfigRevision); !errors.Is(err, ErrSiteRevisionConflict) {
		t.Fatalf("stale resume = %v, want revision conflict", err)
	}
	if got, err := registry.CurrentDirectoryRevision(); err != nil || got != committedRevision {
		t.Fatalf("directory revision after stale command = %d, %v; want unchanged %d", got, err, committedRevision)
	}
}

func TestLiveMySQLDeletionFenceBindsCheckpointToCurrentAllocation(t *testing.T) {
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
	var registered []string
	defer func() {
		for _, hostname := range registered {
			if err := removePlatformSiteRegistration(database, "mysql", hostname); err != nil {
				t.Errorf("remove integration registration for %s: %v", hostname, err)
			}
		}
	}()
	newTenant := func(suffix string) (string, *Config, DBSiteInfo) {
		t.Helper()
		hostname := fmt.Sprintf("delete-fence-%s-%d.example.invalid", suffix, time.Now().UnixNano())
		config := &Config{Hostname: hostname, DomainsList: []string{hostname}, DBType: "mysql", DBHost: "127.0.0.1", DBPort: 3306, DBName: "db_" + suffix + fmt.Sprint(time.Now().UnixNano()), DBUser: "unused"}
		if err := registry.Upsert(config); err != nil {
			t.Fatal("create disposable directory record:", err)
		}
		registered = append(registered, hostname)
		info, err := registry.ResolveAlias(hostname)
		if err != nil {
			t.Fatal("resolve disposable tenant:", err)
		}
		return hostname, config, info
	}

	t.Run("allocation changes after checkpoint inspection", func(t *testing.T) {
		_, config, inspected := newTenant("changed")
		oldAllocation := DataPlaneReference(inspected)
		changed := *config
		changed.DBName += "_new"
		if err := registry.Upsert(&changed); err != nil {
			t.Fatal("replace allocation before delete fence:", err)
		}
		if _, err := registry.SetDeletingAtAllocation(context.Background(), inspected.SiteID, inspected.ConfigRevision, oldAllocation); !errors.Is(err, ErrSiteAllocationConflict) {
			t.Fatalf("SetDeletingAtAllocation() error = %v, want stale-allocation conflict", err)
		}
		current, err := registry.GetByID(inspected.SiteID)
		if err != nil || current.Status != "active" {
			t.Fatalf("tenant state after stale checkpoint = %q, %v; want active", current.Status, err)
		}
	})

	t.Run("deleting state prevents allocation replacement", func(t *testing.T) {
		hostname, config, inspected := newTenant("fenced")
		allocation := DataPlaneReference(inspected)
		deleting, err := registry.SetDeletingAtAllocation(context.Background(), inspected.SiteID, inspected.ConfigRevision, allocation)
		if err != nil || deleting.Status != "deleting" || deleting.DataPlaneRef != allocation {
			t.Fatalf("SetDeletingAtAllocation() = %+v, %v", deleting, err)
		}
		changed := *config
		changed.DBName += "_new"
		if err := registry.Upsert(&changed); err == nil {
			t.Fatal("tenant allocation changed while deletion was fenced")
		}
		current, err := registry.ResolveAlias(hostname)
		if err != nil || current.Status != "deleting" || DataPlaneReference(current) != allocation {
			t.Fatalf("directory after attempted replacement = %+v, %v", current, err)
		}
	})
}

func TestLiveMySQLCheckpointBoundDeletionDropsOnlyTheVerifiedAllocation(t *testing.T) {
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
	hostname := fmt.Sprintf("verified-delete-%d.example.invalid", time.Now().UnixNano())
	databaseName := fmt.Sprintf("kora_del_%x", time.Now().UnixNano())
	if _, err := database.Exec("CREATE DATABASE " + db.Resolve("mysql").QuoteIdent(databaseName)); err != nil {
		t.Fatal("create disposable tenant database:", err)
	}
	defer func() {
		if _, err := database.Exec("DROP DATABASE IF EXISTS " + db.Resolve("mysql").QuoteIdent(databaseName)); err != nil {
			t.Errorf("drop leftover disposable tenant database: %v", err)
		}
		if err := removePlatformSiteRegistration(database, "mysql", hostname); err != nil {
			t.Errorf("remove leftover disposable site registration: %v", err)
		}
	}()
	registry := NewSQLSiteRegistry(database, "mysql")
	// The tenant database lives on the same server the DSN reaches.
	server, err := mysqldriver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse disposable MySQL DSN:", err)
	}
	serverHost, serverPortText, err := net.SplitHostPort(server.Addr)
	if err != nil {
		t.Fatal("split disposable MySQL address:", err)
	}
	serverPort, err := strconv.Atoi(serverPortText)
	if err != nil {
		t.Fatal("parse disposable MySQL port:", err)
	}
	config := &Config{Hostname: hostname, DomainsList: []string{hostname}, DBType: "mysql", DBHost: serverHost, DBPort: serverPort, DBName: databaseName, DBUser: server.User, DBPassword: server.Passwd, FileStorage: "local"}
	if err := registry.Upsert(config); err != nil {
		t.Fatal("create disposable site directory record:", err)
	}
	info, err := registry.ResolveAlias(hostname)
	if err != nil {
		t.Fatal("resolve disposable site:", err)
	}
	common := &CommonConfig{DBType: "mysql", DBHost: serverHost, DBPort: serverPort, DBUser: server.User, DBPassword: server.Passwd}
	if err := DeleteRegisteredSiteWithAllocation(context.Background(), database, "mysql", info.SiteID, hostname, DataPlaneReference(info), common); err != nil {
		t.Fatalf("checkpoint-bound deletion: %v", err)
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name = ?`, databaseName).Scan(&count); err != nil {
		t.Fatal("verify tenant schema removal:", err)
	}
	if count != 0 {
		t.Fatalf("tenant schema remains after checkpoint-bound deletion: %s", databaseName)
	}
	if _, err := registry.GetByID(info.SiteID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("tenant directory lookup after deletion = %v, want no row", err)
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
	oldOwner := &DirectoryConsumer{Registry: registry, ID: consumerID, OwnerID: "mysql-replica-old", InitialCursor: startCursor, Apply: func(context.Context, DirectoryChange) error { return nil }}
	newOwner := &DirectoryConsumer{Registry: registry, ID: consumerID, OwnerID: "mysql-replica-new", InitialCursor: startCursor, Apply: func(context.Context, DirectoryChange) error { return nil }}
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
