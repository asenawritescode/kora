//go:build integration

package auth_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/auth"
	kdb "github.com/asenawritescode/kora/db"
	ksite "github.com/asenawritescode/kora/site"
	"github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
)

func TestPasswordLoginLookupAcrossDialects(t *testing.T) {
	for _, dialectName := range []string{"mysql", "postgres"} {
		t.Run(dialectName, func(t *testing.T) {
			database := openDisposablePasswordLoginDatabase(t, dialectName)
			dialect := kdb.Resolve(dialectName)
			if err := ksite.BootstrapSystemTables(database, dialect); err != nil {
				t.Fatal("bootstrap system tables:", err)
			}
			hash, err := bcrypt.GenerateFromPassword([]byte("correct horse"), bcrypt.MinCost)
			if err != nil {
				t.Fatal(err)
			}
			insert := kdb.Rebind(dialect, `INSERT INTO _kora_user (name, site, email, password_hash, full_name, enabled, email_verified_at, roles) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
			if _, err := database.Exec(insert, "USER-1", "duka", "amina@example.co.ke", string(hash), "Amina Hassan", kdb.BoolParam(dialect, true), time.Now(), "Sales User"); err != nil {
				t.Fatal("insert password user:", err)
			}
			if _, err := database.Exec(insert, "USER-2", "duka", "otieno@example.co.ke", "$kora$passwordless$disabled", "Otieno", kdb.BoolParam(dialect, true), time.Now(), ""); err != nil {
				t.Fatal("insert passwordless user:", err)
			}

			account, err := auth.LookupPasswordLoginAccountForTest(context.Background(), database, dialect, "duka", "AMINA@example.co.ke")
			if err != nil {
				t.Fatal("lookup by differently cased email:", err)
			}
			if !auth.PasswordMatchesForTest(account, "correct horse") || auth.PasswordMatchesForTest(account, "wrong") {
				t.Fatal("password comparison did not match the stored bcrypt hash")
			}
			passwordless, err := auth.LookupPasswordLoginAccountForTest(context.Background(), database, dialect, "duka", "otieno@example.co.ke")
			if err != nil {
				t.Fatal("lookup passwordless user:", err)
			}
			if auth.PasswordMatchesForTest(passwordless, "$kora$passwordless$disabled") {
				t.Fatal("passwordless marker must never match")
			}
			if _, err := auth.LookupPasswordLoginAccountForTest(context.Background(), database, dialect, "other-site", "amina@example.co.ke"); err != sql.ErrNoRows {
				t.Fatalf("lookup on another site = %v, want sql.ErrNoRows", err)
			}
		})
	}
}

func openDisposablePasswordLoginDatabase(t *testing.T, dialectName string) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("kora_password_login_%x", time.Now().UnixNano())
	switch dialectName {
	case "mysql":
		dsn := strings.TrimSpace(os.Getenv("KORA_CONFIG_DISPOSABLE_MYSQL_DSN"))
		if dsn == "" {
			t.Skip("set KORA_CONFIG_DISPOSABLE_MYSQL_DSN to a disposable MySQL service")
		}
		config, err := mysql.ParseDSN(dsn)
		if err != nil {
			t.Fatal("parse MySQL DSN:", err)
		}
		config.DBName = ""
		root, err := sql.Open("mysql", config.FormatDSN())
		if err != nil {
			t.Fatal("open MySQL:", err)
		}
		if _, err := root.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
			_ = root.Close()
			t.Fatal("create MySQL database:", err)
		}
		t.Cleanup(func() {
			_, _ = root.Exec("DROP DATABASE IF EXISTS `" + name + "`")
			_ = root.Close()
		})
		config.DBName = name
		config.ParseTime = true
		database, err := sql.Open("mysql", config.FormatDSN())
		if err != nil {
			t.Fatal("open MySQL database:", err)
		}
		t.Cleanup(func() { _ = database.Close() })
		return database
	default:
		dsn := strings.TrimSpace(os.Getenv("KORA_API_LIVE_POSTGRES_DSN"))
		if dsn == "" {
			t.Skip("set KORA_API_LIVE_POSTGRES_DSN to a disposable PostgreSQL database")
		}
		admin, err := sql.Open("postgres", dsn)
		if err != nil {
			t.Fatal("open PostgreSQL:", err)
		}
		if _, err := admin.Exec(`CREATE SCHEMA "` + name + `"`); err != nil {
			_ = admin.Close()
			t.Fatal("create PostgreSQL schema:", err)
		}
		t.Cleanup(func() {
			_, _ = admin.Exec(`DROP SCHEMA "` + name + `" CASCADE`)
			_ = admin.Close()
		})
		parsed, err := url.Parse(dsn)
		if err != nil {
			t.Fatal("parse PostgreSQL DSN:", err)
		}
		query := parsed.Query()
		query.Set("search_path", name)
		parsed.RawQuery = query.Encode()
		database, err := sql.Open("postgres", parsed.String())
		if err != nil {
			t.Fatal("open PostgreSQL schema:", err)
		}
		t.Cleanup(func() { _ = database.Close() })
		return database
	}
}
