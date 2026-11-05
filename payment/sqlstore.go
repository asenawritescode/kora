package payment

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/asenawritescode/kora/db"
)

const intentCols = `id, provider, state, amount, currency, contact, source_doctype,
	idempotency_key, payload_hash, provider_request_id, provider_reference,
	failure_reason, actor_user, consumed_by, created_at, updated_at`

// SQLStore persists intents in _kora_payment_intent.
type SQLStore struct {
	DB      *sql.DB
	Dialect db.QueryDialect
}

var _ Store = (*SQLStore)(nil)

func (s *SQLStore) q(query string) string { return db.Rebind(s.Dialect, query) }

func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// Create inserts a new intent. A duplicate (site, key) or (site, provider,
// provider_request_id) violates a unique index and is returned as an error.
func (s *SQLStore) Create(ctx context.Context, site string, in Intent) error {
	_, err := s.DB.ExecContext(ctx, s.q(`INSERT INTO _kora_payment_intent
		(id, site, provider, state, amount, currency, contact, source_doctype,
		 idempotency_key, payload_hash, provider_request_id, provider_reference,
		 failure_reason, actor_user, consumed_by, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
		in.ID, site, in.Provider, string(in.State), in.Amount.String(), in.Currency,
		in.Contact, in.SourceDoctype, in.IdempotencyKey, in.PayloadHash,
		nullable(in.ProviderRequestID), nullable(in.ProviderReference),
		in.FailureReason, in.Actor, nullable(in.ConsumedBy),
		in.CreatedAt.UTC(), in.UpdatedAt.UTC())
	if err != nil {
		return fmt.Errorf("payment: insert intent: %w", err)
	}
	return nil
}

func (s *SQLStore) one(ctx context.Context, where string, args ...any) (Intent, error) {
	var (
		it                 Intent
		state              string
		reqID, ref, reason sql.NullString
		consumed           sql.NullString
		created, updated   db.Timestamp
	)
	err := s.DB.QueryRowContext(ctx, s.q("SELECT "+intentCols+" FROM _kora_payment_intent WHERE "+where), args...).
		Scan(&it.ID, &it.Provider, &state, &it.Amount, &it.Currency, &it.Contact,
			&it.SourceDoctype, &it.IdempotencyKey, &it.PayloadHash, &reqID, &ref,
			&reason, &it.Actor, &consumed, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Intent{}, ErrNotFound
	}
	if err != nil {
		return Intent{}, fmt.Errorf("payment: load intent: %w", err)
	}
	it.State, it.ProviderRequestID, it.ProviderReference = State(state), reqID.String, ref.String
	it.FailureReason, it.ConsumedBy = reason.String, consumed.String
	it.CreatedAt, it.UpdatedAt = created.Time.UTC(), updated.Time.UTC()
	return it, nil
}

func (s *SQLStore) Get(ctx context.Context, site, id string) (Intent, error) {
	return s.one(ctx, "site = ? AND id = ?", site, id)
}

func (s *SQLStore) GetByKey(ctx context.Context, site, key string) (Intent, error) {
	return s.one(ctx, "site = ? AND idempotency_key = ?", site, key)
}

func (s *SQLStore) GetByProviderRequest(ctx context.Context, site, provider, reqID string) (Intent, error) {
	return s.one(ctx, "site = ? AND provider = ? AND provider_request_id = ?", site, provider, reqID)
}

// Transition moves a still-pending intent to o.State; it reports false when the
// row was already terminal (or missing), so concurrent confirms apply once.
func (s *SQLStore) Transition(ctx context.Context, site, id string, o Outcome) (bool, error) {
	res, err := s.DB.ExecContext(ctx, s.q(`UPDATE _kora_payment_intent
		SET state = ?, provider_reference = ?, failure_reason = ?, updated_at = ?
		WHERE site = ? AND id = ? AND state = ?`),
		string(o.State), nullable(o.ProviderReference), o.FailureReason,
		time.Now().UTC(), site, id, string(StatePending))
	if err != nil {
		return false, fmt.Errorf("payment: transition intent: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("payment: transition rows: %w", err)
	}
	return n == 1, nil
}
