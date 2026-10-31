//go:build integration

package api

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	kdb "github.com/asenawritescode/kora/db"
	"github.com/go-sql-driver/mysql"
)

func TestMySQLUserDirectoryPrefixSearchUsesIndexes(t *testing.T) {
	dsn := os.Getenv("KORA_TEST_DSN")
	if dsn == "" {
		t.Skip("set KORA_TEST_DSN to a disposable MySQL service")
	}
	rootConfig, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse disposable MySQL DSN:", err)
	}
	rootConfig.DBName = ""
	root, err := sql.Open("mysql", rootConfig.FormatDSN())
	if err != nil {
		t.Fatal("open disposable MySQL service:", err)
	}
	if err := root.PingContext(t.Context()); err != nil {
		_ = root.Close()
		t.Skipf("disposable MySQL unavailable: %v", err)
	}
	name := fmt.Sprintf("kora_user_picker_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := root.ExecContext(t.Context(), "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = root.Close()
		t.Fatal("create isolated user directory database:", err)
	}
	t.Cleanup(func() {
		_, _ = root.ExecContext(context.Background(), "DROP DATABASE IF EXISTS `"+name+"`")
		_ = root.Close()
	})
	tenantConfig := *rootConfig
	tenantConfig.DBName = name
	database, err := sql.Open("mysql", tenantConfig.FormatDSN())
	if err != nil {
		t.Fatal("open isolated user directory database:", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	database.SetMaxOpenConns(2)
	if _, err := database.ExecContext(t.Context(), `CREATE TABLE _kora_user (
		name VARCHAR(140) PRIMARY KEY,
		site VARCHAR(140) NOT NULL,
		email VARCHAR(255) NOT NULL,
		full_name VARCHAR(255) NOT NULL,
		enabled TINYINT(1) NOT NULL DEFAULT 1
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`); err != nil {
		t.Fatal("create isolated user table:", err)
	}
	for _, statement := range (&kdb.MySQLDialect{}).SystemTableSQL() {
		if !strings.Contains(statement, "idx_user_picker_") {
			continue
		}
		if _, err := database.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("create user picker index: %v\n%s", err, statement)
		}
	}

	insert, err := database.PrepareContext(t.Context(), "INSERT INTO _kora_user (name, site, email, full_name, enabled) VALUES (?, ?, ?, ?, ?)")
	if err != nil {
		t.Fatal("prepare user fixture insert:", err)
	}
	defer insert.Close()
	for i := 0; i < 5000; i++ {
		fullName := fmt.Sprintf("Person %05d", i)
		email := fmt.Sprintf("person-%05d@example.test", i)
		if i%100 == 0 {
			fullName = fmt.Sprintf("Alice Example %05d", i)
			email = fmt.Sprintf("alice-%05d@example.test", i)
		}
		if _, err := insert.ExecContext(t.Context(), fmt.Sprintf("user-%05d", i), "tenant-a", email, fullName, true); err != nil {
			t.Fatalf("insert user fixture %d: %v", i, err)
		}
	}
	for _, user := range []struct {
		name, site, email, fullName string
		enabled                     bool
	}{
		{"tenant-b-user", "tenant-b", "alice-tenant-b@example.test", "Alice AAA Tenant B", true},
		{"disabled-user", "tenant-a", "alice-disabled@example.test", "Alice AAB Disabled", false},
	} {
		if _, err := insert.ExecContext(t.Context(), user.name, user.site, user.email, user.fullName, user.enabled); err != nil {
			t.Fatalf("insert MySQL tenant and disabled-user control %q: %v", user.name, err)
		}
	}

	for _, field := range []string{"full_name", "email"} {
		t.Run(field, func(t *testing.T) {
			query := "EXPLAIN SELECT name FROM _kora_user WHERE site = ? AND enabled = ? AND " + field + " LIKE ? ESCAPE '!' LIMIT ?"
			var selectType, table, accessType string
			var partitions, possibleKeys, key, keyLength, ref, extra sql.NullString
			var id, rowsEstimate int64
			var filtered sql.NullFloat64
			if err := database.QueryRowContext(t.Context(), query, "tenant-a", true, "alice%", 25).
				Scan(&id, &selectType, &table, &partitions, &accessType, &possibleKeys, &key, &keyLength, &ref, &rowsEstimate, &filtered, &extra); err != nil {
				t.Fatalf("explain indexed prefix search: %v", err)
			}
			if !key.Valid || !strings.Contains(key.String, "idx_user_picker_"+map[string]string{"full_name": "name", "email": "email"}[field]) {
				t.Fatalf("field %s used key %q (possible keys %q, estimated rows %d), want user picker index", field, key.String, possibleKeys.String, rowsEstimate)
			}
		})
	}

	// Verify the complete picker predicate used by HandleUserDirectory, not
	// only each individual searchable field. The OR and ordering clauses can
	// change the optimizer's plan even when each field is indexed in isolation.
	var combinedPlan string
	combinedQuery := `EXPLAIN FORMAT=JSON SELECT name, email, full_name FROM _kora_user
		WHERE site = ? AND enabled = ?
		AND (full_name LIKE ? ESCAPE '!' OR email LIKE ? ESCAPE '!')
		ORDER BY full_name, name LIMIT ?`
	if err := database.QueryRowContext(t.Context(), combinedQuery, "tenant-a", true, "alice%", "alice%", 25).Scan(&combinedPlan); err != nil {
		t.Fatal("explain complete user-directory prefix query:", err)
	}
	if !strings.Contains(combinedPlan, "idx_user_picker_name") && !strings.Contains(combinedPlan, "idx_user_picker_email") {
		t.Fatalf("complete user-directory query did not use a picker index: %s", combinedPlan)
	}
	results, err := database.QueryContext(t.Context(), `SELECT name, email, full_name FROM _kora_user
		WHERE site = ? AND enabled = ?
		AND (full_name LIKE ? ESCAPE '!' OR email LIKE ? ESCAPE '!')
		ORDER BY full_name, name LIMIT ?`, "tenant-a", true, "alice%", "alice%", 25)
	if err != nil {
		t.Fatal("execute complete MySQL user-directory query:", err)
	}
	assertUserDirectoryPickerSearchResults(t, results, 25)
}
