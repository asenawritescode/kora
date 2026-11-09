package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	kdb "github.com/asenawritescode/kora/internal/db"
)

// TestLiveAuthProvidersAcrossDialects runs against disposable databases only,
// using the same DSN variables as TestLiveIdentityLinksAcrossDialects.
func TestLiveAuthProvidersAcrossDialects(t *testing.T) {
	ran := false
	for _, target := range []struct{ dialect, env string }{
		{"mysql", "KORA_AUTH_IDENTITY_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_AUTH_IDENTITY_DISPOSABLE_POSTGRES_DSN"},
	} {
		dsn := os.Getenv(target.env)
		if dsn == "" {
			continue
		}
		ran = true
		t.Run(target.dialect, func(t *testing.T) {
			database, err := sql.Open(target.dialect, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			dialect := kdb.Resolve(target.dialect)
			ddl := append(dialect.SystemTableSQL(), kdb.IdentityTablesMySQL()...)
			if target.dialect == "postgres" {
				ddl = append(dialect.SystemTableSQL(), kdb.IdentityTablesPostgres()...)
			}
			for _, statement := range ddl {
				if _, err := database.Exec(statement); err != nil && !idempotentDDL(err) {
					t.Fatalf("create tables: %v\n%s", err, statement)
				}
			}
			ctx := context.Background()
			site := fmt.Sprintf("providers-live-%d", time.Now().UnixNano())
			store := SQLAuthProviders{DB: database, Dialect: dialect}

			saved, err := store.Put(ctx, site, OIDCProvider{ID: "kora-cloud", Issuer: "https://id.kora.example/", ClientID: "studio"})
			if err != nil {
				t.Fatalf("put: %v", err)
			}
			saved.Enabled = true
			saved.DisplayName = "Kora Cloud"
			if _, err := store.Put(ctx, site, saved); err != nil {
				t.Fatalf("update: %v", err)
			}
			got, err := store.Get(ctx, site, "kora-cloud")
			if err != nil || !got.Enabled || got.DisplayName != "Kora Cloud" || got.Issuer != "https://id.kora.example" || got.ClientSecretKey != ClientSecretKeyFor("kora-cloud") {
				t.Fatalf("get = %+v, %v", got, err)
			}
			if list, err := store.List(ctx, site); err != nil || len(list) != 1 {
				t.Fatalf("list = %+v, %v", list, err)
			}
			if err := store.Delete(ctx, site, "kora-cloud"); err != nil {
				t.Fatalf("delete: %v", err)
			}
			if _, err := store.Get(ctx, site, "kora-cloud"); !errors.Is(err, ErrProviderNotFound) {
				t.Fatalf("get after delete error = %v", err)
			}

			if allowed, err := LocalSignInAllowed(ctx, database, dialect, site); err != nil || !allowed {
				t.Fatalf("default local sign-in = %v, %v", allowed, err)
			}
			if err := SetLocalSignInAllowed(ctx, database, dialect, site, false); err != nil {
				t.Fatalf("disable local sign-in: %v", err)
			}
			if err := SetLocalSignInAllowed(ctx, database, dialect, site, false); err != nil {
				t.Fatalf("disable local sign-in again: %v", err)
			}
			if allowed, err := LocalSignInAllowed(ctx, database, dialect, site); err != nil || allowed {
				t.Fatalf("local sign-in after disable = %v, %v", allowed, err)
			}
		})
	}
	if !ran {
		t.Skip("set KORA_AUTH_IDENTITY_DISPOSABLE_MYSQL_DSN or KORA_AUTH_IDENTITY_DISPOSABLE_POSTGRES_DSN to run against disposable databases")
	}
}

func idempotentDDL(err error) bool {
	message := err.Error()
	for _, fragment := range []string{"already exists", "Duplicate", "duplicate column", "Error 1060", "Error 1061", "Error 1050", "Error 1091", "Can't DROP", "does not exist"} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}
