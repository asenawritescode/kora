//go:build integration

package configstore_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
)

func TestCapabilityLeaseAdmissionSerializesWithRevocation(t *testing.T) {
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
			if test.name != "postgres" {
				database.SetMaxOpenConns(8)
			}
			store := configstore.NewModuleStore(database, test.dialect())
			if err := store.EnsureCapabilityLeaseSchema(context.Background()); err != nil {
				t.Fatal("ensure capability lease schema:", err)
			}
			for index := 0; index < 20; index++ {
				now := time.Now().UTC()
				lease := contract.CapabilityLease{
					ID: "lease-race-" + contract.NewID(), TenantID: "tenant-lease-race",
					Scope:          contract.BusinessScope{TenantID: "tenant-lease-race", Kind: contract.ScopeKindTenant},
					CallerModuleID: "sales.orders", ProviderID: "billing.provider", Operation: "issue",
					BindingRevision: 3, GrantEpoch: 1, ExpiresAt: now.Add(time.Hour), State: "active",
				}
				if err := store.CreateCapabilityLease(context.Background(), lease); err != nil {
					t.Fatal("create race lease:", err)
				}
				start := make(chan struct{})
				var group sync.WaitGroup
				group.Add(2)
				var admissionErr, revokeErr error
				go func() {
					defer group.Done()
					<-start
					tx, err := database.BeginTx(context.Background(), nil)
					if err != nil {
						admissionErr = err
						return
					}
					admissionErr = store.AdmitCapabilityAttemptInTx(context.Background(), tx, lease, time.Now().UTC())
					if admissionErr != nil {
						_ = tx.Rollback()
						return
					}
					admissionErr = tx.Commit()
				}()
				go func() {
					defer group.Done()
					<-start
					revokeErr = store.RevokeCapabilityLease(context.Background(), lease, "security-admin", "race test revocation", time.Now().UTC())
				}()
				close(start)
				group.Wait()
				if revokeErr != nil {
					t.Fatalf("concurrent revocation %d failed (admission=%v): %v", index, admissionErr, revokeErr)
				}
				if err := store.AdmitCapabilityAttempt(context.Background(), lease, time.Now().UTC()); err == nil {
					t.Fatalf("attempt admitted after revocation %d committed", index)
				}
				current, found, err := store.LoadCapabilityLease(context.Background(), lease.TenantID, lease.ID)
				if err != nil || !found || current.State != "revoked" || current.GrantEpoch != lease.GrantEpoch+1 {
					t.Fatalf("durable revoke state = (%+v,%t,%v)", current, found, err)
				}
			}
		})
	}
}
