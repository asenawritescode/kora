//go:build integration

package configstore_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func TestLiveManagementActivationFenceAcrossDialects(t *testing.T) {
	tests := []struct {
		name, env, driver string
		dialect           func() db.Dialect
	}{
		{"mysql", "KORA_CONFIG_DISPOSABLE_MYSQL_DSN", "mysql", func() db.Dialect { return db.Resolve("mysql") }},
		{"postgres", "KORA_API_LIVE_POSTGRES_DSN", "postgres", func() db.Dialect { return db.Resolve("postgres") }},
		{"libsql", "KORA_CONFIG_DISPOSABLE_LIBSQL_DSN", "libsql", func() db.Dialect { return db.Resolve("libsql") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(test.env))
			if dsn == "" {
				t.Skipf("set %s to a disposable loopback SQL service", test.env)
			}
			database, cleanup := openFenceTestDatabase(t, test.driver, dsn)
			defer cleanup()
			database.SetMaxOpenConns(1)
			if err := site.BootstrapSystemTables(context.Background(), database, test.dialect()); err != nil {
				t.Fatal("bootstrap disposable tenant system tables:", err)
			}
			digest := strings.Repeat("a", 64)
			record := func(fence, revision uint64, value string) error {
				return configstore.RecordManagementActivationFence(context.Background(), database, test.dialect(), "tenant-fence.example.test", "operation-fence-1", fence, revision, value)
			}
			if err := record(4, 1, digest); err != nil {
				t.Fatal("record initial claim:", err)
			}
			validate := func(fence, revision uint64, value string) error {
				tx, err := database.BeginTx(context.Background(), nil)
				if err != nil {
					return err
				}
				defer tx.Rollback()
				return configstore.ValidateManagementActivationFenceTx(context.Background(), tx, test.dialect(), "tenant-fence.example.test", "operation-fence-1", fence, revision, value)
			}
			if err := validate(4, 1, digest); err != nil {
				t.Fatal("validate current claim:", err)
			}
			if err := record(5, 1, digest); err != nil {
				t.Fatal("record takeover claim:", err)
			}
			if err := validate(4, 1, digest); err != configstore.ErrManagementActivationFence {
				t.Fatalf("old fence validation = %v, want stale fence", err)
			}
			if err := validate(5, 1, digest); err != nil {
				t.Fatal("validate takeover claim:", err)
			}
			if err := record(5, 1, digest); err != nil {
				t.Fatalf("idempotent same-claim replay: %v", err)
			}
			if err := record(6, 2, digest); err != configstore.ErrManagementActivationFence {
				t.Fatalf("changed expected revision = %v, want immutable identity conflict", err)
			}
			if err := record(6, 1, strings.Repeat("b", 64)); err != configstore.ErrManagementActivationFence {
				t.Fatalf("changed artifact digest = %v, want immutable identity conflict", err)
			}

			// The accepted operation's immutable snapshot and progress phase are
			// coupled to the config transaction; retries cannot substitute content.
			snapshot := &doctype.ConfigSnapshot{}
			if _, err := configstore.EnsureActivationOperation(database, test.dialect(), "tenant-fence.example.test", "operation-fence-1", "management_package", snapshot); err != nil {
				t.Fatal("persist accepted package snapshot:", err)
			}
			if err := configstore.UpdateActivationOperation(database, test.dialect(), "tenant-fence.example.test", "operation-fence-1", "management_package", configstore.ActivationValidated, "[]", "replan-from-live-schema", ""); err != nil {
				t.Fatal("mark package DDL validated:", err)
			}

			// A stale worker's transaction must fail before config revision or
			// journal phase can change; the current claim commits both atomically.
			store := configstore.NewStore(database, test.dialect())
			commit := configstore.ManagementActivationCommit{
				OperationID: "operation-fence-1", JournalKind: "management_package",
				FromPhase: configstore.ActivationValidated, RecoveryPlan: "replan-from-live-schema",
				CreatedBy: "management", VersionLabel: "Apply package retail@1.0.0",
				Fence: 5, ExpectedRevision: 1, ArtifactDigest: digest,
			}
			staleTx, err := database.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal("begin stale activation:", err)
			}
			staleCommit := commit
			staleCommit.Fence = 4
			if _, _, err := store.CommitManagementActivation(context.Background(), staleTx, snapshot, "tenant-fence.example.test", test.dialect(), staleCommit); err != configstore.ErrManagementActivationFence {
				_ = staleTx.Rollback()
				t.Fatalf("stale activation = %v, want fence conflict", err)
			}
			if err := staleTx.Rollback(); err != nil {
				t.Fatal("rollback stale activation:", err)
			}
			if revision, err := configstore.CurrentConfigRevision(context.Background(), database, test.dialect()); err != nil || revision != 1 {
				t.Fatalf("revision after stale activation = %d, %v; want 1", revision, err)
			}
			currentTx, err := database.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal("begin current activation:", err)
			}
			versionID, version, err := store.CommitManagementActivation(context.Background(), currentTx, snapshot, "tenant-fence.example.test", test.dialect(), commit)
			if err != nil {
				_ = currentTx.Rollback()
				t.Fatal("current activation:", err)
			}
			if versionID == "" || version != 1 {
				_ = currentTx.Rollback()
				t.Fatalf("committed config version = %q/%d, want nonempty ID/version 1", versionID, version)
			}
			if err := currentTx.Commit(); err != nil {
				t.Fatal("commit current activation:", err)
			}
			if err := configstore.CompleteManagementActivation(context.Background(), database, test.dialect(), "tenant-fence.example.test", "operation-fence-1", 4, 1, digest); err != configstore.ErrManagementActivationFence {
				t.Fatalf("stale worker completed runtime phase = %v, want fence conflict", err)
			}
			if err := configstore.CompleteManagementActivation(context.Background(), database, test.dialect(), "tenant-fence.example.test", "operation-fence-1", 5, 1, digest); err != nil {
				t.Fatal("complete current runtime generation:", err)
			}
			if err := configstore.CompleteManagementActivation(context.Background(), database, test.dialect(), "tenant-fence.example.test", "operation-fence-1", 5, 1, digest); err != nil {
				t.Fatal("idempotently complete current runtime generation:", err)
			}
			if revision, err := configstore.CurrentConfigRevision(context.Background(), database, test.dialect()); err != nil || revision != 2 {
				t.Fatalf("revision after current activation = %d, %v; want 2", revision, err)
			}
			journal, err := configstore.LoadActivationOperation(database, test.dialect(), "tenant-fence.example.test", "operation-fence-1", "management_package")
			if err != nil {
				t.Fatal("load committed activation journal:", err)
			}
			if journal.Phase != configstore.ActivationCompleted {
				t.Fatalf("activation journal phase = %q, want %q", journal.Phase, configstore.ActivationCompleted)
			}
			var activeVersions int
			if err := database.QueryRow(db.Rebind(test.dialect(), `SELECT COUNT(*) FROM _kora_config_version WHERE site = ? AND status = 'Active'`), "tenant-fence.example.test").Scan(&activeVersions); err != nil {
				t.Fatal("count active config versions:", err)
			}
			if activeVersions != 1 {
				t.Fatalf("active config version count = %d, want 1", activeVersions)
			}
		})
	}
}

func openFenceTestDatabase(t *testing.T, driver, dsn string) (*sql.DB, func()) {
	t.Helper()
	switch driver {
	case "mysql":
		config, err := mysql.ParseDSN(dsn)
		if err != nil {
			t.Fatal("parse disposable MySQL DSN:", err)
		}
		if !strings.Contains(config.Addr, "127.0.0.1:") && !strings.Contains(config.Addr, "localhost:") {
			t.Fatal("refusing non-loopback MySQL DSN")
		}
		config.DBName = ""
		root, err := sql.Open(driver, config.FormatDSN())
		if err != nil {
			t.Fatal("open disposable MySQL service:", err)
		}
		name := fmt.Sprintf("kora_fence_%d", time.Now().UnixNano()%1_000_000_000)
		if _, err := root.Exec("CREATE DATABASE " + name + " CHARACTER SET utf8mb4"); err != nil {
			root.Close()
			t.Fatal("create isolated MySQL database:", err)
		}
		config.DBName = name
		database, err := sql.Open(driver, config.FormatDSN())
		if err != nil {
			_, _ = root.Exec("DROP DATABASE " + name)
			root.Close()
			t.Fatal("open isolated MySQL database:", err)
		}
		return database, func() { _ = database.Close(); _, _ = root.Exec("DROP DATABASE IF EXISTS " + name); _ = root.Close() }
	case "postgres":
		parsed, err := url.Parse(dsn)
		if err != nil || (parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost") {
			t.Fatal("refusing non-loopback PostgreSQL DSN")
		}
		database, err := sql.Open(driver, dsn)
		if err != nil {
			t.Fatal("open disposable PostgreSQL service:", err)
		}
		name := fmt.Sprintf("kora_fence_%d", time.Now().UnixNano()%1_000_000_000)
		if _, err := database.Exec("CREATE SCHEMA \"" + name + "\""); err != nil {
			database.Close()
			t.Fatal("create isolated PostgreSQL schema:", err)
		}
		database.SetMaxOpenConns(1)
		if _, err := database.Exec("SET search_path TO \"" + name + "\""); err != nil {
			_, _ = database.Exec("DROP SCHEMA \"" + name + "\" CASCADE")
			database.Close()
			t.Fatal("select isolated PostgreSQL schema:", err)
		}
		return database, func() { _, _ = database.Exec("DROP SCHEMA \"" + name + "\" CASCADE"); _ = database.Close() }
	default:
		parsed, err := url.Parse(dsn)
		if err != nil || (parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost") {
			t.Fatal("refusing non-loopback LibSQL DSN")
		}
		database, err := sql.Open(driver, dsn)
		if err != nil {
			t.Fatal("open disposable LibSQL HTTP service:", err)
		}
		var existing int
		if err := database.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = '_kora_management_activation_fence'").Scan(&existing); err != nil {
			database.Close()
			t.Fatal("inspect disposable LibSQL schema:", err)
		}
		if existing != 0 {
			database.Close()
			t.Fatal("refusing LibSQL database with existing management activation state")
		}
		return database, func() { _ = database.Close() }
	}
}
