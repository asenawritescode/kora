// Package outbox implements the transactional outbox for durable event delivery
// (RFC §8.1). Business writes record an event into _kora_outbox in the same SQL
// transaction as the business effect; a separate publisher claims, publishes, and
// marks rows so a crash between SQL commit and provider publish cannot lose an
// event.
package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/eventstream"
	"github.com/oklog/ulid/v2"
)

// Status is the durable state of an outbox row (RFC §8.1).
type Status string

const (
	StatusPending    Status = "pending"
	StatusPublishing Status = "publishing"
	StatusPublished  Status = "published"
	StatusFailed     Status = "failed"
)

const (
	DefaultBatchSize     = 100
	MaxBatchSize         = 500
	MaxEventPayloadBytes = 16 << 20
	// MaxDispatchBatchBytes bounds the retained batch payload. One additional
	// maximum-size row may be transiently scanned before the dispatcher stops
	// the result iteration and closes its cursor.
	MaxDispatchBatchBytes = 48 << 20
	BacklogSampleLimit    = 256
)

var ErrEventPayloadTooLarge = errors.New("outbox: event payload exceeds the 16 MiB limit")

// ErrSourceSequenceExhausted reports that a tenant exhausted its signed
// database sequence range. Allocation fails closed instead of wrapping.
var ErrSourceSequenceExhausted = errors.New("outbox: tenant source sequence exhausted its signed BIGINT range")

const maxSourceSequence int64 = 1<<63 - 1

func boundedBatchLimit(limit int) (int, error) {
	if limit <= 0 {
		return DefaultBatchSize, nil
	}
	if limit > MaxBatchSize {
		return 0, fmt.Errorf("outbox: batch limit %d exceeds maximum %d", limit, MaxBatchSize)
	}
	return limit, nil
}

// Writer records events into the outbox within an existing transaction. The
// business transaction and the outbox write must share the same *sql.Tx so they
// commit atomically.
type Writer interface {
	// Append writes an event into the outbox using the given transaction.
	Append(ctx context.Context, tx *sql.Tx, event contract.EventEnvelope) error
}

// SQLWriter is the default Writer implementation. It receives the site's SQL
// dialect so its placeholders match the active database driver.
type SQLWriter struct{ Dialect db.QueryDialect }

// NewSQLWriter returns a Writer backed by _kora_outbox.
func NewSQLWriter(dialect db.QueryDialect) Writer { return &SQLWriter{Dialect: dialect} }

// Append inserts an event row. The event ID is the outbox row primary key and is
// later reused as the provider message ID so duplicate publishes are detectable.
func (w *SQLWriter) Append(ctx context.Context, tx *sql.Tx, event contract.EventEnvelope) error {
	if event.ID == "" {
		event.ID = contract.NewEventID()
	}
	if event.Version == 0 {
		event.Version = contract.CurrentVersion
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	// Size against the maximum signed BIGINT sequence before taking the tenant
	// sequence lock. The exact sequence assigned below can only be shorter, so
	// the final payload cannot cross the cap after allocating sequence state.
	event.Sequence = contract.Revision(strconv.FormatInt(maxSourceSequence, 10))
	if err := event.Validate(); err != nil {
		return fmt.Errorf("outbox: validate event: %w", err)
	}
	preflight, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("outbox: marshal event: %w", err)
	}
	if len(preflight) > MaxEventPayloadBytes {
		return ErrEventPayloadTooLarge
	}
	event.Sequence = ""
	sequence, err := nextSourceSequence(ctx, tx, w.Dialect)
	if err != nil {
		return fmt.Errorf("outbox: allocate tenant source sequence: %w", err)
	}
	event.Sequence = contract.Revision(strconv.FormatInt(sequence, 10))
	if err := event.Validate(); err != nil {
		return fmt.Errorf("outbox: validate event: %w", err)
	}

	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("outbox: marshal event: %w", err)
	}
	if len(payload) > MaxEventPayloadBytes {
		return ErrEventPayloadTooLarge
	}

	_, err = tx.ExecContext(ctx, db.Rebind(w.Dialect,
		`INSERT INTO _kora_outbox
			(id, site, source_sequence, event_type, event_version, aggregate_type, aggregate_id, payload, status, attempts, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	),
		event.ID,
		event.Site,
		sequence,
		event.Type,
		event.Version,
		event.AggregateType,
		event.AggregateID,
		string(payload),
		string(StatusPending),
		0,
		event.OccurredAt,
	)
	if err != nil {
		return fmt.Errorf("outbox: insert event: %w", err)
	}
	return nil
}

func nextSourceSequence(ctx context.Context, tx *sql.Tx, dialect db.QueryDialect) (int64, error) {
	if ctx == nil || tx == nil || dialect == nil {
		return 0, fmt.Errorf("source sequence requires context, transaction, and dialect")
	}
	driver := "mysql"
	if named, ok := dialect.(interface{ DriverName() string }); ok {
		driver = named.DriverName()
	}
	insert := `INSERT INTO _kora_tenant_event_sequence (id, last_sequence) VALUES (1, 0) ON DUPLICATE KEY UPDATE id = VALUES(id)`
	if driver == "postgres" || driver == "libsql" {
		insert = `INSERT INTO _kora_tenant_event_sequence (id, last_sequence) VALUES (1, 0) ON CONFLICT (id) DO NOTHING`
	}
	if _, err := tx.ExecContext(ctx, db.Rebind(dialect, insert)); err != nil {
		return 0, err
	}
	selectSequence := `SELECT last_sequence FROM _kora_tenant_event_sequence WHERE id = 1 FOR UPDATE`
	if driver == "libsql" {
		// LibSQL's HTTP transaction adapter serializes write transactions; it
		// does not accept server-side SELECT FOR UPDATE syntax.
		selectSequence = `SELECT last_sequence FROM _kora_tenant_event_sequence WHERE id = 1`
	}
	var current, legacyMax int64
	if err := tx.QueryRowContext(ctx, db.Rebind(dialect, selectSequence)).Scan(&current); err != nil {
		return 0, err
	}
	if err := tx.QueryRowContext(ctx, db.Rebind(dialect,
		`SELECT COALESCE(MAX(last_sequence), 0) FROM _kora_event_sequence`)).Scan(&legacyMax); err != nil {
		return 0, err
	}
	if current < 0 || legacyMax < 0 {
		return 0, fmt.Errorf("source sequence contains a negative value (current=%d legacy=%d)", current, legacyMax)
	}
	base := current
	if legacyMax > base {
		base = legacyMax
	}
	if base == maxSourceSequence {
		return 0, ErrSourceSequenceExhausted
	}
	next := base + 1
	if _, err := tx.ExecContext(ctx, db.Rebind(dialect,
		`UPDATE _kora_tenant_event_sequence SET last_sequence = ? WHERE id = 1`), next); err != nil {
		return 0, err
	}
	return next, nil
}

// Publisher claims pending rows, publishes them through a contract.EventPublisher,
// and marks them published (or failed). It is safe to run from multiple processes
// because claims use a lease. Rows are never deleted automatically.
type Publisher struct {
	DB            *sql.DB
	Dialect       db.QueryDialect
	Destination   contract.EventPublisher
	WorkAdmission interface {
		Acquire(context.Context) (func(), error)
	}
	LeaseOwner  string
	LeaseTTL    time.Duration
	MaxAttempts int
}

// ErrLeaseLost identifies a fenced outbox claim whose owner no longer controls
// the durable row. Callers may count this separately from SQL/provider errors.
var ErrLeaseLost = errors.New("outbox: claim lost")

// NewPublisher returns a Publisher with sensible defaults.
func NewPublisher(database *sql.DB, dest contract.EventPublisher, dialect db.QueryDialect) *Publisher {
	return &Publisher{
		DB:          database,
		Dialect:     dialect,
		Destination: dest,
		LeaseOwner:  ulid.Make().String(),
		LeaseTTL:    30 * time.Second,
		MaxAttempts: 8,
	}
}

// PublishDue claims up to limit due rows and publishes them. It returns the
// number of rows published.
func (p *Publisher) PublishDue(ctx context.Context, limit int) (int, error) {
	bounded, err := boundedBatchLimit(limit)
	if err != nil {
		return 0, err
	}
	limit = bounded
	maxAttempts := p.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 8
	}
	now := time.Now().UTC()
	leaseUntil := now.Add(p.LeaseTTL)

	// Select due row IDs first (LIMIT in SELECT is portable across MySQL, LibSQL,
	// and Postgres), then claim each row by primary key. This avoids LIMIT in an
	// UPDATE, which SQLite/LibSQL do not support.
	releaseSelect, err := p.acquireWork(ctx)
	if err != nil {
		return 0, fmt.Errorf("outbox: admit due-row selection: %w", err)
	}
	rows, err := p.DB.QueryContext(ctx, db.Rebind(p.Dialect,
		`SELECT id FROM _kora_outbox
		 WHERE (status = 'pending' AND (next_attempt_at IS NULL OR next_attempt_at <= ?))
			OR (status = 'publishing' AND lease_until IS NOT NULL AND lease_until < ?)
		 ORDER BY created_at
		 LIMIT ?`),
		now, now, limit)
	if err != nil {
		releaseSelect()
		return 0, fmt.Errorf("outbox: select due rows: %w", err)
	}

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			releaseSelect()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		releaseSelect()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		releaseSelect()
		return 0, fmt.Errorf("outbox: close due-row selection: %w", err)
	}
	releaseSelect()

	if len(ids) == 0 {
		return 0, nil
	}

	// Claim each due row. Rows that another worker claimed in between are skipped.
	published := 0
	var batchErr error
	for _, id := range ids {
		claimOwner := p.LeaseOwner + ":" + ulid.Make().String()
		releaseClaim, err := p.acquireWork(ctx)
		if err != nil {
			batchErr = errors.Join(batchErr, fmt.Errorf("outbox: admit claim for event %s: %w", id, err))
			continue
		}
		res, err := p.DB.ExecContext(ctx, db.Rebind(p.Dialect,
			`UPDATE _kora_outbox
			 SET status = ?, lease_owner = ?, lease_until = ?, attempts = attempts + 1
			 WHERE id = ?
			   AND (
			       (status = 'pending' AND (next_attempt_at IS NULL OR next_attempt_at <= ?))
			       OR (status = 'publishing' AND lease_until IS NOT NULL AND lease_until < ?)
			   )`),
			string(StatusPublishing), claimOwner, leaseUntil, id, now, now)
		if err != nil {
			releaseClaim()
			batchErr = errors.Join(batchErr, fmt.Errorf("outbox: claim event %s: %w", id, err))
			continue // leave the durable event for another worker
		}
		affected, err := res.RowsAffected()
		releaseClaim()
		if err != nil {
			batchErr = errors.Join(batchErr, fmt.Errorf("outbox: check claim for event %s: %w", id, err))
			continue
		}
		if affected == 0 {
			continue // already claimed by another worker
		}

		releaseAttempts, err := p.acquireWork(ctx)
		if err != nil {
			batchErr = errors.Join(batchErr, fmt.Errorf("outbox: admit attempt lookup for event %s: %w", id, err))
			continue
		}
		var attempts int
		err = p.DB.QueryRowContext(ctx,
			db.Rebind(p.Dialect, `SELECT attempts FROM _kora_outbox WHERE id = ? AND status = 'publishing' AND lease_owner = ?`), id, claimOwner).Scan(&attempts)
		releaseAttempts()
		if err != nil {
			batchErr = errors.Join(batchErr, fmt.Errorf("outbox: read claimed attempts for event %s: %w", id, err))
			continue
		}

		if err := p.publishOne(ctx, id); err != nil {
			var persistErr error
			if attempts >= maxAttempts {
				persistErr = p.markFailed(ctx, id, claimOwner, err)
			} else {
				persistErr = p.retry(ctx, id, claimOwner, attempts, err)
			}
			if persistErr != nil {
				batchErr = errors.Join(batchErr, fmt.Errorf("outbox: persist delivery failure for event %s: %w", id, persistErr))
			}
			continue
		}
		if err := p.markPublished(ctx, id, claimOwner); err != nil {
			batchErr = errors.Join(batchErr, fmt.Errorf("outbox: acknowledge published event %s: %w", id, err))
			continue
		}
		published++
	}

	return published, batchErr
}

func (p *Publisher) publishOne(ctx context.Context, id string) error {
	release, err := p.acquireWork(ctx)
	if err != nil {
		return fmt.Errorf("outbox: admit payload lookup for event %s: %w", id, err)
	}
	var payload string
	var sequence int64
	err = p.DB.QueryRowContext(ctx,
		db.Rebind(p.Dialect, `SELECT payload, source_sequence FROM _kora_outbox WHERE id = ?`), id).Scan(&payload, &sequence)
	release()
	if err != nil {
		return err
	}

	var event contract.EventEnvelope
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return fmt.Errorf("outbox: unmarshal event %s: %w", id, err)
	}
	if err := applyStoredSourceSequence(&event, sequence); err != nil {
		return fmt.Errorf("outbox: restore source sequence for event %s: %w", id, err)
	}
	// Provider calls happen after releasing the database connection and work
	// permit; the durable claim lease handles interruption and retry.
	return p.Destination.Publish(ctx, event)
}

func (p *Publisher) markPublished(ctx context.Context, id, claimOwner string) error {
	release, err := p.acquireWork(ctx)
	if err != nil {
		return fmt.Errorf("outbox: admit publication acknowledgment for event %s: %w", id, err)
	}
	defer release()
	res, err := p.DB.ExecContext(ctx, db.Rebind(p.Dialect,
		`UPDATE _kora_outbox SET status = ?, published_at = ?, lease_owner = '', lease_until = NULL WHERE id = ? AND status = 'publishing' AND lease_owner = ?`),
		string(StatusPublished), time.Now().UTC(), id, claimOwner)
	return requireClaimTransition(res, err, id)
}

func (p *Publisher) retry(ctx context.Context, id, claimOwner string, attempts int, cause error) error {
	release, err := p.acquireWork(ctx)
	if err != nil {
		return fmt.Errorf("outbox: admit retry transition for event %s: %w", id, err)
	}
	defer release()
	res, err := p.DB.ExecContext(ctx, db.Rebind(p.Dialect,
		`UPDATE _kora_outbox SET status = 'pending', last_error = ?, lease_owner = '', lease_until = NULL,
			next_attempt_at = ? WHERE id = ? AND status = 'publishing' AND lease_owner = ?`),
		cause.Error(), time.Now().UTC().Add(backoff(attempts)), id, claimOwner)
	return requireClaimTransition(res, err, id)
}

func (p *Publisher) markFailed(ctx context.Context, id, claimOwner string, cause error) error {
	release, err := p.acquireWork(ctx)
	if err != nil {
		return fmt.Errorf("outbox: admit failed transition for event %s: %w", id, err)
	}
	defer release()
	res, err := p.DB.ExecContext(ctx, db.Rebind(p.Dialect,
		`UPDATE _kora_outbox SET status = ?, last_error = ?, lease_owner = '', lease_until = NULL WHERE id = ? AND status = 'publishing' AND lease_owner = ?`),
		string(StatusFailed), cause.Error(), id, claimOwner)
	return requireClaimTransition(res, err, id)
}

func (p *Publisher) acquireWork(ctx context.Context) (func(), error) {
	if p.WorkAdmission == nil {
		return func() {}, nil
	}
	return p.WorkAdmission.Acquire(ctx)
}

func requireClaimTransition(result sql.Result, err error, id string) error {
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("%w before state update for event %s", ErrLeaseLost, id)
	}
	return nil
}

// ReplayFailed resets failed rows to pending so they are retried (operator replay).
func (p *Publisher) ReplayFailed(ctx context.Context) (int, error) {
	release, err := p.acquireWork(ctx)
	if err != nil {
		return 0, fmt.Errorf("outbox: admit failed-event replay: %w", err)
	}
	defer release()
	res, err := p.DB.ExecContext(ctx, db.Rebind(p.Dialect,
		`UPDATE _kora_outbox SET status = 'pending', next_attempt_at = NULL, last_error = NULL
		 WHERE status = 'failed' AND attempts < ?`),
		p.MaxAttempts)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Consumer is a named receiver of outbox events. Transactional local consumers
// reserve their unique receipt before effects in the same transaction. The
// legacy external worker path writes receipts after effects and remains
// at-least-once across a crash between those operations.
type Consumer struct {
	DB      *sql.DB
	Name    string
	Dialect db.QueryDialect
}

// RecordReceiptTx reserves a consumer receipt in the caller's transaction.
// Transactional workers call this before applying local effects so the unique
// receipt and those effects either commit together or both roll back.
func (c *Consumer) RecordReceiptTx(ctx context.Context, tx *sql.Tx, eventID, site string) (bool, error) {
	return c.RecordReceiptSequenceTx(ctx, tx, eventID, site, "")
}

// RecordReceiptSequenceTx atomically records the consumer receipt together
// with the source sequence from the delivered outbox row. Old callers that do
// not have a sequence remain source-compatible and store NULL.
func (c *Consumer) RecordReceiptSequenceTx(ctx context.Context, tx *sql.Tx, eventID, site, sourceSequence string) (bool, error) {
	if err := validateReceiptInput(c, ctx, eventID, site); err != nil {
		return false, err
	}
	if tx == nil {
		return false, fmt.Errorf("outbox: transactional receipt requires a transaction")
	}
	sequence, err := parseReceiptSequence(sourceSequence)
	if err != nil {
		return false, err
	}
	query := `INSERT INTO _kora_outbox_receipt (consumer_name, event_id, site, source_sequence, received_at) VALUES (?, ?, ?, ?, ?)`
	if driver, ok := c.Dialect.(interface{ DriverName() string }); ok {
		switch driver.DriverName() {
		case "mysql":
			query = `INSERT IGNORE INTO _kora_outbox_receipt (consumer_name, event_id, site, source_sequence, received_at) VALUES (?, ?, ?, ?, ?)`
		case "postgres", "libsql":
			query += ` ON CONFLICT (consumer_name, event_id) DO NOTHING`
		}
	}
	result, err := tx.ExecContext(ctx, db.Rebind(c.Dialect, query), c.Name, eventID, site, sequence, time.Now().UTC())
	if err != nil {
		return false, fmt.Errorf("outbox: reserve transactional receipt: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("outbox: inspect transactional receipt: %w", err)
	}
	return rows == 1, nil
}

// NewConsumer returns a named consumer for idempotent outbox delivery.
func NewConsumer(database *sql.DB, name string, dialect db.QueryDialect) *Consumer {
	return &Consumer{DB: database, Name: name, Dialect: dialect}
}

// HasSeen reports whether the consumer has already recorded a receipt for eventID.
func (c *Consumer) HasSeen(ctx context.Context, eventID string) (bool, error) {
	var n int
	if err := c.DB.QueryRowContext(ctx, db.Rebind(c.Dialect,
		`SELECT COUNT(*) FROM _kora_outbox_receipt WHERE consumer_name = ? AND event_id = ?`),
		c.Name, eventID).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// RecordReceipt records a (consumer_name, event_id) receipt idempotently. The
// unique primary key makes a concurrent duplicate insert a no-op for MySQL and
// LibSQL (the error is swallowed by the caller as a dedupe signal).
func (c *Consumer) RecordReceipt(ctx context.Context, eventID, site string) error {
	return c.RecordReceiptSequence(ctx, eventID, site, "")
}

// RecordReceiptSequence preserves source ordering for successful at-least-once
// deliveries while retaining event-ID deduplication.
func (c *Consumer) RecordReceiptSequence(ctx context.Context, eventID, site, sourceSequence string) error {
	if err := validateReceiptInput(c, ctx, eventID, site); err != nil {
		return err
	}
	sequence, err := parseReceiptSequence(sourceSequence)
	if err != nil {
		return err
	}
	_, err = c.DB.ExecContext(ctx, db.Rebind(c.Dialect,
		`INSERT INTO _kora_outbox_receipt (consumer_name, event_id, site, source_sequence, received_at)
		 VALUES (?, ?, ?, ?, ?)`),
		c.Name, eventID, site, sequence, time.Now().UTC())
	return err
}

func parseReceiptSequence(raw string) (any, error) {
	if raw == "" {
		return nil, nil
	}
	sequence, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || sequence < 0 || strconv.FormatInt(sequence, 10) != raw {
		return nil, fmt.Errorf("outbox: invalid source sequence %q for consumer receipt", raw)
	}
	return sequence, nil
}

func applyStoredSourceSequence(event *contract.EventEnvelope, sourceSequence int64) error {
	if event == nil {
		return fmt.Errorf("outbox: event is required")
	}
	if sourceSequence < 0 {
		return fmt.Errorf("outbox: negative persisted source sequence %d", sourceSequence)
	}
	if sourceSequence > 0 {
		// The outbox column is allocated in the same transaction as this row and
		// is the durable ordering authority. Repair stale envelope metadata from
		// it before local consumers record receipts or external providers see it.
		event.Sequence = contract.Revision(strconv.FormatInt(sourceSequence, 10))
		return nil
	}
	if event.Sequence == "" {
		return nil
	}
	if _, err := parseReceiptSequence(string(event.Sequence)); err != nil {
		return err
	}
	return nil
}

func validateReceiptInput(c *Consumer, ctx context.Context, eventID, site string) error {
	if ctx == nil || c == nil || c.DB == nil || c.Dialect == nil || strings.TrimSpace(c.Name) == "" || eventID == "" {
		return fmt.Errorf("outbox: receipt requires context, database, dialect and identity")
	}
	if len(c.Name) > 140 || len(eventID) > 255 || len(site) > 140 || !utf8.ValidString(c.Name) || !utf8.ValidString(eventID) || !utf8.ValidString(site) {
		return fmt.Errorf("outbox: receipt identity exceeds schema limits")
	}
	return nil
}

// OutboxMetrics is an operational snapshot of outbox health (RFC §8.1).
type OutboxMetrics struct {
	Pending   int `json:"pending"`
	Failed    int `json:"failed"`
	Published int `json:"published"`
}

// Metrics returns outbox health counters for operator visibility.
func (p *Publisher) Metrics(ctx context.Context) (OutboxMetrics, error) {
	release, err := p.acquireWork(ctx)
	if err != nil {
		return OutboxMetrics{}, fmt.Errorf("outbox: admit metrics query: %w", err)
	}
	defer release()
	var m OutboxMetrics
	err = p.DB.QueryRowContext(ctx, db.Rebind(p.Dialect,
		`SELECT
		  COALESCE(SUM(CASE WHEN status = 'pending' THEN 1 ELSE 0 END), 0),
		  COALESCE(SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END), 0),
		  COALESCE(SUM(CASE WHEN status = 'published' THEN 1 ELSE 0 END), 0)
		 FROM _kora_outbox`)).Scan(&m.Pending, &m.Failed, &m.Published)
	return m, err
}

// BacklogSample is a bounded lower-bound sample of the unpublished backlog.
// Saturated means at least BacklogSampleLimit rows exist in a state.
type BacklogSample struct {
	Pending              int64
	Failed               int64
	Publishing           int64
	OldestUnpublishedAge time.Duration
	Saturated            bool
}

// Backlog returns at most BacklogSampleLimit indexed timestamp rows for each
// unfinished state, avoiding a table scan during periodic resource sampling.
func (p *Publisher) Backlog(ctx context.Context) (BacklogSample, error) {
	if ctx == nil || p == nil || p.DB == nil || p.Dialect == nil {
		return BacklogSample{}, errors.New("outbox: backlog sample requires context, database and dialect")
	}
	var sample BacklogSample
	states := []struct {
		name  string
		count *int64
	}{{"pending", &sample.Pending}, {"failed", &sample.Failed}, {"publishing", &sample.Publishing}}
	for _, state := range states {
		release, err := p.acquireWork(ctx)
		if err != nil {
			return BacklogSample{}, fmt.Errorf("outbox: admit %s backlog sample: %w", state.name, err)
		}
		rows, err := p.DB.QueryContext(ctx, db.Rebind(p.Dialect,
			`SELECT created_at FROM _kora_outbox WHERE status = ? ORDER BY created_at ASC LIMIT ?`),
			state.name, BacklogSampleLimit)
		if err != nil {
			release()
			return BacklogSample{}, fmt.Errorf("outbox: sample %s backlog: %w", state.name, err)
		}
		var oldest time.Time
		for rows.Next() {
			var created db.Timestamp
			if err := rows.Scan(&created); err != nil {
				_ = rows.Close()
				release()
				return BacklogSample{}, fmt.Errorf("outbox: scan %s backlog: %w", state.name, err)
			}
			if *state.count == 0 {
				oldest = created.Time
			}
			(*state.count)++
		}
		rowsErr := rows.Err()
		closeErr := rows.Close()
		release()
		if rowsErr != nil {
			return BacklogSample{}, fmt.Errorf("outbox: iterate %s backlog: %w", state.name, rowsErr)
		}
		if closeErr != nil {
			return BacklogSample{}, fmt.Errorf("outbox: close %s backlog: %w", state.name, closeErr)
		}
		if *state.count >= BacklogSampleLimit {
			sample.Saturated = true
		}
		if !oldest.IsZero() {
			age := time.Since(oldest)
			if age < 0 {
				age = 0
			}
			if age > sample.OldestUnpublishedAge {
				sample.OldestUnpublishedAge = age
			}
		}
	}
	return sample, nil
}

// BacklogSnapshot is a cached, low-cardinality view of one tenant outbox.
type BacklogSnapshot struct {
	Pending              int64
	Failed               int64
	Publishing           int64
	OldestUnpublishedAge time.Duration
	Saturated            bool
	RefreshFailures      int64
	LeaseLost            int64
	RefreshedAt          time.Time
}

// BacklogMetrics stores the last admission-protected outbox SQL sample. Metric
// collection reads this snapshot and never performs database I/O.
type BacklogMetrics struct {
	mu       sync.RWMutex
	snapshot BacklogSnapshot
}

func (m *BacklogMetrics) Refresh(ctx context.Context, publisher *Publisher) error {
	if m == nil || publisher == nil || publisher.DB == nil || publisher.Dialect == nil {
		return errors.New("outbox: backlog metrics require a configured publisher")
	}
	metrics, err := publisher.Backlog(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.snapshot.RefreshFailures++
		return err
	}
	failures := m.snapshot.RefreshFailures
	leaseLost := m.snapshot.LeaseLost
	m.snapshot = BacklogSnapshot{
		Pending: metrics.Pending, Failed: metrics.Failed, Publishing: metrics.Publishing,
		OldestUnpublishedAge: metrics.OldestUnpublishedAge, Saturated: metrics.Saturated,
		RefreshFailures: failures, LeaseLost: leaseLost, RefreshedAt: time.Now().UTC(),
	}
	return nil
}

// RecordLeaseLost records a publisher claim fenced by a newer owner.
func (m *BacklogMetrics) RecordLeaseLost() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.snapshot.LeaseLost++
	m.mu.Unlock()
}

func (m *BacklogMetrics) Snapshot(now time.Time) BacklogSnapshot {
	if m == nil {
		return BacklogSnapshot{}
	}
	m.mu.RLock()
	snapshot := m.snapshot
	m.mu.RUnlock()
	if !snapshot.RefreshedAt.IsZero() && snapshot.OldestUnpublishedAge > 0 {
		snapshot.OldestUnpublishedAge += now.Sub(snapshot.RefreshedAt)
		if snapshot.OldestUnpublishedAge < 0 {
			snapshot.OldestUnpublishedAge = 0
		}
	}
	return snapshot
}

// Worker is the consumer-side processing function for a named consumer. It
// receives a single event and returns an error to signal a failed (retryable)
// effect. Receipting is handled by Dispatcher.Route, not the worker.
type Worker func(ctx context.Context, event contract.EventEnvelope) error

// TransactionalWorker applies local database effects in the same transaction
// as its consumer receipt. It must use tx for every coordinated local effect.
type TransactionalWorker func(ctx context.Context, tx *sql.Tx, event contract.EventEnvelope) error

// Dispatcher delivers outbox events to local consumers. Receipts represent
// consumer completion; only Publisher changes publication status after the
// destination acknowledges the event.
type Dispatcher struct {
	DB            *sql.DB
	Dialect       db.QueryDialect
	WorkAdmission interface {
		Acquire(context.Context) (func(), error)
	}
	Workers              map[string]Worker
	WorkerTypes          map[string]map[string]struct{}
	WorkerPatterns       map[string][]string
	TransactionalWorkers map[string]TransactionalWorker
	TransactionalTypes   map[string]map[string]struct{}
	registrationErr      error
}

// NewDispatcher returns a Dispatcher with no workers registered.
func NewDispatcher(database *sql.DB, dialect db.QueryDialect) *Dispatcher {
	return &Dispatcher{DB: database, Dialect: dialect, Workers: make(map[string]Worker), WorkerTypes: make(map[string]map[string]struct{}), WorkerPatterns: make(map[string][]string), TransactionalWorkers: make(map[string]TransactionalWorker), TransactionalTypes: make(map[string]map[string]struct{})}
}

// Register adds a named worker.
func (d *Dispatcher) Register(name string, w Worker) {
	if d == nil || name == "" || w == nil {
		if d != nil && d.registrationErr == nil {
			d.registrationErr = fmt.Errorf("outbox: worker requires dispatcher, name and handler")
		}
		return
	}
	if _, exists := d.TransactionalWorkers[name]; exists {
		d.registrationErr = fmt.Errorf("outbox: consumer %q is already registered", name)
		return
	}
	if _, exists := d.Workers[name]; exists {
		d.registrationErr = fmt.Errorf("outbox: consumer %q is already registered", name)
		return
	}
	if d.Workers == nil {
		d.Workers = make(map[string]Worker)
	}
	d.Workers[name] = w
}

// RegisterForTypes registers an at-least-once local consumer for an explicit
// set of event types. It prevents unrelated command/system events from
// accumulating receipts for consumers that cannot handle them.
func (d *Dispatcher) RegisterForTypes(name string, eventTypes []string, worker Worker) error {
	if len(eventTypes) == 0 || len(eventTypes) > 100 {
		return fmt.Errorf("outbox: filtered consumer requires 1 to 100 event types")
	}
	allowed := make(map[string]struct{}, len(eventTypes))
	for _, eventType := range eventTypes {
		if eventType == "" || len(eventType) > 255 {
			return fmt.Errorf("outbox: filtered consumer event type must be 1 to 255 bytes")
		}
		if _, exists := allowed[eventType]; exists {
			return fmt.Errorf("outbox: filtered consumer event types must be unique")
		}
		allowed[eventType] = struct{}{}
	}
	if d == nil || name == "" || worker == nil {
		return fmt.Errorf("outbox: filtered consumer requires dispatcher, name and handler")
	}
	d.Register(name, worker)
	if d.registrationErr != nil {
		return d.registrationErr
	}
	if d.WorkerTypes == nil {
		d.WorkerTypes = make(map[string]map[string]struct{})
	}
	d.WorkerTypes[name] = allowed
	return nil
}

// RegisterForPatterns registers a consumer for bounded SQL LIKE patterns.
// Patterns are parameters, never interpolated SQL; intended for canonical
// event families whose concrete aggregate names are tenant-defined.
func (d *Dispatcher) RegisterForPatterns(name string, patterns []string, worker Worker) error {
	if len(patterns) == 0 || len(patterns) > 16 {
		return fmt.Errorf("outbox: patterned consumer requires 1 to 16 patterns")
	}
	for _, pattern := range patterns {
		if pattern == "" || len(pattern) > 255 {
			return fmt.Errorf("outbox: event pattern must be 1 to 255 bytes")
		}
	}
	if d == nil || name == "" || worker == nil {
		return fmt.Errorf("outbox: patterned consumer requires dispatcher, name and handler")
	}
	d.Register(name, worker)
	if d.registrationErr != nil {
		return d.registrationErr
	}
	if d.WorkerPatterns == nil {
		d.WorkerPatterns = make(map[string][]string)
	}
	d.WorkerPatterns[name] = append([]string(nil), patterns...)
	return nil
}

// RegisterTransactional registers a local consumer whose receipt and effects
// are committed atomically. A name cannot be registered in both modes.
func (d *Dispatcher) RegisterTransactional(name string, w TransactionalWorker) error {
	if d == nil || name == "" || w == nil {
		return fmt.Errorf("outbox: transactional worker requires dispatcher, name and handler")
	}
	if _, exists := d.Workers[name]; exists {
		return fmt.Errorf("outbox: consumer %q is already registered", name)
	}
	if _, exists := d.TransactionalWorkers[name]; exists {
		return fmt.Errorf("outbox: consumer %q is already registered", name)
	}
	if d.TransactionalWorkers == nil {
		d.TransactionalWorkers = make(map[string]TransactionalWorker)
	}
	d.TransactionalWorkers[name] = w
	return nil
}

// RegisterTransactionalForTypes limits a local consumer to an explicit event
// type allowlist. Unmatched events receive no consumer receipt, so they do not
// grow irrelevant receipt rows or block other outbox consumers.
func (d *Dispatcher) RegisterTransactionalForTypes(name string, eventTypes []string, w TransactionalWorker) error {
	if len(eventTypes) == 0 || len(eventTypes) > 100 {
		return fmt.Errorf("outbox: filtered consumer requires 1 to 100 event types")
	}
	allowed := make(map[string]struct{}, len(eventTypes))
	for _, eventType := range eventTypes {
		if eventType == "" || len(eventType) > 160 {
			return fmt.Errorf("outbox: filtered consumer event type must be 1 to 160 bytes")
		}
		if _, exists := allowed[eventType]; exists {
			return fmt.Errorf("outbox: filtered consumer event types must be unique")
		}
		allowed[eventType] = struct{}{}
	}
	if err := d.RegisterTransactional(name, w); err != nil {
		return err
	}
	if d.TransactionalTypes == nil {
		d.TransactionalTypes = make(map[string]map[string]struct{})
	}
	d.TransactionalTypes[name] = allowed
	return nil
}

// Run processes up to limit events that lack a receipt for at least one
// applicable consumer. It never acknowledges external publication. A
// Transactional consumer reserves its receipt with the local effect.
func (d *Dispatcher) Run(ctx context.Context, limit int) (int, error) {
	if d == nil || d.DB == nil || d.Dialect == nil {
		return 0, fmt.Errorf("outbox: dispatcher requires database and dialect")
	}
	if d.registrationErr != nil {
		return 0, d.registrationErr
	}
	bounded, err := boundedBatchLimit(limit)
	if err != nil {
		return 0, err
	}
	limit = bounded
	query, queryArgs, ok := d.consumerQuery(limit)
	if !ok {
		return 0, nil
	}

	releaseSelect, err := d.acquireWork(ctx)
	if err != nil {
		return 0, fmt.Errorf("outbox: admit local dispatch selection: %w", err)
	}
	rows, err := d.DB.QueryContext(ctx, db.Rebind(d.Dialect, query), queryArgs...)
	if err != nil {
		releaseSelect()
		return 0, fmt.Errorf("outbox: select due events: %w", err)
	}
	type pending struct {
		id    string
		event contract.EventEnvelope
	}
	batch := make([]pending, 0, limit)
	var batchBytes int
	for rows.Next() {
		var id, payload string
		var sourceSequence int64
		if err := rows.Scan(&id, &payload, &sourceSequence); err != nil {
			_ = rows.Close()
			releaseSelect()
			return 0, fmt.Errorf("outbox: scan event for local consumers: %w", err)
		}
		var event contract.EventEnvelope
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			_ = rows.Close()
			releaseSelect()
			return 0, fmt.Errorf("outbox: decode event %s for local consumers: %w", id, err)
		}
		if err := applyStoredSourceSequence(&event, sourceSequence); err != nil {
			_ = rows.Close()
			releaseSelect()
			return 0, fmt.Errorf("outbox: restore source sequence for event %s: %w", id, err)
		}
		if len(payload) > MaxEventPayloadBytes {
			_ = rows.Close()
			releaseSelect()
			return 0, fmt.Errorf("outbox: event %s exceeds the %d-byte dispatch payload limit", id, MaxEventPayloadBytes)
		}
		if batchBytes+len(payload) > MaxDispatchBatchBytes && len(batch) > 0 {
			// The row remains unreceipted and will be selected on the next pass.
			break
		}
		batch = append(batch, pending{id: id, event: event})
		batchBytes += len(payload)
		if batchBytes >= MaxDispatchBatchBytes {
			break
		}
	}
	rowsErr := rows.Err()
	closeErr := rows.Close()
	releaseSelect()
	if rowsErr != nil || closeErr != nil {
		return 0, errors.Join(rowsErr, closeErr)
	}

	processed := 0
	for _, item := range batch {
		id, event := item.id, item.event

		allOK := true
		for name, worker := range d.Workers {
			if !d.matchesWorkerEvent(name, event.Type) {
				continue
			}
			c := &Consumer{DB: d.DB, Name: name, Dialect: d.Dialect}
			release, err := d.acquireWork(ctx)
			if err != nil {
				allOK = false
				continue
			}
			seen, err := c.HasSeen(ctx, id)
			release()
			if err != nil {
				allOK = false
				continue
			}
			if seen {
				continue // already processed by this consumer
			}
			if err := worker(ctx, event); err != nil {
				allOK = false
				continue
			}
			// Record the receipt after a successful effect; a crash between the
			// effect and the receipt means a rare duplicate effect, which is the
			// documented at-least-once tradeoff (idempotency keys further harden it).
			release, err = d.acquireWork(ctx)
			if err != nil {
				allOK = false
				continue
			}
			err = c.RecordReceiptSequence(ctx, id, event.Site, string(event.Sequence))
			release()
			if err != nil {
				allOK = false
			}
		}
		for name, worker := range d.TransactionalWorkers {
			if allowed, filtered := d.TransactionalTypes[name]; filtered {
				if _, matches := allowed[event.Type]; !matches {
					continue
				}
			}
			release, err := d.acquireWork(ctx)
			if err != nil {
				allOK = false
				continue
			}
			err = d.runTransactional(ctx, name, id, event, worker)
			release()
			if err != nil {
				allOK = false
			}
		}

		if allOK {
			processed++
		}
	}
	return processed, nil
}

func (d *Dispatcher) acquireWork(ctx context.Context) (func(), error) {
	if d.WorkAdmission == nil {
		return func() {}, nil
	}
	return d.WorkAdmission.Acquire(ctx)
}

func (d *Dispatcher) matchesWorkerEvent(name, eventType string) bool {
	if patterns, ok := d.WorkerPatterns[name]; ok {
		for _, pattern := range patterns {
			var expression strings.Builder
			expression.WriteByte('^')
			escaped := false
			for _, character := range pattern {
				if escaped {
					expression.WriteString(regexp.QuoteMeta(string(character)))
					escaped = false
					continue
				}
				switch character {
				case '!':
					escaped = true
				case '%':
					expression.WriteString(".*")
				case '_':
					expression.WriteByte('.')
				default:
					expression.WriteString(regexp.QuoteMeta(string(character)))
				}
			}
			if escaped {
				expression.WriteString(regexp.QuoteMeta("!"))
			}
			expression.WriteByte('$')
			if matched, _ := regexp.MatchString(expression.String(), eventType); matched {
				return true
			}
		}
		return false
	}
	if allowed, ok := d.WorkerTypes[name]; ok {
		_, matches := allowed[eventType]
		return matches
	}
	return true
}

func (d *Dispatcher) consumerQuery(limit int) (string, []any, bool) {
	clauses := make([]string, 0, len(d.Workers)+len(d.TransactionalWorkers))
	args := make([]any, 0, len(d.Workers)+len(d.TransactionalWorkers)+1)
	workerNames := make([]string, 0, len(d.Workers))
	for name := range d.Workers {
		workerNames = append(workerNames, name)
	}
	sort.Strings(workerNames)
	for _, name := range workerNames {
		if patterns, patterned := d.WorkerPatterns[name]; patterned {
			patternClauses := make([]string, 0, len(patterns))
			for _, pattern := range patterns {
				patternClauses = append(patternClauses, `o.event_type LIKE ? ESCAPE '!'`)
				args = append(args, pattern)
			}
			clauses = append(clauses, `(`+strings.Join(patternClauses, ` OR `)+`) AND NOT EXISTS (SELECT 1 FROM _kora_outbox_receipt r WHERE r.consumer_name=? AND r.event_id=o.id)`)
		} else if filter, filtered := d.WorkerTypes[name]; filtered {
			eventTypes := make([]string, 0, len(filter))
			for eventType := range filter {
				eventTypes = append(eventTypes, eventType)
			}
			sort.Strings(eventTypes)
			if len(eventTypes) == 0 {
				continue
			}
			placeholders := make([]string, len(eventTypes))
			for i, eventType := range eventTypes {
				placeholders[i] = "?"
				args = append(args, eventType)
			}
			clauses = append(clauses, ` (o.event_type IN (`+strings.Join(placeholders, ",")+`)) AND NOT EXISTS (SELECT 1 FROM _kora_outbox_receipt r WHERE r.consumer_name=? AND r.event_id=o.id)`)
		} else {
			clauses = append(clauses, `NOT EXISTS (SELECT 1 FROM _kora_outbox_receipt r WHERE r.consumer_name=? AND r.event_id=o.id)`)
		}
		args = append(args, name)
	}
	transactionalNames := make([]string, 0, len(d.TransactionalWorkers))
	for name := range d.TransactionalWorkers {
		transactionalNames = append(transactionalNames, name)
	}
	sort.Strings(transactionalNames)
	for _, name := range transactionalNames {
		filter, filtered := d.TransactionalTypes[name]
		if filtered {
			eventTypes := make([]string, 0, len(filter))
			for eventType := range filter {
				eventTypes = append(eventTypes, eventType)
			}
			sort.Strings(eventTypes)
			if len(eventTypes) == 0 {
				continue
			}
			placeholders := make([]string, len(eventTypes))
			for i, eventType := range eventTypes {
				placeholders[i] = "?"
				args = append(args, eventType)
			}
			clauses = append(clauses, ` (o.event_type IN (`+strings.Join(placeholders, ",")+`)) AND NOT EXISTS (SELECT 1 FROM _kora_outbox_receipt r WHERE r.consumer_name=? AND r.event_id=o.id)`)
		} else {
			clauses = append(clauses, `NOT EXISTS (SELECT 1 FROM _kora_outbox_receipt r WHERE r.consumer_name=? AND r.event_id=o.id)`)
		}
		args = append(args, name)
	}
	if len(clauses) == 0 {
		return "", nil, false
	}
	args = append(args, limit)
	query := `SELECT o.id,o.payload,o.source_sequence FROM _kora_outbox o WHERE (` + strings.Join(clauses, ` OR `) + `) ORDER BY o.created_at,o.id LIMIT ?`
	return query, args, true
}

func (d *Dispatcher) runTransactional(ctx context.Context, name, eventID string, event contract.EventEnvelope, worker TransactionalWorker) error {
	tx, err := d.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("outbox: begin consumer %q transaction: %w", name, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	consumer := &Consumer{DB: d.DB, Name: name, Dialect: d.Dialect}
	inserted, err := consumer.RecordReceiptSequenceTx(ctx, tx, eventID, event.Site, string(event.Sequence))
	if err != nil {
		return err
	}
	if !inserted {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("outbox: commit duplicate consumer receipt %q: %w", name, err)
		}
		committed = true
		return nil
	}
	if err := worker(ctx, tx, event); err != nil {
		return fmt.Errorf("outbox: transactional consumer %q effect: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("outbox: commit consumer %q effect and receipt: %w", name, err)
	}
	committed = true
	return nil
}

// backoff returns a bounded exponential backoff with a deterministic base. It
// doubles from 1s up to 1 minute.
func backoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	base := time.Second
	for i := 0; i < attempt && base < time.Minute; i++ {
		base *= 2
	}
	if base > time.Minute {
		base = time.Minute
	}
	return base
}

// ChangeEventToEnvelope converts a ChangeEvent into an EventEnvelope
// using the canonical event-type naming scheme shared with webhooks.
func ChangeEventToEnvelope(e eventstream.ChangeEvent) contract.EventEnvelope {
	return contract.EventEnvelope{
		ID:            contract.NewEventID(),
		Type:          EventTypeName(e.Doctype, e.Operation),
		Version:       contract.CurrentVersion,
		Source:        "kora.kernel",
		Site:          e.Site,
		BusinessScope: e.BusinessScope,
		AggregateType: e.Doctype,
		AggregateID:   e.DocName,
		OccurredAt:    e.Timestamp,
		Data:          contract.MustEncodeData(map[string]any{"data": e.Data, "old_data": e.OldData}),
	}
}

// EventTypeName returns a canonical event type name for a doctype + operation.
func EventTypeName(doctype string, op eventstream.EventOp) string {
	snake := ToSnake(doctype)
	switch op {
	case eventstream.EventInsert:
		return "kora." + snake + ".after_insert"
	case eventstream.EventUpdate:
		return "kora." + snake + ".after_save"
	case eventstream.EventDelete:
		return "kora." + snake + ".after_delete"
	case eventstream.EventSubmit:
		return "kora." + snake + ".after_submit"
	case eventstream.EventCancel:
		return "kora." + snake + ".after_cancel"
	default:
		return "kora." + snake + "." + string(op)
	}
}

// ToSnake lowercases a doctype name and replaces spaces with underscores.
func ToSnake(name string) string {
	b := []byte(name)
	for i := 0; i < len(b); i++ {
		if b[i] == ' ' {
			b[i] = '_'
		} else if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 32
		}
	}
	return string(b)
}
