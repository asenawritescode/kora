//go:build integration

package api

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/sweep"
)

// TestConsoleDeleteSignalsRecoveryAfterCommitIntegration proves that the console
// deletion step commits the deleting state, then signals the recovery wake: the
// pending row is visible to a reader the moment the wake arrives.
func TestConsoleDeleteSignalsRecoveryAfterCommitIntegration(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_SITE_DIRECTORY_LIVE_MYSQL_DSN"},
		{"postgres", "KORA_SITE_DIRECTORY_LIVE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s platform database", tc.env, tc.driver)
			}
			database := disposabledb.Open(t, tc.driver, dsn).DB
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			if err := site.BootstrapPlatformRegistry(ctx, database, db.Resolve(tc.driver)); err != nil {
				t.Fatal("bootstrap platform registry:", err)
			}
			registry := site.NewSQLSiteRegistry(database, tc.driver)
			host := fmt.Sprintf("console-delete-%d.example.invalid", time.Now().UnixNano())
			if err := registry.Upsert(&site.Config{Hostname: host, DomainsList: []string{host}, DBType: tc.driver, DBHost: "127.0.0.1", DBPort: 3306, DBName: host}); err != nil {
				t.Fatal("register site:", err)
			}
			info, err := registry.ResolveAlias(host)
			if err != nil {
				t.Fatal(err)
			}
			wake := sweep.NewWake()
			signalled := wake.Subscribe()
			handler := &ConsoleHandler{DeletionWake: wake}
			if _, err = handler.markSiteDeleting(registry, info.SiteID); err != nil {
				t.Fatal("mark site deleting:", err)
			}
			select {
			case <-signalled:
			case <-time.After(time.Second):
				t.Fatal("deletion request did not signal recovery within 1s")
			}
			pending, err := registry.GetPendingDeletionDescriptors()
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 1 || pending[0].SiteID != info.SiteID {
				t.Fatalf("pending deletions after wake = %v, want the requested site", pending)
			}
		})
	}
}
