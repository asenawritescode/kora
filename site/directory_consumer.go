package site

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// DirectoryChangeFeed is the minimal registry boundary needed to consume
// directory revisions. It keeps propagation independent of SQL implementation.
type DirectoryChangeFeed interface {
	LoadConsumerCursor(consumerID string, initialCursor uint64) (uint64, error)
	AcquireConsumerLease(consumerID, ownerID string, duration time.Duration) error
	VerifyConsumerLease(consumerID, ownerID string) error
	RenewConsumerLease(consumerID, ownerID string, duration time.Duration) error
	ReleaseConsumerLease(consumerID, ownerID string) error
	ChangesAfter(cursor uint64, limit int) ([]SiteDirectoryChange, error)
	CurrentDirectoryRevision() (uint64, error)
	AdvanceConsumerCursor(consumerID, ownerID string, expected, next uint64) error
	ReconcileConsumerCursor(consumerID, ownerID string, expected, snapshotRevision uint64) error
}

// DirectoryConsumer applies the Engine-owned outbox feed before advancing its
// durable cursor. A failed application is retried from the same cursor after a
// restart; applications must therefore be revision-idempotent.
type DirectoryConsumer struct {
	Registry DirectoryChangeFeed
	ID       string
	// OwnerID is unique per process incarnation, while ID remains stable across
	// restarts. The lease owner fences accidental duplicate consumer identities.
	OwnerID  string
	PageSize int
	// InitialCursor is captured before Engine reads its eager startup snapshot.
	// It is used only if this consumer identity has no persisted cursor yet.
	InitialCursor uint64
	Apply         func(context.Context, SiteDirectoryChange) error
	// Resync applies an authoritative registry snapshot representing the
	// supplied directory revision. It is required to recover after compaction.
	Resync        func(context.Context, uint64) error
	mu            sync.Mutex
	loaded        bool
	leaseClaimed  bool
	cursor        uint64
	lastHeartbeat time.Time
}

const directoryConsumerHeartbeatInterval = 30 * time.Second
const directoryConsumerLeaseDuration = 90 * time.Second

func (c *DirectoryConsumer) ProcessBatch(ctx context.Context) (int, error) {
	if c == nil || c.Registry == nil || c.Apply == nil || c.ID == "" {
		return 0, errors.New("directory consumer requires registry, identity, and apply function")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.OwnerID == "" {
		id, err := newDirectoryChangeID()
		if err != nil {
			return 0, err
		}
		c.OwnerID = "consumer-owner:" + id
	}
	pageSize := c.PageSize
	if pageSize == 0 {
		pageSize = 100
	}
	if !c.loaded {
		cursor, err := c.Registry.LoadConsumerCursor(c.ID, c.InitialCursor)
		if err != nil {
			return 0, err
		}
		c.cursor = cursor
		c.loaded = true
	}
	now := time.Now()
	if !c.leaseClaimed {
		if err := c.Registry.AcquireConsumerLease(c.ID, c.OwnerID, directoryConsumerLeaseDuration); err != nil {
			return 0, err
		}
		c.leaseClaimed = true
		c.lastHeartbeat = now
	} else if now.Sub(c.lastHeartbeat) >= directoryConsumerHeartbeatInterval {
		if err := c.Registry.RenewConsumerLease(c.ID, c.OwnerID, directoryConsumerLeaseDuration); err != nil {
			c.leaseClaimed = false
			return 0, err
		}
		c.lastHeartbeat = now
	} else if err := c.Registry.VerifyConsumerLease(c.ID, c.OwnerID); err != nil {
		c.leaseClaimed = false
		return 0, err
	}
	cursor := c.cursor
	changes, err := c.Registry.ChangesAfter(cursor, pageSize)
	if err != nil {
		return 0, err
	}
	if len(changes) == 0 {
		latest, err := c.Registry.CurrentDirectoryRevision()
		if err != nil {
			return 0, err
		}
		if latest > cursor {
			return 0, c.resync(ctx, cursor, latest)
		}
	}
	applied := 0
	for _, change := range changes {
		if err := ctx.Err(); err != nil {
			return applied, err
		}
		if time.Since(c.lastHeartbeat) >= directoryConsumerHeartbeatInterval {
			if err := c.Registry.RenewConsumerLease(c.ID, c.OwnerID, directoryConsumerLeaseDuration); err != nil {
				c.leaseClaimed = false
				return applied, err
			}
			c.lastHeartbeat = time.Now()
		}
		if change.Cursor != cursor+1 {
			latest, err := c.Registry.CurrentDirectoryRevision()
			if err != nil {
				return applied, err
			}
			if err := c.resync(ctx, cursor, latest); err != nil {
				return applied, fmt.Errorf("directory feed gap: cursor %d followed by %d; resync failed: %w", cursor, change.Cursor, err)
			}
			return applied, nil
		}
		if err := c.Apply(ctx, change); err != nil {
			return applied, fmt.Errorf("apply directory change %d (%s): %w", change.Cursor, change.ID, err)
		}
		if err := c.Registry.AdvanceConsumerCursor(c.ID, c.OwnerID, cursor, change.Cursor); err != nil {
			if errors.Is(err, ErrDirectoryConsumerLeaseLost) {
				c.leaseClaimed = false
			}
			return applied, err
		}
		cursor = change.Cursor
		c.cursor = cursor
		c.lastHeartbeat = time.Now()
		applied++
	}
	return applied, nil
}

func (c *DirectoryConsumer) resync(ctx context.Context, cursor, latest uint64) error {
	if c.Resync == nil {
		return fmt.Errorf("directory history expired after cursor %d (latest %d) and no snapshot resync is configured", cursor, latest)
	}
	if err := c.Resync(ctx, latest); err != nil {
		return fmt.Errorf("apply directory snapshot at revision %d: %w", latest, err)
	}
	if err := c.Registry.ReconcileConsumerCursor(c.ID, c.OwnerID, cursor, latest); err != nil {
		if errors.Is(err, ErrDirectoryConsumerLeaseLost) {
			c.leaseClaimed = false
		}
		return err
	}
	c.cursor = latest
	return nil
}

// Run polls the durable feed. Errors are returned to the caller so supervision
// can report them; cursor advancement is deliberately withheld on any failure.
func (c *DirectoryConsumer) Run(ctx context.Context, interval time.Duration, onError func(error)) {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer c.releaseLease()
	for {
		_, err := c.ProcessBatch(ctx)
		if err != nil && !errors.Is(err, context.Canceled) && onError != nil {
			onError(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *DirectoryConsumer) releaseLease() {
	if c == nil || c.Registry == nil || c.ID == "" || c.OwnerID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.leaseClaimed {
		if err := c.Registry.ReleaseConsumerLease(c.ID, c.OwnerID); err != nil {
			// A lease is bounded and will expire if graceful release fails.
			return
		}
		c.leaseClaimed = false
	}
}
