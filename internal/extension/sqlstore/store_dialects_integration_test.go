//go:build integration

package sqlstore

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
)

// An active extension's token resolves on both dialects. Postgres stores is_active as BOOLEAN, so the lookup
// must not compare it with the integer 1.
func TestVerifyAccessTokenFindsActiveExtensionAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_EXTENSION_STORE_MYSQL_DSN"},
		{"postgres", "KORA_EXTENSION_STORE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(tc.env))
			if dsn == "" {
				t.Skip("set " + tc.env + " to a disposable server")
			}
			database := disposabledb.Open(t, tc.driver, dsn)
			dialect := db.Resolve(tc.driver)
			ctx := context.Background()
			ddls := db.ExtensibilityTablesMySQL()
			if tc.driver == "postgres" {
				ddls = db.ExtensibilityTablesPostgres()
			}
			for _, ddl := range ddls {
				// The list repeats some ALTERs and indexes that bootstrap runs idempotently; a repeat is fine.
				if _, err := database.DB.ExecContext(ctx, ddl); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate") && !strings.Contains(err.Error(), "already exists") {
					t.Fatalf("create extensibility tables: %v", err)
				}
			}
			if _, err := database.DB.ExecContext(ctx, db.Rebind(dialect,
				`INSERT INTO _kora_extension (site, name, endpoint_url, secret, access_token_hash, api_permissions, is_active) VALUES (?, ?, ?, ?, ?, ?, ?)`),
				"acme", "kora-cloud-channel", "", "s1", "hash-on", "[]", true); err != nil {
				t.Fatalf("seed active extension: %v", err)
			}
			if _, err := database.DB.ExecContext(ctx, db.Rebind(dialect,
				`INSERT INTO _kora_extension (site, name, endpoint_url, secret, access_token_hash, api_permissions, is_active) VALUES (?, ?, ?, ?, ?, ?, ?)`),
				"acme", "retired", "", "s2", "hash-off", "[]", false); err != nil {
				t.Fatalf("seed inactive extension: %v", err)
			}
			store := New(database.DB, dialect)
			got, err := store.VerifyAccessToken(ctx, "acme", "hash-on")
			if err != nil || got.Name != "kora-cloud-channel" {
				t.Fatalf("active token = %+v, %v; want kora-cloud-channel", got, err)
			}
			if _, err := store.VerifyAccessToken(ctx, "acme", "hash-off"); err == nil {
				t.Fatal("inactive extension token resolved")
			}
		})
	}
}
