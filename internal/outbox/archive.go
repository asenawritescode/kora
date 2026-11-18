package outbox

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/storage"
)

const (
	// DefaultArchiveHorizon keeps published outbox rows in the table for 400 days.
	DefaultArchiveHorizon = 400 * 24 * time.Hour
	// MinArchiveHorizon is the rebuild window: rows newer than it stay in place.
	MinArchiveHorizon = 90 * 24 * time.Hour
	// DefaultArchiveChunkSize bounds the rows archived and deleted per transaction.
	DefaultArchiveChunkSize = 1000
)

// ErrArchiveHorizon rejects a replay horizon shorter than the rebuild window.
var ErrArchiveHorizon = errors.New("outbox: archive horizon is shorter than the 90-day rebuild window")

var errArchiveDependencies = errors.New("outbox: archiver requires a database, dialect and store")

// ArchiveStore writes one archive object. An engine adapter wraps storage.Backend.
type ArchiveStore interface {
	Put(ctx context.Context, key string, r io.Reader, size int64, meta storage.FileMeta) error
}

// Archiver moves published outbox rows older than Horizon into archive objects,
// then deletes them. Build it with NewArchiver.
type Archiver struct {
	DB        *sql.DB
	Dialect   db.Dialect
	Store     ArchiveStore
	Horizon   time.Duration
	ChunkSize int
	Metrics   *BacklogMetrics
}

// NewArchiver validates the dependencies and fills defaults. Zero Horizon and
// ChunkSize take their defaults; a Horizon below MinArchiveHorizon is rejected.
func NewArchiver(a Archiver) (*Archiver, error) {
	if a.DB == nil || a.Dialect == nil || a.Store == nil {
		return nil, errArchiveDependencies
	}
	if a.Horizon == 0 {
		a.Horizon = DefaultArchiveHorizon
	}
	if a.Horizon < MinArchiveHorizon {
		return nil, ErrArchiveHorizon
	}
	if a.ChunkSize <= 0 {
		a.ChunkSize = DefaultArchiveChunkSize
	}
	return &a, nil
}

// ArchiveDue archives chunks of due rows for site until none remain or ctx ends.
// It returns the number of rows deleted.
func (a *Archiver) ArchiveDue(ctx context.Context, site string) (int, error) {
	total := 0
	for {
		n, err := a.archiveChunk(ctx, site)
		if err != nil {
			a.Metrics.RecordArchiveFailure()
			return total, err
		}
		if n == 0 {
			return total, nil
		}
		total += n
		a.Metrics.RecordArchived(int64(n))
	}
}

// archiveChunk writes one object, then deletes its rows. The object key is
// deterministic, so a rerun after a crash overwrites the same object. Rows are
// deleted only after Put succeeds.
func (a *Archiver) archiveChunk(ctx context.Context, site string) (int, error) {
	rows, created, err := a.selectDue(ctx, site)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	body, err := encodeArchive(rows)
	if err != nil {
		return 0, err
	}
	key := archiveKey(site, created, rows)
	meta := storage.FileMeta{Key: key, Filename: key[strings.LastIndex(key, "/")+1:], MIMEType: "application/gzip", Size: int64(body.Len())}
	if err := a.Store.Put(ctx, key, bytes.NewReader(body.Bytes()), int64(body.Len()), meta); err != nil {
		return 0, fmt.Errorf("outbox: write archive %s: %w", key, err)
	}
	if err := a.deleteArchived(ctx, site, rows); err != nil {
		return 0, err
	}
	return len(rows), nil
}

// archiveRow holds every _kora_outbox column, keyed by column name.
type archiveRow map[string]any

// archiveColumns lists the _kora_outbox columns that the archive keeps.
var archiveColumns = []string{"id", "site", "source_sequence", "event_type", "event_version", "aggregate_type", "aggregate_id", "payload", "status", "attempts", "next_attempt_at", "lease_owner", "lease_until", "created_at", "published_at", "last_error"}

// selectDue returns up to ChunkSize published rows older than the horizon that no
// delivery failure references, oldest first, plus the first row's created_at.
func (a *Archiver) selectDue(ctx context.Context, site string) ([]archiveRow, time.Time, error) {
	query := `SELECT ` + strings.Join(archiveColumns, ",") + ` FROM _kora_outbox o WHERE o.site = ? AND o.status = ? AND o.created_at < ` + a.Dialect.NowMinusSeconds() + ` AND NOT EXISTS (SELECT 1 FROM _kora_outbox_delivery_failure f WHERE f.site = o.site AND f.event_id = o.id) ORDER BY o.created_at, o.id LIMIT ?`
	rows, err := a.DB.QueryContext(ctx, db.Rebind(a.Dialect, query), site, string(StatusPublished), archiveHorizonSeconds(a.Horizon), a.ChunkSize)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("outbox: select archive rows: %w", err)
	}
	defer rows.Close()
	var out []archiveRow
	var created time.Time
	for rows.Next() {
		values := make([]any, len(archiveColumns))
		targets := make([]any, len(values))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, time.Time{}, fmt.Errorf("outbox: scan archive row: %w", err)
		}
		row := archiveRow{}
		for i, name := range archiveColumns {
			row[name] = normalizeArchiveValue(values[i])
		}
		if len(out) == 0 {
			created, _ = values[indexOf(archiveColumns, "created_at")].(time.Time)
		}
		out = append(out, row)
	}
	return out, created, rows.Err()
}

func archiveHorizonSeconds(horizon time.Duration) int64 {
	return int64(horizon / time.Second)
}

// ArchiveDueProbe returns a query that selects one row when ArchiveDue would
// find work for horizon, and its bound arguments. It applies the same filters
// as the archiver, so a probe never reports rows the archiver keeps.
func ArchiveDueProbe(dialect db.Dialect, horizon time.Duration) (string, []any) {
	query := `SELECT 1 FROM _kora_outbox o WHERE o.status = 'published' AND o.created_at < ` + dialect.NowMinusSeconds() + ` AND NOT EXISTS (SELECT 1 FROM _kora_outbox_delivery_failure f WHERE f.site = o.site AND f.event_id = o.id) LIMIT 1`
	return db.Rebind(dialect, query), []any{archiveHorizonSeconds(horizon)}
}

// normalizeArchiveValue turns driver values into JSON-friendly ones: bytes become
// strings and instants become UTC RFC 3339 with nanoseconds.
func normalizeArchiveValue(v any) any {
	switch typed := v.(type) {
	case []byte:
		return string(typed)
	case time.Time:
		return typed.UTC().Format(time.RFC3339Nano)
	default:
		return v
	}
}

func indexOf(list []string, want string) int {
	for i, item := range list {
		if item == want {
			return i
		}
	}
	return -1
}

// encodeArchive writes rows as gzip-compressed newline-delimited JSON.
func encodeArchive(rows []archiveRow) (*bytes.Buffer, error) {
	var body bytes.Buffer
	zw := gzip.NewWriter(&body)
	enc := json.NewEncoder(zw)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			return nil, fmt.Errorf("outbox: encode archive row: %w", err)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("outbox: close archive: %w", err)
	}
	return &body, nil
}

// archiveKey builds outbox-archive/<site>/<yyyy>/<mm>/<first-id>_<last-id>.ndjson.gz.
func archiveKey(site string, created time.Time, rows []archiveRow) string {
	first, _ := rows[0]["id"].(string)
	last, _ := rows[len(rows)-1]["id"].(string)
	created = created.UTC()
	return fmt.Sprintf("outbox-archive/%s/%04d/%02d/%s_%s.ndjson.gz", site, created.Year(), int(created.Month()), first, last)
}

// deleteArchived removes receipts and then the outbox rows in one transaction.
// The guard repeats the published and unreferenced checks. A count mismatch
// rolls the transaction back, so no receipt goes unless its row goes too.
func (a *Archiver) deleteArchived(ctx context.Context, site string, rows []archiveRow) error {
	marks := make([]string, len(rows))
	ids := make([]any, len(rows))
	for i, row := range rows {
		marks[i] = "?"
		ids[i] = row["id"]
	}
	in := strings.Join(marks, ",")
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("outbox: begin archive delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit
	if _, err = tx.ExecContext(ctx, db.Rebind(a.Dialect, `DELETE FROM _kora_outbox_receipt WHERE site = ? AND event_id IN (`+in+`)`), append([]any{site}, ids...)...); err != nil {
		return fmt.Errorf("outbox: delete archived receipts: %w", err)
	}
	result, err := tx.ExecContext(ctx, db.Rebind(a.Dialect, `DELETE FROM _kora_outbox WHERE site = ? AND status = ? AND id IN (`+in+`) AND NOT EXISTS (SELECT 1 FROM _kora_outbox_delivery_failure f WHERE f.site = _kora_outbox.site AND f.event_id = _kora_outbox.id)`), append([]any{site, string(StatusPublished)}, ids...)...)
	if err != nil {
		return fmt.Errorf("outbox: delete archived rows: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("outbox: count archived rows: %w", err)
	}
	if n != int64(len(rows)) {
		return fmt.Errorf("outbox: archive delete removed %d of %d rows; rows changed during archive", n, len(rows))
	}
	return tx.Commit()
}
