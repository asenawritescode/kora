//go:build integration

package engine

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/sweep"
)

// recordingWakeups is a NATS stand-in that never delivers subscriber wakeups and
// records every revision the relay publishes.
type recordingWakeups struct {
	published chan uint64
}

// SubscribeSiteDirectoryWakeups closes its channel on cancellation, as the
// provider contract requires, so the feed's relay goroutine can be joined.
func (w *recordingWakeups) SubscribeSiteDirectoryWakeups(ctx context.Context, _ string) (<-chan uint64, error) {
	wakes := make(chan uint64)
	go func() {
		<-ctx.Done()
		close(wakes)
	}()
	return wakes, nil
}

func (w *recordingWakeups) PublishSiteDirectoryRevision(_ context.Context, revision uint64) error {
	w.published <- revision
	return nil
}

// TestDirectoryRelayWakeIntegration proves that a directory change committed by
// this process reaches the NATS relay within a second, while the relay's sweep
// is long enough that only the commit wake can explain it.
func TestDirectoryRelayWakeIntegration(t *testing.T) {
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
			dialect := db.Resolve(tc.driver)
			if err := site.BootstrapPlatformRegistry(ctx, database, dialect); err != nil {
				t.Fatal("bootstrap platform registry:", err)
			}
			registry := site.NewSQLSiteRegistry(database, tc.driver)
			consumer := &site.DirectoryConsumer{
				Registry: registry, ID: "relay-wake-test", PageSize: 100,
				Apply: func(context.Context, site.DirectoryChange) error { return nil },
			}
			wakeups := &recordingWakeups{published: make(chan uint64, 8)}
			feed, err := StartDirectoryFeed(ctx, consumer, wakeups)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = feed.Shutdown(context.Background()) })
			// Let the relay finish its first (empty) pass and go idle on its sweep.
			time.Sleep(200 * time.Millisecond)

			host := fmt.Sprintf("relay-wake-%d.example.invalid", time.Now().UnixNano())
			if err := registry.Upsert(&site.Config{Hostname: host, DomainsList: []string{host}, DBType: tc.driver, DBHost: "127.0.0.1", DBPort: 3306, DBName: host}); err != nil {
				t.Fatal("register site:", err)
			}
			committed := time.Now()
			select {
			case revision := <-wakeups.published:
				if lag := time.Since(committed); lag > time.Second {
					t.Fatalf("relay published revision %d after %v, want < 1s", revision, lag)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("relay did not publish the committed directory change within 3s")
			}
		})
	}
}

// TestPendingDeletionWakeIntegration proves that a deletion committed by the
// console step is seen by the recovery worker's next pass within a second, and
// that the idle worker otherwise waits for its sweep.
func TestPendingDeletionWakeIntegration(t *testing.T) {
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
			host := fmt.Sprintf("delete-wake-%d.example.invalid", time.Now().UnixNano())
			if err := registry.Upsert(&site.Config{Hostname: host, DomainsList: []string{host}, DBType: tc.driver, DBHost: "127.0.0.1", DBPort: 3306, DBName: host}); err != nil {
				t.Fatal("register site:", err)
			}
			info, err := registry.ResolveAlias(host)
			if err != nil {
				t.Fatal(err)
			}
			seen := make(chan int, 4)
			wake := sweep.NewWake()
			worker, err := startPendingDeletionWorker(ctx, pendingDeletionRecovery{platformDB: database, dbType: tc.driver, retry: func(ctx context.Context, platformDB *sql.DB, dbType string, _ *site.CommonConfig) error {
				pending, listErr := site.NewSQLSiteRegistry(platformDB, dbType).GetPendingDeletionDescriptors()
				seen <- len(pending)
				return listErr
			}, interval: time.Hour, wake: wake.Subscribe()})
			if err != nil {
				t.Fatal(err)
			}
			worker.Start()
			<-seen // readiness pass sees nothing pending
			if _, err := registry.SetStatusAndGetDescriptor(info.SiteID, "deleting"); err != nil {
				t.Fatal("commit deletion request:", err)
			}
			wake.Signal() // the console step signals after its commit
			select {
			case n := <-seen:
				if n != 1 {
					t.Fatalf("recovery pass saw %d pending deletions, want 1", n)
				}
			case <-time.After(time.Second):
				t.Fatal("deletion request did not run a recovery pass within 1s")
			}
			shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			if err := worker.Shutdown(shutdown); err != nil {
				t.Fatal(err)
			}
		})
	}
}
