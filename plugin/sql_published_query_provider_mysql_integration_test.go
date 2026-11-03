//go:build integration

package plugin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/go-sql-driver/mysql"
)

func TestSQLPublishedQueryProviderMySQLFreshDatabaseRestart(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KORA_PUBLISHED_QUERY_DISPOSABLE_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("set KORA_PUBLISHED_QUERY_DISPOSABLE_MYSQL_DSN to a disposable loopback MySQL service")
	}
	rootConfig, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if rootConfig.Net != "tcp" {
		t.Fatal("disposable published-query test requires loopback TCP MySQL")
	}
	host, _, err := net.SplitHostPort(rootConfig.Addr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(host, "localhost") {
		address, parseErr := netip.ParseAddr(strings.Trim(host, "[]"))
		if parseErr != nil || !address.IsLoopback() {
			t.Fatal("refusing published-query integration test against non-loopback MySQL")
		}
	}
	root, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Ping(); err != nil {
		_ = root.Close()
		t.Fatalf("disposable MySQL unavailable: %v", err)
	}
	name := fmt.Sprintf("kora_published_query_%d", time.Now().UnixNano())
	if _, err := root.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = root.Exec("DROP DATABASE IF EXISTS `" + name + "`")
		_ = root.Close()
	})
	databaseConfig := *rootConfig
	databaseConfig.DBName = name
	database, err := sql.Open("mysql", databaseConfig.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	ctx := context.Background()
	provider, err := NewSQLPublishedQueryProvider(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	query := contract.PublishedQuery{OwnerModuleID: "sales.orders", Name: "mine", Operation: "list", ScopePolicy: contract.ScopeEntityOwned, AllowedFields: []string{"id", "owner_id"}, OwnerField: "owner_id", PageLimit: 10, OfflineEligible: true, OfflineFactKey: "id"}
	execution, err := contract.NewModuleExecution(scope, query.OwnerModuleID, query.OwnerModuleID, 1, query.Operation)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.PutFactTx(ctx, tx, execution, query, json.RawMessage(`{}`), json.RawMessage(`{"id":"order-1","owner_id":"alice"}`)); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewSQLPublishedQueryProvider(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	actor := PublishedQueryActorScope{BusinessScope: scope, SubjectID: "alice", OwnerOnly: true, OwnerField: query.OwnerField}
	result, err := restarted.ExecutePublishedQueryForActor(ctx, execution, query, actor, json.RawMessage(`{}`), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Data) != `[{"id":"order-1","owner_id":"alice"}]` || result.SnapshotToken == "" {
		t.Fatalf("restarted MySQL snapshot = %+v", result)
	}
}
