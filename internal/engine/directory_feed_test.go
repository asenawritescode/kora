package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/asenawritescode/kora/site"
)

type directoryFeedRegistry struct {
	mu           sync.Mutex
	owners       map[string]string
	released     int
	acquired     chan struct{}
	acquiredOnce sync.Once
}

func newDirectoryFeedRegistry() *directoryFeedRegistry {
	return &directoryFeedRegistry{owners: make(map[string]string), acquired: make(chan struct{})}
}

func (r *directoryFeedRegistry) LoadConsumerCursor(string, uint64) (uint64, error) { return 0, nil }
func (r *directoryFeedRegistry) AcquireConsumerLease(id, owner string, _ time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.owners[id] = owner
	r.acquiredOnce.Do(func() { close(r.acquired) })
	return nil
}
func (r *directoryFeedRegistry) VerifyConsumerLease(id, owner string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.owners[id] != owner {
		return site.ErrDirectoryConsumerLeaseLost
	}
	return nil
}
func (r *directoryFeedRegistry) RenewConsumerLease(id, owner string, _ time.Duration) error {
	return r.VerifyConsumerLease(id, owner)
}
func (r *directoryFeedRegistry) ReleaseConsumerLease(id, owner string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.owners[id] == owner {
		delete(r.owners, id)
		r.released++
	}
	return nil
}
func (*directoryFeedRegistry) ChangesAfter(uint64, int) ([]site.SiteDirectoryChange, error) {
	return nil, nil
}
func (*directoryFeedRegistry) CurrentDirectoryRevision() (uint64, error) { return 0, nil }
func (*directoryFeedRegistry) AdvanceConsumerCursor(string, string, uint64, uint64) error {
	return nil
}
func (*directoryFeedRegistry) ReconcileConsumerCursor(string, string, uint64, uint64) error {
	return nil
}

type directoryWakeupStub struct {
	ready  chan uint64
	err    error
	closed chan struct{}
}

func (p *directoryWakeupStub) SubscribeSiteDirectoryWakeups(ctx context.Context, _ string) (<-chan uint64, error) {
	if p.err != nil {
		return nil, p.err
	}
	go func() {
		<-ctx.Done()
		close(p.ready)
		close(p.closed)
	}()
	return p.ready, nil
}
func (*directoryWakeupStub) PublishSiteDirectoryRevision(context.Context, uint64) error { return nil }

func TestDirectoryFeedShutdownJoinsPollingAndWakeupWorkers(t *testing.T) {
	registry := newDirectoryFeedRegistry()
	consumer := &site.DirectoryConsumer{Registry: registry, ID: "engine:test", Apply: func(context.Context, site.SiteDirectoryChange) error { return nil }}
	wakeups := &directoryWakeupStub{ready: make(chan uint64), closed: make(chan struct{})}
	worker, err := StartDirectoryFeed(context.Background(), consumer, wakeups)
	if err != nil {
		t.Fatalf("StartDirectoryFeed: %v", err)
	}
	select {
	case <-registry.acquired:
	case <-time.After(time.Second):
		t.Fatal("directory consumer did not acquire its lease before shutdown")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := worker.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-wakeups.closed:
	default:
		t.Fatal("shutdown returned before the provider closed its wakeup channel")
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if len(registry.owners) != 0 || registry.released == 0 {
		t.Fatalf("directory leases after shutdown: owners=%v released=%d", registry.owners, registry.released)
	}
}

type blockingDirectoryFeedRegistry struct {
	*directoryFeedRegistry
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingDirectoryFeedRegistry) LoadConsumerCursor(_ string, _ uint64) (uint64, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	return 0, nil
}

type closeCounter struct{ calls int }

func (c *closeCounter) Close() { c.calls++ }

func TestProcessResourcesRetainsProviderUntilDirectoryFeedJoins(t *testing.T) {
	registry := &blockingDirectoryFeedRegistry{
		directoryFeedRegistry: newDirectoryFeedRegistry(),
		started:               make(chan struct{}),
		release:               make(chan struct{}),
	}
	consumer := &site.DirectoryConsumer{Registry: registry, ID: "engine:blocked", Apply: func(context.Context, site.SiteDirectoryChange) error { return nil }}
	worker, err := StartDirectoryFeed(context.Background(), consumer, nil)
	if err != nil {
		t.Fatalf("StartDirectoryFeed: %v", err)
	}
	select {
	case <-registry.started:
	case <-time.After(time.Second):
		t.Fatal("directory consumer did not enter its registry operation")
	}
	provider := &closeCounter{}
	resources := &ProcessResources{DirectoryFeed: worker, NATSProvider: provider}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := resources.Shutdown(ctx); !errors.Is(err, ErrShutdownIncomplete) {
		t.Fatalf("Shutdown with blocked feed = %v, want incomplete shutdown", err)
	}
	if provider.calls != 0 {
		t.Fatal("NATS provider closed while directory feed was still running")
	}
	close(registry.release)
	if err := resources.Shutdown(context.Background()); err != nil {
		t.Fatalf("retry Shutdown after feed exits: %v", err)
	}
	if provider.calls != 1 {
		t.Fatalf("provider close calls = %d, want one after feed joined", provider.calls)
	}
}

func TestDirectoryFeedFallsBackToSQLPollingWhenWakeupSubscriptionFails(t *testing.T) {
	registry := newDirectoryFeedRegistry()
	consumer := &site.DirectoryConsumer{Registry: registry, ID: "engine:test", Apply: func(context.Context, site.SiteDirectoryChange) error { return nil }}
	wakeups := &directoryWakeupStub{err: errors.New("broker unavailable")}
	worker, err := StartDirectoryFeed(context.Background(), consumer, wakeups)
	if err != nil {
		t.Fatalf("StartDirectoryFeed with optional wakeup failure: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := worker.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}
