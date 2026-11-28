package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/site"
)

// directoryWakeSweep is the consumer's SQL sweep while NATS wakeups are
// subscribed. A relayed revision wakes the consumer at once; the sweep only
// recovers a lost wakeup.
const directoryWakeSweep = 5 * time.Minute

// directoryOutageCheck is how often the consumer checks the directory revision
// while the broker is down. It bounds how long a change made during an outage
// waits to reach the consumer.
const directoryOutageCheck = 5 * time.Second

// SiteDirectoryWakeups provides best-effort wakeups for the durable SQL
// directory feed. SQL polling remains authoritative when the broker is absent.
type SiteDirectoryWakeups interface {
	SubscribeSiteDirectoryWakeups(context.Context, string) (<-chan uint64, error)
	PublishSiteDirectoryRevision(context.Context, uint64) error
}

// DirectoryFeedWorker owns the SQL poller, optional broker relay, and wakeup
// consumer. Shutdown cancels and joins every goroutine started by the worker.
type DirectoryFeedWorker struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// StartDirectoryFeed starts the durable SQL consumer and, when configured,
// its best-effort broker wakeup relay. Feed application and resync policy stay
// on the supplied consumer so they use the caller's tenant activation rules.
func StartDirectoryFeed(parent context.Context, consumer *site.DirectoryConsumer, wakeups SiteDirectoryWakeups) (*DirectoryFeedWorker, error) {
	if parent == nil {
		return nil, errors.New("directory feed context is required")
	}
	if consumer == nil || consumer.Registry == nil || consumer.ID == "" || consumer.Apply == nil {
		return nil, errors.New("directory feed requires a configured SQL consumer")
	}

	ctx, cancel := context.WithCancel(parent)
	worker := &DirectoryFeedWorker{cancel: cancel, done: make(chan struct{})}
	var workers sync.WaitGroup
	var revisionWakeups <-chan uint64
	pollInterval := time.Second
	if wakeups != nil {
		var err error
		revisionWakeups, err = wakeups.SubscribeSiteDirectoryWakeups(ctx, consumer.ID)
		if err != nil {
			slog.Warn("NATS directory wakeups unavailable; retaining SQL polling", "error", err)
			revisionWakeups = nil
		}
		if revisionWakeups != nil {
			pollInterval = directoryWakeSweep
			startDirectoryRelay(ctx, consumer, wakeups, &workers)
		}
	}

	workers.Add(1)
	go func() {
		defer workers.Done()
		consumer.Run(ctx, pollInterval, func(err error) {
			logDirectoryConsumerRetry(consumer.ID, err)
		})
	}()
	if revisionWakeups != nil {
		workers.Add(1)
		go func() {
			defer workers.Done()
			runDirectoryOutageCheck(ctx, consumer, wakeups)
		}()
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					// The provider contract requires the subscription goroutine to
					// close its channel after context cancellation. Drain to closure
					// so its final acknowledgment/unsubscribe work is joined too.
					for range revisionWakeups {
					}
					return
				case _, ok := <-revisionWakeups:
					if !ok {
						return
					}
					if _, err := consumer.ProcessBatch(ctx); err != nil && !errors.Is(err, context.Canceled) {
						slog.Warn("site directory wakeup apply failed; SQL poll will retry", "error", err)
					}
				}
			}
		}()
	}
	go func() {
		workers.Wait()
		close(worker.done)
	}()
	return worker, nil
}

// directoryPageSize mirrors the consumer's default page size.
func directoryPageSize(consumer *site.DirectoryConsumer) int {
	if consumer.PageSize > 0 {
		return consumer.PageSize
	}
	return 100
}

// startDirectoryRelay runs the NATS relay: it publishes each revision this
// process commits, retries a failed publish without advancing its SQL cursor,
// and is joined through workers.
func startDirectoryRelay(ctx context.Context, consumer *site.DirectoryConsumer, wakeups SiteDirectoryWakeups, workers *sync.WaitGroup) {
	digest := sha256.Sum256([]byte(consumer.ID))
	publisher := &site.DirectoryConsumer{
		Registry:        consumer.Registry,
		ContextRegistry: consumer.ContextRegistry,
		WorkAdmission:   consumer.WorkAdmission,
		ID:              "engine-directory-nats:" + hex.EncodeToString(digest[:]),
		PageSize:        100, InitialCursor: consumer.InitialCursor,
		Apply: func(_ context.Context, change site.DirectoryChange) error {
			return wakeups.PublishSiteDirectoryRevision(ctx, change.Cursor)
		},
		Resync: func(_ context.Context, revision uint64) error {
			return wakeups.PublishSiteDirectoryRevision(ctx, revision)
		},
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		// Committed directory changes of this process wake the relay; the
		// sweep only recovers a lost wake. A failed pass retries after 1 s.
		publisher.RunOnWake(ctx, site.SubscribeDirectoryChanges(), directoryWakeSweep, time.Second, func(err error) {
			slog.Warn("NATS directory relay will retry without advancing its SQL cursor", "error", err)
		})
	}()
}

// siteDirectoryConnectivity is implemented by brokers that can report whether
// their connection is up. Brokers without it are treated as connected.
type siteDirectoryConnectivity interface {
	Connected() bool
}

// runDirectoryOutageCheck runs the consumer's NATS-down fallback. While the
// broker is down no wakeup can arrive, so every directoryOutageCheck it reads
// the one-row directory revision and runs a consumer pass only when that
// revision moved. While the broker is up it reads no SQL at all.
func runDirectoryOutageCheck(ctx context.Context, consumer *site.DirectoryConsumer, wakeups SiteDirectoryWakeups) {
	connectivity, _ := wakeups.(siteDirectoryConnectivity)
	ticker := time.NewTicker(directoryOutageCheck)
	defer ticker.Stop()
	var seen uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if connectivity == nil || connectivity.Connected() {
			continue
		}
		seen = checkDirectoryRevision(ctx, consumer, seen)
	}
}

// checkDirectoryRevision runs one consumer pass when the directory revision
// moved past seen. It returns the revision the next check compares against; a
// failed pass or a pass that filled a page keeps the older value so it retries.
func checkDirectoryRevision(ctx context.Context, consumer *site.DirectoryConsumer, seen uint64) uint64 {
	latest, err := consumer.Registry.CurrentDirectoryRevision()
	if err != nil {
		slog.Warn("site directory revision check failed while NATS is down", "error", err)
		return seen
	}
	if latest == seen {
		return seen
	}
	applied, err := consumer.ProcessBatch(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			logDirectoryConsumerRetry(consumer.ID, err)
		}
		return seen
	}
	if applied >= directoryPageSize(consumer) {
		return seen
	}
	return latest
}

// Shutdown cancels the feed and waits for its goroutines to release their
// leases and stop using shared directory/provider resources.
func (w *DirectoryFeedWorker) Shutdown(ctx context.Context) error {
	if w == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if w.cancel != nil {
		w.cancel()
	}
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("join tenant directory feed: %w", ctx.Err())
	}
}

// logDirectoryConsumerRetry skips background-busy passes, which are normal.
func logDirectoryConsumerRetry(consumerID string, err error) {
	if dbaccess.IsBackgroundBusy(err) {
		return
	}
	slog.Warn("site directory consumer will retry without advancing its cursor", "consumer_id", consumerID, "error", err)
}
