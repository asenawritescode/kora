package site

import (
	"errors"
	"fmt"
	"time"
)

// DirectoryConsumerLag describes how far one Engine replica has applied the
// durable directory and how old its oldest unapplied change is. It contains no
// site identifiers or credentials, so it is safe for telemetry.
type DirectoryConsumerLag struct {
	ConsumerRevision uint64
	LatestRevision   uint64
	RevisionLag      uint64
	OldestChangeAge  time.Duration
	ObservedAt       time.Time
}

// ConsumerLag reads one consumer cursor and the oldest still-pending change.
// A non-zero revision lag with no retained outbox row can happen after history
// compaction; in that case the consumer's last cursor-update time is used as a
// conservative stale-snapshot age until the required full resync succeeds.
func (r *SQLSiteRegistry) ConsumerLag(consumerID string) (DirectoryConsumerLag, error) {
	if r == nil || r.database == nil || consumerID == "" {
		return DirectoryConsumerLag{}, errors.New("site registry and consumer identity are required")
	}
	query := `SELECT c.directory_revision, m.directory_revision, MIN(o.created_at), c.updated_at
		FROM _kora_site_directory_consumers c
		JOIN _kora_site_directory_meta m ON m.id = 1
		LEFT JOIN _kora_site_directory_outbox o ON o.directory_revision > c.directory_revision
		WHERE c.consumer_id = ?
		GROUP BY c.directory_revision, m.directory_revision, c.updated_at`
	if r.dialect == "postgres" {
		query = `SELECT c.directory_revision, m.directory_revision, MIN(o.created_at), c.updated_at
			FROM _kora_site_directory_consumers c
			JOIN _kora_site_directory_meta m ON m.id = 1
			LEFT JOIN _kora_site_directory_outbox o ON o.directory_revision > c.directory_revision
			WHERE c.consumer_id = $1
			GROUP BY c.directory_revision, m.directory_revision, c.updated_at`
	}
	var status DirectoryConsumerLag
	var oldestRaw, updatedRaw any
	if err := r.database.QueryRow(query, consumerID).Scan(&status.ConsumerRevision, &status.LatestRevision, &oldestRaw, &updatedRaw); err != nil {
		return DirectoryConsumerLag{}, fmt.Errorf("read site directory consumer lag: %w", err)
	}
	status.ObservedAt = time.Now().UTC()
	if status.LatestRevision > status.ConsumerRevision {
		status.RevisionLag = status.LatestRevision - status.ConsumerRevision
		updatedAt, err := directoryMetricTime(updatedRaw)
		if err != nil {
			return DirectoryConsumerLag{}, fmt.Errorf("decode directory consumer cursor timestamp: %w", err)
		}
		oldestAt := updatedAt
		if oldestRaw != nil {
			oldestAt, err = directoryMetricTime(oldestRaw)
			if err != nil {
				return DirectoryConsumerLag{}, fmt.Errorf("decode oldest unapplied directory timestamp: %w", err)
			}
		}
		if age := time.Since(oldestAt); age > 0 {
			status.OldestChangeAge = age
		}
	}
	return status, nil
}

func directoryMetricTime(value any) (time.Time, error) {
	switch value := value.(type) {
	case time.Time:
		return value, nil
	case string:
		return parseDirectoryChangeTime(value)
	case []byte:
		return parseDirectoryChangeTime(string(value))
	case nil:
		return time.Time{}, errors.New("timestamp is null")
	default:
		return time.Time{}, fmt.Errorf("unsupported timestamp type %T", value)
	}
}
