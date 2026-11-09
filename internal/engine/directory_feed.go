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

	"github.com/asenawritescode/kora/internal/site"
)

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
			pollInterval = 30 * time.Second
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
				publisher.Run(ctx, time.Second, func(err error) {
					slog.Warn("NATS directory relay will retry without advancing its SQL cursor", "error", err)
				})
			}()
		}
	}

	workers.Add(1)
	go func() {
		defer workers.Done()
		consumer.Run(ctx, pollInterval, func(err error) {
			slog.Warn("site directory consumer will retry without advancing its cursor", "consumer_id", consumer.ID, "error", err)
		})
	}()
	if revisionWakeups != nil {
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
