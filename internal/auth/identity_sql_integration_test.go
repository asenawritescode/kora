package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	kdb "github.com/asenawritescode/kora/internal/db"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
)

// TestLiveIdentityLinksAcrossDialects runs against disposable databases only.
// Set KORA_AUTH_IDENTITY_DISPOSABLE_MYSQL_DSN and/or
// KORA_AUTH_IDENTITY_DISPOSABLE_POSTGRES_DSN.
func TestLiveIdentityLinksAcrossDialects(t *testing.T) {
	targets := []struct {
		dialect string
		env     string
		ddl     []string
	}{
		{"mysql", "KORA_AUTH_IDENTITY_DISPOSABLE_MYSQL_DSN", kdb.IdentityTablesMySQL()},
		{"postgres", "KORA_AUTH_IDENTITY_DISPOSABLE_POSTGRES_DSN", kdb.IdentityTablesPostgres()},
	}
	ran := false
	for _, target := range targets {
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
			for _, ddl := range target.ddl {
				if _, err := database.Exec(ddl); err != nil {
					t.Fatalf("create identity table: %v", err)
				}
			}
			ctx := context.Background()
			store := SQLIdentityLinks{DB: database, Dialect: kdb.Resolve(target.dialect)}
			site := fmt.Sprintf("identity-live-%d", time.Now().UnixNano())
			now := time.Now().UTC()

			if err := store.Link(ctx, site, "u1", "https://idp.example", "sub-1", "a@example.com", now); err != nil {
				t.Fatalf("link: %v", err)
			}
			if err := store.Link(ctx, site, "u1", "https://idp.example", "sub-1", "a@example.com", now); err != nil {
				t.Fatalf("relink same pair: %v", err)
			}
			if user, err := store.Resolve(ctx, site, "https://idp.example", "sub-1"); err != nil || user != "u1" {
				t.Fatalf("resolve = %q, %v", user, err)
			}
			if err := store.Link(ctx, site, "u2", "https://idp.example", "sub-1", "b@example.com", now); !errors.Is(err, ErrIdentityCollision) {
				t.Fatalf("link pair to another user error = %v, want collision", err)
			}
			if err := store.Link(ctx, site, "u1", "https://idp.example", "sub-2", "a@example.com", now); !errors.Is(err, ErrIdentityCollision) {
				t.Fatalf("link second subject for issuer error = %v, want collision", err)
			}
			if err := store.Link(ctx, site, "u1", "https://other-idp.example", "sub-1", "a@example.com", now); err != nil {
				t.Fatalf("link second issuer: %v", err)
			}
			if _, err := store.Resolve(ctx, "other-"+site, "https://idp.example", "sub-1"); !errors.Is(err, ErrLinkNotFound) {
				t.Fatalf("links leaked across sites: %v", err)
			}
		})
	}
	if !ran {
		t.Skip("set KORA_AUTH_IDENTITY_DISPOSABLE_MYSQL_DSN or KORA_AUTH_IDENTITY_DISPOSABLE_POSTGRES_DSN to run against disposable databases")
	}
}
