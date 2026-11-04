//go:build integration

package api

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	kdb "github.com/asenawritescode/kora/db"
	_ "github.com/lib/pq"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func TestPostgresUserDirectoryPrefixSearchUsesIndexes(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KORA_API_DISPOSABLE_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set KORA_API_DISPOSABLE_POSTGRES_DSN to a disposable loopback PostgreSQL database")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost") {
		t.Fatal("refusing non-loopback PostgreSQL DSN")
	}
	database, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("open disposable PostgreSQL database:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	if err := database.PingContext(t.Context()); err != nil {
		t.Fatal("ping disposable PostgreSQL database:", err)
	}

	schema := fmt.Sprintf("kora_user_picker_%d", time.Now().UnixNano())
	if _, err := database.ExecContext(t.Context(), `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatal("create isolated PostgreSQL schema:", err)
	}
	t.Cleanup(func() { _, _ = database.ExecContext(t.Context(), `DROP SCHEMA "`+schema+`" CASCADE`) })
	if _, err := database.ExecContext(t.Context(), `SET search_path TO "`+schema+`"`); err != nil {
		t.Fatal("select isolated PostgreSQL schema:", err)
	}
	if _, err := database.ExecContext(t.Context(), `CREATE TABLE _kora_user (
		name VARCHAR(140) PRIMARY KEY,
		site VARCHAR(140) NOT NULL,
		email VARCHAR(255) NOT NULL,
		full_name VARCHAR(255) NOT NULL,
		enabled SMALLINT NOT NULL DEFAULT 1
	)`); err != nil {
		t.Fatal("create isolated user directory table:", err)
	}
	for _, statement := range (&kdb.PostgresDialect{}).SystemTableSQL() {
		if !strings.Contains(statement, "idx_user_picker_") {
			continue
		}
		if _, err := database.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("create user picker index: %v\n%s", err, statement)
		}
	}
	if _, err := database.ExecContext(t.Context(), `INSERT INTO _kora_user (name, site, email, full_name, enabled)
		SELECT 'user-' || lpad(i::text, 5, '0'), 'tenant-a',
			CASE WHEN i % 500 = 0 THEN 'alice-' || i || '@example.test' ELSE 'person-' || i || '@example.test' END,
			CASE WHEN i % 500 = 0 THEN 'Alice Example ' || i ELSE 'Person ' || i END,
			1 FROM generate_series(1, 50000) AS i`); err != nil {
		t.Fatal("populate disposable PostgreSQL user directory:", err)
	}
	if _, err := database.ExecContext(t.Context(), `INSERT INTO _kora_user (name, site, email, full_name, enabled) VALUES
		('tenant-b-user', 'tenant-b', 'alice-tenant-b@example.test', 'Alice AAA Tenant B', 1),
		('disabled-user', 'tenant-a', 'alice-disabled@example.test', 'Alice AAB Disabled', 0)`); err != nil {
		t.Fatal("insert PostgreSQL tenant and disabled-user controls:", err)
	}
	if _, err := database.ExecContext(t.Context(), "ANALYZE _kora_user"); err != nil {
		t.Fatal("analyze PostgreSQL user directory:", err)
	}

	query := `EXPLAIN SELECT name, email, full_name FROM _kora_user
		WHERE site = $1 AND enabled = $2
		AND (LOWER(full_name) LIKE $3 ESCAPE '!' OR LOWER(email) LIKE $4 ESCAPE '!')
		ORDER BY full_name, name LIMIT $5`
	planRows, err := database.QueryContext(t.Context(), query, "tenant-a", int64(1), "alice%", "alice%", 25)
	if err != nil {
		t.Fatal("explain complete PostgreSQL user-directory query:", err)
	}
	var plan []string
	for planRows.Next() {
		var detail string
		if err := planRows.Scan(&detail); err != nil {
			planRows.Close()
			t.Fatal("read PostgreSQL query plan:", err)
		}
		plan = append(plan, detail)
	}
	if err := planRows.Err(); err != nil {
		planRows.Close()
		t.Fatal("iterate PostgreSQL query plan:", err)
	}
	if err := planRows.Close(); err != nil {
		t.Fatal("close PostgreSQL query plan:", err)
	}
	joinedPlan := strings.Join(plan, "\n")
	if !strings.Contains(joinedPlan, "idx_user_picker_name") && !strings.Contains(joinedPlan, "idx_user_picker_email") {
		t.Fatalf("complete PostgreSQL user-directory query did not use a picker index:\n%s", joinedPlan)
	}
	results, err := database.QueryContext(t.Context(), `SELECT name, email, full_name FROM _kora_user
		WHERE site = $1 AND enabled = $2
		AND (LOWER(full_name) LIKE $3 ESCAPE '!' OR LOWER(email) LIKE $4 ESCAPE '!')
		ORDER BY full_name, name LIMIT $5`, "tenant-a", int64(1), "alice%", "alice%", 25)
	if err != nil {
		t.Fatal("execute complete PostgreSQL user-directory query:", err)
	}
	assertUserDirectoryPickerSearchResults(t, results, 25)
}

func TestLibSQLUserDirectoryPrefixSearchUsesIndexesOverHTTP(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KORA_API_DISPOSABLE_LIBSQL_DSN"))
	if dsn == "" {
		t.Skip("set KORA_API_DISPOSABLE_LIBSQL_DSN to a fresh disposable loopback LibSQL HTTP database")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost") {
		t.Fatal("refusing non-loopback LibSQL HTTP DSN")
	}
	database, err := sql.Open("libsql", dsn)
	if err != nil {
		t.Fatal("open disposable LibSQL HTTP database:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	if err := database.PingContext(t.Context()); err != nil {
		t.Fatal("ping disposable LibSQL HTTP database:", err)
	}
	if _, err := database.ExecContext(t.Context(), `CREATE TABLE _kora_user (
		name TEXT NOT NULL PRIMARY KEY,
		site TEXT NOT NULL,
		email TEXT NOT NULL,
		full_name TEXT NOT NULL,
		enabled INTEGER NOT NULL DEFAULT 1
	)`); err != nil {
		t.Fatal("create isolated LibSQL user directory table:", err)
	}
	for _, statement := range (&kdb.LibSQLDialect{}).SystemTableSQL() {
		if !strings.Contains(statement, "idx_user_picker_") {
			continue
		}
		if _, err := database.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("create LibSQL user picker index: %v\n%s", err, statement)
		}
	}
	if _, err := database.ExecContext(t.Context(), `WITH RECURSIVE users(i) AS (
		SELECT 1 UNION ALL SELECT i + 1 FROM users WHERE i < 10000
	) INSERT INTO _kora_user (name, site, email, full_name, enabled)
	SELECT printf('user-%05d', i), 'tenant-a',
		CASE WHEN i % 100 = 0 THEN printf('alice-%05d@example.test', i) ELSE printf('person-%05d@example.test', i) END,
		CASE WHEN i % 100 = 0 THEN printf('Alice Example %05d', i) ELSE printf('Person %05d', i) END,
		1 FROM users`); err != nil {
		t.Fatal("populate disposable LibSQL user directory:", err)
	}
	if _, err := database.ExecContext(t.Context(), `INSERT INTO _kora_user (name, site, email, full_name, enabled) VALUES
		('tenant-b-user', 'tenant-b', 'alice-tenant-b@example.test', 'Alice AAA Tenant B', 1),
		('disabled-user', 'tenant-a', 'alice-disabled@example.test', 'Alice AAB Disabled', 0)`); err != nil {
		t.Fatal("insert LibSQL tenant and disabled-user controls:", err)
	}
	query := `EXPLAIN QUERY PLAN SELECT name, email, full_name FROM _kora_user
		WHERE site = ? AND enabled = ?
		AND (full_name LIKE ? ESCAPE '!' OR email LIKE ? ESCAPE '!')
		ORDER BY full_name, name LIMIT ?`
	planRows, err := database.QueryContext(t.Context(), query, "tenant-a", true, "alice%", "alice%", 25)
	if err != nil {
		t.Fatal("explain complete LibSQL user-directory query over HTTP:", err)
	}
	var plan []string
	for planRows.Next() {
		var id, parent, unused int
		var detail string
		if err := planRows.Scan(&id, &parent, &unused, &detail); err != nil {
			planRows.Close()
			t.Fatal("read LibSQL query plan:", err)
		}
		plan = append(plan, detail)
	}
	if err := planRows.Err(); err != nil {
		planRows.Close()
		t.Fatal("iterate LibSQL query plan:", err)
	}
	if err := planRows.Close(); err != nil {
		t.Fatal("close LibSQL query plan:", err)
	}
	joinedPlan := strings.Join(plan, "\n")
	if !strings.Contains(joinedPlan, "idx_user_picker_name") && !strings.Contains(joinedPlan, "idx_user_picker_email") {
		t.Fatalf("complete LibSQL user-directory query did not use a picker index:\n%s", joinedPlan)
	}
	results, err := database.QueryContext(t.Context(), `SELECT name, email, full_name FROM _kora_user
		WHERE site = ? AND enabled = ?
		AND (full_name LIKE ? ESCAPE '!' OR email LIKE ? ESCAPE '!')
		ORDER BY full_name, name LIMIT ?`, "tenant-a", true, "alice%", "alice%", 25)
	if err != nil {
		t.Fatal("execute complete LibSQL user-directory query over HTTP:", err)
	}
	assertUserDirectoryPickerSearchResults(t, results, 25)
}

func assertUserDirectoryPickerSearchResults(t *testing.T, rows *sql.Rows, expectedCount int) {
	t.Helper()
	defer rows.Close()

	count := 0
	for rows.Next() {
		var user UserDirectoryEntry
		if err := rows.Scan(&user.Name, &user.Email, &user.FullName); err != nil {
			t.Fatal("scan user-directory search result:", err)
		}
		count++
		if !strings.HasPrefix(strings.ToLower(user.FullName), "alice") && !strings.HasPrefix(strings.ToLower(user.Email), "alice") {
			t.Errorf("user-directory result does not match its visible name or email: %#v", user)
		}
		if user.Name == "tenant-b-user" || user.Name == "disabled-user" {
			t.Errorf("user-directory search leaked a cross-tenant or disabled account: %#v", user)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal("iterate user-directory search results:", err)
	}
	if count != expectedCount {
		t.Fatalf("user-directory search returned %d rows, want bounded result count %d", count, expectedCount)
	}
}
