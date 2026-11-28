//go:build integration

package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/site"
)

// outageBroker stands in for NATS. Its connection state is switchable, the relay
// can be muted to isolate the writer announce path, and wakeups are delivered
// only through a successful publish.
type outageBroker struct {
	connected  atomic.Bool
	relayMuted atomic.Bool
	mu         sync.Mutex
	closed     bool
	wakes      chan uint64
}

func newOutageBroker() *outageBroker {
	b := &outageBroker{wakes: make(chan uint64, 16)}
	b.connected.Store(true)
	return b
}

func (b *outageBroker) Connected() bool { return b.connected.Load() }

// SubscribeSiteDirectoryWakeups closes its channel on cancellation, as the
// provider contract requires, so the feed's goroutines can be joined.
func (b *outageBroker) SubscribeSiteDirectoryWakeups(ctx context.Context, _ string) (<-chan uint64, error) {
	go func() {
		<-ctx.Done()
		b.mu.Lock()
		defer b.mu.Unlock()
		b.closed = true
		close(b.wakes)
	}()
	return b.wakes, nil
}

func (b *outageBroker) deliver(revision uint64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return errors.New("broker closed")
	}
	select {
	case b.wakes <- revision:
		return nil
	default:
		return errors.New("wake buffer full")
	}
}

// PublishSiteDirectoryRevision is the relay's publish path.
func (b *outageBroker) PublishSiteDirectoryRevision(_ context.Context, revision uint64) error {
	if !b.connected.Load() || b.relayMuted.Load() {
		return errors.New("nats unavailable")
	}
	return b.deliver(revision)
}

// outageAnnouncer is the out-of-process writer's publish path.
type outageAnnouncer struct{ broker *outageBroker }

func (a outageAnnouncer) PublishSiteDirectoryRevision(_ context.Context, revision uint64) error {
	if !a.broker.connected.Load() {
		return errors.New("nats unavailable")
	}
	return a.broker.deliver(revision)
}

// countingFeed counts fallback revision reads; the embedded registry supplies
// everything else.
type countingFeed struct {
	*site.SQLSiteRegistry
	revisionReads atomic.Int64
}

func (f *countingFeed) CurrentDirectoryRevision() (uint64, error) {
	f.revisionReads.Add(1)
	return f.SQLSiteRegistry.CurrentDirectoryRevision()
}

// TestDirectoryOutageIntegration covers the writer announce path and the
// NATS-down revision check against a real SQL feed.
func TestDirectoryOutageIntegration(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_SITE_DIRECTORY_LIVE_MYSQL_DSN"},
		{"postgres", "KORA_SITE_DIRECTORY_LIVE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s platform database", tc.env, tc.driver)
			}
			t.Run("out-of-process announce reaches consumer within 2s", func(t *testing.T) {
				broker := newOutageBroker()
				broker.relayMuted.Store(true)
				database, _, applied, stop := startOutageFeed(t, tc.driver, dsn, broker)
				defer stop()
				committed := time.Now()
				writeOutageSite(t, database, tc.driver, "announce")
				announceOutageRevision(t, database, broker)
				target := currentOutageRevision(t, database, tc.driver)
				if err := waitOutageApplied(applied, target, 2*time.Second); err != nil {
					t.Fatal(err)
				}
				if lag := time.Since(committed); lag > 2*time.Second {
					t.Fatalf("announced change applied after %v, want < 2s", lag)
				}
			})
			t.Run("NATS down applies a committed change within 7s", func(t *testing.T) {
				broker := newOutageBroker()
				broker.connected.Store(false)
				database, _, applied, stop := startOutageFeed(t, tc.driver, dsn, broker)
				defer stop()
				committed := time.Now()
				writeOutageSite(t, database, tc.driver, "down")
				target := currentOutageRevision(t, database, tc.driver)
				if err := waitOutageApplied(applied, target, 7*time.Second); err != nil {
					t.Fatal(err)
				}
				if lag := time.Since(committed); lag > 7*time.Second {
					t.Fatalf("change made during outage applied after %v, want < 7s", lag)
				}
			})
			t.Run("NATS up and idle reads no revision", func(t *testing.T) {
				broker := newOutageBroker()
				_, feed, _, stop := startOutageFeed(t, tc.driver, dsn, broker)
				defer stop()
				time.Sleep(12 * time.Second)
				if reads := feed.revisionReads.Load(); reads != 0 {
					t.Fatalf("idle NATS-up consumer read the directory revision %d times, want 0", reads)
				}
			})
		})
	}
}

// startOutageFeed opens a disposable database and starts a directory feed whose
// consumer records every applied cursor.
func startOutageFeed(t *testing.T, driver, dsn string, broker *outageBroker) (*sql.DB, *countingFeed, *atomic.Uint64, func()) {
	t.Helper()
	database := disposabledb.Open(t, driver, dsn).DB
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	if err := site.BootstrapPlatformRegistry(ctx, database, db.Resolve(driver)); err != nil {
		cancel()
		t.Fatal("bootstrap platform registry:", err)
	}
	feed := &countingFeed{SQLSiteRegistry: site.NewSQLSiteRegistry(database, driver)}
	var applied atomic.Uint64
	consumer := &site.DirectoryConsumer{
		Registry: feed, ID: fmt.Sprintf("engine-outage-test:%s:%d", driver, time.Now().UnixNano()), PageSize: 100,
		Apply: func(_ context.Context, change site.DirectoryChange) error {
			applied.Store(change.Cursor)
			return nil
		},
	}
	worker, err := StartDirectoryFeed(ctx, consumer, broker)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return database, feed, &applied, func() {
		cancel()
		_ = worker.Shutdown(context.Background())
	}
}

func writeOutageSite(t *testing.T, database *sql.DB, driver, label string) {
	t.Helper()
	host := fmt.Sprintf("outage-%s-%d.example.invalid", label, time.Now().UnixNano())
	registry := site.NewSQLSiteRegistry(database, driver)
	if err := registry.Upsert(&site.Config{Hostname: host, DomainsList: []string{host}, DBType: driver, DBHost: "127.0.0.1", DBPort: 3306, DBName: host}); err != nil {
		t.Fatal("register site:", err)
	}
}

func announceOutageRevision(t *testing.T, database *sql.DB, broker *outageBroker) {
	t.Helper()
	if err := site.AnnounceDirectoryRevision(t.Context(), database, outageAnnouncer{broker: broker}); err != nil {
		t.Fatal("announce directory revision:", err)
	}
}

func currentOutageRevision(t *testing.T, database *sql.DB, driver string) uint64 {
	t.Helper()
	revision, err := site.NewSQLSiteRegistry(database, driver).CurrentDirectoryRevision()
	if err != nil {
		t.Fatal("read directory revision:", err)
	}
	return revision
}

func waitOutageApplied(applied *atomic.Uint64, target uint64, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for applied.Load() < target {
		if time.Now().After(deadline) {
			return fmt.Errorf("consumer applied cursor %d did not reach revision %d within %v", applied.Load(), target, limit)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}
