package engine

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/storage"
)

// outboxArchiveInterval is the gap between archive runs for one tenant.
const outboxArchiveInterval = 24 * time.Hour

// outboxArchiveStore adapts the tenant storage backend to the archiver's
// write-only store.
type outboxArchiveStore struct{ backend storage.Backend }

func (s outboxArchiveStore) Put(ctx context.Context, key string, r io.Reader, size int64, meta storage.FileMeta) error {
	_, err := s.backend.Put(ctx, key, r, size, meta)
	return err
}

// archiveMaintenance is the admission an archive run holds. dbaccess.Admission
// satisfies it.
type archiveMaintenance interface {
	AcquireMaintenance(context.Context) (func(), error)
}

// archiveFirstDelay spreads each tenant's first run across the first hour, so a
// restart does not archive every tenant at once.
func archiveFirstDelay(random func(time.Duration) time.Duration) time.Duration {
	return min(max(random(time.Hour), 0), time.Hour-time.Nanosecond)
}

// RunOutboxArchiver archives due outbox rows after a random first delay, then
// daily, holding one maintenance admission slot per run. It returns on ctx end.
func RunOutboxArchiver(ctx context.Context, siteName string, archiver *outbox.Archiver, admission archiveMaintenance, random func(time.Duration) time.Duration) {
	timer := time.NewTimer(archiveFirstDelay(random))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		runOutboxArchive(ctx, siteName, archiver, admission)
		timer.Reset(outboxArchiveInterval)
	}
}

func runOutboxArchive(ctx context.Context, siteName string, archiver *outbox.Archiver, admission archiveMaintenance) {
	release, err := admission.AcquireMaintenance(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("outbox archive admission refused; retrying tomorrow", "site", siteName, "error", err)
		}
		return
	}
	defer release()
	if _, err := archiver.ArchiveDue(ctx, siteName); err != nil && ctx.Err() == nil {
		slog.Warn("outbox archive run failed", "site", siteName, "error", err)
	}
}
