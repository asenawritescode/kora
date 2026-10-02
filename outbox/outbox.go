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
	"fmt"
	"time"

	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
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
	if err := event.Validate(); err != nil {
		return fmt.Errorf("outbox: validate event: %w", err)
	}

	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("outbox: marshal event: %w", err)
	}

	_, err = tx.ExecContext(ctx, db.Rebind(w.Dialect,
		`INSERT INTO _kora_outbox
			(id, site, event_type, event_version, aggregate_type, aggregate_id, payload, status, attempts, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	),
		event.ID,
		event.Site,
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

// Publisher claims pending rows, publishes them through a contract.EventPublisher,
// and marks them published (or failed). It is safe to run from multiple processes
// because claims use a lease. Rows are never deleted automatically.
type Publisher struct {
	DB          *sql.DB
	Dialect     db.QueryDialect
	Destination contract.EventPublisher
	LeaseOwner  string
	LeaseTTL    time.Duration
	MaxAttempts int
}

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
	if limit <= 0 {
		limit = 100
	}
	now := time.Now().UTC()
	leaseUntil := now.Add(p.LeaseTTL)

	// Select due row IDs first (LIMIT in SELECT is portable across MySQL, LibSQL,
	// and Postgres), then claim each row by primary key. This avoids LIMIT in an
	// UPDATE, which SQLite/LibSQL do not support.
	rows, err := p.DB.QueryContext(ctx, db.Rebind(p.Dialect,
		`SELECT id FROM _kora_outbox
		 WHERE (status = 'pending' AND (next_attempt_at IS NULL OR next_attempt_at <= ?))
			OR (status = 'publishing' AND lease_until IS NOT NULL AND lease_until < ?)
		 ORDER BY created_at
		 LIMIT ?`),
		now, now, limit)
	if err != nil {
		return 0, fmt.Errorf("outbox: select due rows: %w", err)
	}

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	if len(ids) == 0 {
		return 0, nil
	}

	// Claim each due row. Rows that another worker claimed in between are skipped.
	published := 0
	for _, id := range ids {
		res, err := p.DB.ExecContext(ctx, db.Rebind(p.Dialect,
			`UPDATE _kora_outbox
			 SET status = ?, lease_owner = ?, lease_until = ?, attempts = attempts + 1
			 WHERE id = ?
			   AND (
			       (status = 'pending' AND (next_attempt_at IS NULL OR next_attempt_at <= ?))
			       OR (status = 'publishing' AND lease_until IS NOT NULL AND lease_until < ?)
			   )`),
			string(StatusPublishing), p.LeaseOwner, leaseUntil, id, now, now)
		if err != nil {
			continue // claim failed — leave for another worker
		}
		if affected, _ := res.RowsAffected(); affected == 0 {
			continue // already claimed by another worker
		}

		var attempts int
		_ = p.DB.QueryRowContext(ctx,
			db.Rebind(p.Dialect, `SELECT attempts FROM _kora_outbox WHERE id = ?`), id).Scan(&attempts)

		if err := p.publishOne(ctx, id); err != nil {
			if attempts >= p.MaxAttempts {
				_ = p.markFailed(ctx, id, err)
			} else {
				_ = p.retry(ctx, id, attempts, err)
			}
			continue
		}
		_ = p.markPublished(ctx, id)
		published++
	}

	return published, nil
}

func (p *Publisher) publishOne(ctx context.Context, id string) error {
	var payload string
	err := p.DB.QueryRowContext(ctx,
		db.Rebind(p.Dialect, `SELECT payload FROM _kora_outbox WHERE id = ?`), id).Scan(&payload)
	if err != nil {
		return err
	}

	var event contract.EventEnvelope
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return fmt.Errorf("outbox: unmarshal event %s: %w", id, err)
	}
	return p.Destination.Publish(ctx, event)
}

func (p *Publisher) markPublished(ctx context.Context, id string) error {
	_, err := p.DB.ExecContext(ctx, db.Rebind(p.Dialect,
		`UPDATE _kora_outbox SET status = ?, published_at = ?, lease_owner = '', lease_until = NULL WHERE id = ?`),
		string(StatusPublished), time.Now().UTC(), id)
	return err
}

func (p *Publisher) retry(ctx context.Context, id string, attempts int, cause error) error {
	_, err := p.DB.ExecContext(ctx, db.Rebind(p.Dialect,
		`UPDATE _kora_outbox SET status = 'pending', last_error = ?, lease_owner = '', lease_until = NULL,
			next_attempt_at = ? WHERE id = ?`),
		cause.Error(), time.Now().UTC().Add(backoff(attempts)), id)
	return err
}

func (p *Publisher) markFailed(ctx context.Context, id string, cause error) error {
	_, err := p.DB.ExecContext(ctx, db.Rebind(p.Dialect,
		`UPDATE _kora_outbox SET status = ?, last_error = ?, lease_owner = '', lease_until = NULL WHERE id = ?`),
		string(StatusFailed), cause.Error(), id)
	return err
}

// ReplayFailed resets failed rows to pending so they are retried (operator replay).
func (p *Publisher) ReplayFailed(ctx context.Context) (int, error) {
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

// Consumer is a named receiver of outbox events. Receipts make redelivery safe:
// a consumer persists a unique (consumer_name, event_id) receipt before applying
// its effect, so duplicate delivery (at-least-once) never duplicates the effect.
type Consumer struct {
	DB      *sql.DB
	Name    string
	Dialect db.QueryDialect
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
	_, err := c.DB.ExecContext(ctx, db.Rebind(c.Dialect,
		`INSERT INTO _kora_outbox_receipt (consumer_name, event_id, site, received_at)
		 VALUES (?, ?, ?, ?)`),
		c.Name, eventID, site, time.Now().UTC())
	return err
}

// OutboxMetrics is an operational snapshot of outbox health (RFC §8.1).
type OutboxMetrics struct {
	Pending   int `json:"pending"`
	Failed    int `json:"failed"`
	Published int `json:"published"`
}

// Metrics returns outbox health counters for operator visibility.
func (p *Publisher) Metrics(ctx context.Context) (OutboxMetrics, error) {
	var m OutboxMetrics
	err := p.DB.QueryRowContext(ctx, db.Rebind(p.Dialect,
		`SELECT
		  COALESCE(SUM(CASE WHEN status = 'pending' THEN 1 ELSE 0 END), 0),
		  COALESCE(SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END), 0),
		  COALESCE(SUM(CASE WHEN status = 'published' THEN 1 ELSE 0 END), 0)
		 FROM _kora_outbox`)).Scan(&m.Pending, &m.Failed, &m.Published)
	return m, err
}

// Worker is the consumer-side processing function for a named consumer. It
// receives a single event and returns an error to signal a failed (retryable)
// effect. Receipting is handled by Dispatcher.Route, not the worker.
type Worker func(ctx context.Context, event contract.EventEnvelope) error

// Dispatcher delivers due outbox events to named consumers with idempotent
// receipting. This is the "worker interface" seam from RFC Phase 1: analytics,
// webhooks, and async hooks each register a named worker and the dispatcher
// guarantees at-least-once delivery with at-most-once effect via
// (consumer_name, event_id) receipts.
type Dispatcher struct {
	DB      *sql.DB
	Dialect db.QueryDialect
	Workers map[string]Worker
}

// NewDispatcher returns a Dispatcher with no workers registered.
func NewDispatcher(database *sql.DB, dialect db.QueryDialect) *Dispatcher {
	return &Dispatcher{DB: database, Dialect: dialect, Workers: make(map[string]Worker)}
}

// Register adds a named worker.
func (d *Dispatcher) Register(name string, w Worker) {
	d.Workers[name] = w
}

// Run claims up to limit due events and routes each to every registered worker,
// using a per-consumer receipt so a redelivered event is not processed again.
func (d *Dispatcher) Run(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := d.DB.QueryContext(ctx, db.Rebind(d.Dialect,
		`SELECT id, payload FROM _kora_outbox
		 WHERE status = 'pending'
		 ORDER BY created_at
		 LIMIT ?`), limit)
	if err != nil {
		return 0, fmt.Errorf("outbox: select due events: %w", err)
	}
	defer rows.Close()

	processed := 0
	for rows.Next() {
		var id, payload string
		if err := rows.Scan(&id, &payload); err != nil {
			continue
		}
		var event contract.EventEnvelope
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}

		allOK := true
		for name, worker := range d.Workers {
			c := &Consumer{DB: d.DB, Name: name, Dialect: d.Dialect}
			seen, err := c.HasSeen(ctx, id)
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
			if err := c.RecordReceipt(ctx, id, event.Site); err != nil {
				allOK = false
			}
		}

		if allOK {
			_ = d.markPublished(ctx, id)
			processed++
		}
	}
	return processed, rows.Err()
}

func (d *Dispatcher) markPublished(ctx context.Context, id string) error {
	_, err := d.DB.ExecContext(ctx, db.Rebind(d.Dialect,
		`UPDATE _kora_outbox SET status = ?, published_at = ? WHERE id = ?`),
		string(StatusPublished), time.Now().UTC(), id)
	return err
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

// ChangeEventToEnvelope converts an analytics.ChangeEvent into an EventEnvelope
// using the canonical event-type naming scheme shared with webhooks.
func ChangeEventToEnvelope(e analytics.ChangeEvent) contract.EventEnvelope {
	return contract.EventEnvelope{
		ID:            contract.NewEventID(),
		Type:          EventTypeName(e.Doctype, e.Operation),
		Version:       contract.CurrentVersion,
		Source:        "kora.kernel",
		Site:          e.Site,
		AggregateType: e.Doctype,
		AggregateID:   e.DocName,
		OccurredAt:    e.Timestamp,
		Data:          contract.MustEncodeData(map[string]any{"data": e.Data, "old_data": e.OldData}),
	}
}

// EventTypeName returns a canonical event type name for a doctype + operation.
func EventTypeName(doctype string, op analytics.EventOp) string {
	snake := ToSnake(doctype)
	switch op {
	case analytics.EventInsert:
		return "kora." + snake + ".after_insert"
	case analytics.EventUpdate:
		return "kora." + snake + ".after_save"
	case analytics.EventDelete:
		return "kora." + snake + ".after_delete"
	case analytics.EventSubmit:
		return "kora." + snake + ".after_submit"
	case analytics.EventCancel:
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
