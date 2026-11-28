package site

import (
	"context"
	"database/sql"
	"fmt"
)

// DirectoryWakeupPublisher publishes a directory revision wakeup to the broker.
// The Engine's relay and out-of-process writers share this one publish path.
type DirectoryWakeupPublisher interface {
	PublishSiteDirectoryRevision(context.Context, uint64) error
}

// AnnounceDirectoryRevision wakes Engine consumers after a writer in another
// process has committed a directory change. The wakeup carries only the latest
// committed revision; consumers read the durable SQL feed. The publish is
// idempotent per revision, so an announce that repeats a wakeup is harmless.
// Callers log a returned error: the Engine's NATS-down check and 5 min sweep
// recover a missed announce.
func AnnounceDirectoryRevision(ctx context.Context, database *sql.DB, publisher DirectoryWakeupPublisher) error {
	if database == nil || publisher == nil {
		return fmt.Errorf("announce directory revision requires a database and publisher")
	}
	var revision uint64
	if err := database.QueryRowContext(ctx, `SELECT directory_revision FROM _kora_site_directory_meta WHERE id = 1`).Scan(&revision); err != nil {
		return fmt.Errorf("read directory revision for announce: %w", err)
	}
	if revision == 0 {
		return nil
	}
	return publisher.PublishSiteDirectoryRevision(ctx, revision)
}
