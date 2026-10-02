package site

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/natsprovider"
	_ "github.com/lib/pq"
)

// TestLivePostgresNATSSiteDirectoryRecovery exercises an external JetStream
// server with a disposable PostgreSQL directory. SQL remains the source of
// truth: closing the publisher connection must not block a directory consumer,
// and reconnecting must publish the outbox revision that was not relayed.
func TestLivePostgresNATSSiteDirectoryRecovery(t *testing.T) {
	dsn := os.Getenv("KORA_SITE_DIRECTORY_LIVE_POSTGRES_DSN")
	natsURL := os.Getenv("KORA_SITE_DIRECTORY_LIVE_NATS_URL")
	if dsn == "" || natsURL == "" {
		t.Skip("set KORA_SITE_DIRECTORY_LIVE_POSTGRES_DSN and KORA_SITE_DIRECTORY_LIVE_NATS_URL for live SQL+NATS recovery")
	}
	database, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Ping(); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapPlatformRegistry(database, db.Resolve("postgres")); err != nil {
		t.Fatal("bootstrap platform registry:", err)
	}
	registry := NewSQLSiteRegistry(database, "postgres")
	startCursor, err := registry.CurrentDirectoryRevision()
	if err != nil {
		t.Fatal("read startup feed boundary:", err)
	}
	id := fmt.Sprintf("%d", time.Now().UnixNano())
	hostname := "nats-directory-" + id + ".example.invalid"
	consumerID := "integration:nats-directory:" + id
	cfg := natsprovider.Config{
		Name: "kora-directory-integration", ServerURLs: []string{natsURL},
		StreamName: "DIRTEST_EVENTS_" + id, SubjectPrefix: "dirtest_" + id,
		MaxDeliver: 5,
	}
	provider, err := natsprovider.New(context.Background(), cfg)
	if err != nil {
		t.Fatal("connect live NATS:", err)
	}
	defer provider.Close()
	if err := provider.Bootstrap(context.Background()); err != nil {
		t.Fatal("bootstrap live NATS event stream:", err)
	}
	if err := provider.BootstrapSiteDirectory(context.Background()); err != nil {
		t.Fatal("bootstrap live NATS directory stream:", err)
	}
	wakeCtx, cancelWake := context.WithCancel(context.Background())
	wakeups, err := provider.SubscribeSiteDirectoryWakeups(wakeCtx, consumerID)
	if err != nil {
		cancelWake()
		t.Fatal("subscribe live directory wakeups:", err)
	}
	defer cancelWake()

	if err := registry.Upsert(&SiteConfig{
		Hostname: hostname, DomainsList: []string{hostname, "alias." + hostname},
		DBType: "postgres", DBHost: "127.0.0.1", DBPort: 5432, DBName: "unused",
	}); err != nil {
		t.Fatal("create disposable directory entry:", err)
	}
	defer func() {
		if err := removePlatformSiteRegistration(database, "postgres", hostname); err != nil {
			t.Errorf("remove disposable directory entry: %v", err)
		}
	}()
	info, err := registry.ResolveAlias("alias." + hostname)
	if err != nil {
		t.Fatal("resolve disposable alias:", err)
	}

	newRelay := func(p *natsprovider.Provider) *DirectoryConsumer {
		return &DirectoryConsumer{
			Registry: registry, ID: "integration:nats-relay:" + id, InitialCursor: startCursor,
			Apply: func(ctx context.Context, change SiteDirectoryChange) error {
				return p.PublishSiteDirectoryRevision(ctx, change.Cursor)
			},
			Resync: func(ctx context.Context, revision uint64) error {
				return p.PublishSiteDirectoryRevision(ctx, revision)
			},
		}
	}
	relay := newRelay(provider)
	defer relay.releaseLease()
	if count, err := relay.ProcessBatch(context.Background()); err != nil || count != 1 {
		t.Fatalf("relay create revision = %d, %v", count, err)
	}
	waitWakeup := func(ch <-chan uint64, want uint64) {
		t.Helper()
		select {
		case got, ok := <-ch:
			if !ok || got != want {
				t.Fatalf("directory wakeup = %d (open=%v), want %d", got, ok, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for directory wakeup revision %d", want)
		}
	}
	wakeup, err := registry.CurrentDirectoryRevision()
	if err != nil {
		t.Fatal(err)
	}
	waitWakeup(wakeups, wakeup)

	var applied []SiteDirectoryChange
	consumer := &DirectoryConsumer{Registry: registry, ID: consumerID, InitialCursor: startCursor, Apply: func(_ context.Context, change SiteDirectoryChange) error {
		applied = append(applied, change)
		return nil
	}}
	defer consumer.releaseLease()
	if count, err := consumer.ProcessBatch(context.Background()); err != nil || count != 1 {
		t.Fatalf("consume create from SQL after NATS wake = %d, %v", count, err)
	}
	if len(applied) != 1 || applied[0].Descriptor.SiteID != info.SiteID {
		t.Fatalf("SQL consumer applied unexpected create descriptor: %#v", applied)
	}

	// Simulate a publisher-side broker outage. The SQL runtime consumer still
	// processes the committed directory change and advances its own cursor.
	provider.Close()
	if err := registry.SetStatus(info.SiteID, "suspended"); err != nil {
		t.Fatal("commit status revision during broker outage:", err)
	}
	if count, err := relay.ProcessBatch(context.Background()); err == nil || count != 0 {
		t.Fatalf("relay during NATS outage = %d, %v; want publish error without cursor advance", count, err)
	}
	if count, err := consumer.ProcessBatch(context.Background()); err != nil || count != 1 {
		t.Fatalf("SQL consumer during NATS outage = %d, %v", count, err)
	}
	if len(applied) != 2 || applied[1].Descriptor.Status != "suspended" {
		t.Fatalf("SQL fallback did not apply outage revision: %#v", applied)
	}

	// Reconnect with the same JetStream identity. The relay's durable SQL cursor
	// remains behind, so it must publish the missed revision after recovery.
	relay.releaseLease()
	recovered, err := natsprovider.New(context.Background(), cfg)
	if err != nil {
		t.Fatal("reconnect live NATS:", err)
	}
	defer recovered.Close()
	if err := recovered.Bootstrap(context.Background()); err != nil {
		t.Fatal("rebootstrap live NATS event stream:", err)
	}
	if err := recovered.BootstrapSiteDirectory(context.Background()); err != nil {
		t.Fatal("rebootstrap live NATS directory stream:", err)
	}
	cancelWake()
	for range wakeups {
	}
	resumeCtx, cancelResume := context.WithCancel(context.Background())
	defer cancelResume()
	resumedWakeups, err := recovered.SubscribeSiteDirectoryWakeups(resumeCtx, consumerID)
	if err != nil {
		t.Fatal("resume live durable directory wakeups:", err)
	}
	recoveredRelay := newRelay(recovered)
	defer recoveredRelay.releaseLease()
	if count, err := recoveredRelay.ProcessBatch(context.Background()); err != nil || count != 1 {
		t.Fatalf("relay missed revision after reconnect = %d, %v", count, err)
	}
	latest, err := registry.CurrentDirectoryRevision()
	if err != nil {
		t.Fatal(err)
	}
	waitWakeup(resumedWakeups, latest)
	if count, err := consumer.ProcessBatch(context.Background()); err != nil || count != 0 {
		t.Fatalf("duplicate wake after SQL cursor recovery = %d, %v", count, err)
	}
}
