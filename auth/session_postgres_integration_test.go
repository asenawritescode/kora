package auth

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	kdb "github.com/asenawritescode/kora/db"
	_ "github.com/lib/pq"
)

func TestLivePostgresSessionLifecycle(t *testing.T) {
	dsn := os.Getenv("KORA_AUTH_DISPOSABLE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_AUTH_DISPOSABLE_POSTGRES_DSN to test sessions against a disposable PostgreSQL database")
	}
	database, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("open disposable PostgreSQL database:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	schemaName := fmt.Sprintf("kora_auth_sessions_%d", time.Now().UnixNano())
	if _, err := database.Exec(`CREATE SCHEMA "` + schemaName + `"`); err != nil {
		t.Fatal("create isolated PostgreSQL schema:", err)
	}
	defer func() { _, _ = database.Exec(`DROP SCHEMA "` + schemaName + `" CASCADE`) }()
	if _, err := database.Exec(`SET search_path TO "` + schemaName + `"`); err != nil {
		t.Fatal("select isolated PostgreSQL schema:", err)
	}
	dialect := kdb.Resolve("postgres")
	for _, statement := range dialect.SystemTableSQL() {
		for _, table := range []string{`"_kora_session"`, `"_kora_user"`, `"_kora_magic_link"`} {
			if strings.HasPrefix(strings.TrimSpace(statement), "CREATE TABLE IF NOT EXISTS "+table) {
				if _, err := database.Exec(statement); err != nil {
					t.Fatalf("create %s: %v", table, err)
				}
			}
		}
	}
	if _, err := database.Exec(`INSERT INTO "_kora_user" (name, site, email, password_hash, full_name, enabled, roles) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		"u1", "site-a", "u1@example.com", "x", "User One", 1, "Sales User"); err != nil {
		t.Fatal("insert user:", err)
	}

	writer := newRequestSessionManager(database).SetDialect(dialect)
	user, err := writer.FindUserByEmailContext(context.Background(), "site-a", "u1@example.com")
	if err != nil {
		t.Fatal("find user by email:", err)
	}
	if err := writer.MarkEmailVerified("site-a", "u1@example.com"); err != nil {
		t.Fatal("mark email verified:", err)
	}
	if verified, err := writer.IsEmailVerified("site-a", "u1@example.com"); err != nil || !verified {
		t.Fatalf("email verified = %v, err = %v", verified, err)
	}
	sid, err := writer.CreateSession("site-a", user)
	if err != nil {
		t.Fatal("create session:", err)
	}

	// A fresh manager has an empty cache, so validation reads PostgreSQL.
	reader := newRequestSessionManager(database).SetDialect(dialect)
	got, _, err := reader.GetSession("site-a", sid)
	if err != nil {
		t.Fatal("validate session:", err)
	}
	if got.Name != "u1" || got.Email != "u1@example.com" {
		t.Fatalf("session user = %+v", got)
	}
	if _, err := reader.ListMagicLinks("site-a", "u1@example.com"); err != nil {
		t.Fatal("list magic links:", err)
	}
	if err := reader.RevokeAllMagicLinks("site-a", "u1@example.com"); err != nil {
		t.Fatal("revoke magic links:", err)
	}

	reader.DeleteSession(sid)
	if _, _, err := newRequestSessionManager(database).SetDialect(dialect).GetSession("site-a", sid); err == nil {
		t.Fatal("deleted session still validates")
	}
	var remaining int
	if err := database.QueryRow(`SELECT COUNT(*) FROM "_kora_session" WHERE sid = $1`, sid).Scan(&remaining); err != nil {
		t.Fatal("count sessions:", err)
	}
	if remaining != 0 {
		t.Fatalf("remaining sessions = %d", remaining)
	}
}
