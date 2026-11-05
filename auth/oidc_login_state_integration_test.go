package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/auth/oidc"
	kdb "github.com/asenawritescode/kora/db"
)

// TestLiveOIDCLoginStatesAcrossDialects runs against disposable databases only.
func TestLiveOIDCLoginStatesAcrossDialects(t *testing.T) {
	ran := false
	for _, target := range []struct {
		dialect, env string
		ddl          []string
	}{
		{"mysql", "KORA_AUTH_IDENTITY_DISPOSABLE_MYSQL_DSN", kdb.IdentityTablesMySQL()},
		{"postgres", "KORA_AUTH_IDENTITY_DISPOSABLE_POSTGRES_DSN", kdb.IdentityTablesPostgres()},
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
			for _, statement := range target.ddl {
				if _, err := database.Exec(statement); err != nil && !idempotentDDL(err) {
					t.Fatalf("create tables: %v", err)
				}
			}
			ctx := context.Background()
			store := SQLLoginStates{DB: database, Dialect: kdb.Resolve(target.dialect)}
			now := time.Now().UTC()
			site := fmt.Sprintf("oidc-live-%d", now.UnixNano())
			pending := oidcLoginState{Site: site, ProviderID: "kora-cloud", Nonce: "n", CodeVerifier: "v", ReturnTo: "/workspace", BindingHash: sha256Hex("b"), ExpiresAt: now.Add(10 * time.Minute)}

			if err := store.Save(ctx, sha256Hex(site+"ok"), pending); err != nil {
				t.Fatalf("save: %v", err)
			}
			got, err := store.Consume(ctx, site, sha256Hex(site+"ok"), now)
			if err != nil || got.Nonce != "n" || got.CodeVerifier != "v" || got.BindingHash != pending.BindingHash {
				t.Fatalf("consume = %+v, %v", got, err)
			}
			if _, err := store.Consume(ctx, site, sha256Hex(site+"ok"), now); !errors.Is(err, oidc.ErrStateInvalid) {
				t.Fatalf("second consume error = %v, want invalid", err)
			}

			if err := store.Save(ctx, sha256Hex(site+"other-site"), pending); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Consume(ctx, "other-"+site, sha256Hex(site+"other-site"), now); !errors.Is(err, oidc.ErrStateSiteMismatch) {
				t.Fatalf("cross-site consume error = %v", err)
			}

			if err := store.Save(ctx, sha256Hex(site+"late"), pending); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Consume(ctx, site, sha256Hex(site+"late"), now.Add(11*time.Minute)); !errors.Is(err, oidc.ErrStateExpired) {
				t.Fatalf("expired consume error = %v", err)
			}
		})
	}
	if !ran {
		t.Skip("set KORA_AUTH_IDENTITY_DISPOSABLE_MYSQL_DSN or KORA_AUTH_IDENTITY_DISPOSABLE_POSTGRES_DSN to run against disposable databases")
	}
}
