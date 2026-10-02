package ai

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/oklog/ulid/v2"
)

// BudgetReservation is a durable reservation against a site's AI token budget.
type BudgetReservation struct {
	ID              string
	Site            string
	Model           string
	RequestedTokens int
	Note            string
}

type AuditEvent struct {
	ID             string
	Site           string
	RunID          string
	StepID         string
	ConversationID string
	Kind           string
	Name           string
	Status         string
	UserID         string
	SessionID      string
	CorrelationID  string
	IdempotencyKey string
	Details        map[string]any
	CreatedAt      time.Time
}

func auditContextFields(ctx context.Context) (userID, sessionID, correlationID, idempotencyKey string) {
	if ctx == nil {
		return "", "", "", ""
	}
	userID, _ = ctx.Value("user").(string)
	sessionID, _ = ctx.Value("session_sid").(string)
	correlationID, _ = ctx.Value("correlation_id").(string)
	idempotencyKey, _ = ctx.Value("idempotency_key").(string)
	return
}

func enrichAuditContext(ctx context.Context, userID, sessionID, correlationID, idempotencyKey string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if userID != "" {
		ctx = context.WithValue(ctx, "user", userID)
	}
	if sessionID != "" {
		ctx = context.WithValue(ctx, "session_sid", sessionID)
	}
	if correlationID != "" {
		ctx = context.WithValue(ctx, "correlation_id", correlationID)
	}
	if idempotencyKey != "" {
		ctx = context.WithValue(ctx, "idempotency_key", idempotencyKey)
	}
	return ctx
}

// RecordUsage stores an immutable AI usage event.
func RecordUsage(ctx context.Context, db *sql.DB, ev contract.UsageEvent) error {
	if db == nil {
		return nil
	}
	if ev.ID == "" {
		ev.ID = ulid.Make().String()
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now().UTC()
	}
	tokensJSON, _ := json.Marshal(ev.Tokens)
	attrJSON, _ := json.Marshal(ev.Attribution)
	_, err := (aiSQL{db}).ExecContext(ctx, `
INSERT INTO _kora_ai_usage (
	id, site, organization_id, user_id, model, provider, run_id, step_id, channel,
	attempt, status, tokens, latency_ms, occurred_at, retry_of, attribution
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ID, ev.Site, ev.Organization, ev.UserID, ev.Model, ev.Provider, ev.RunID, ev.StepID, ev.Channel,
		ev.Attempt, ev.Status, string(tokensJSON), ev.LatencyMs, ev.OccurredAt, ev.RetryOf, string(attrJSON),
	)
	if err != nil {
		return fmt.Errorf("record ai usage: %w", err)
	}
	return nil
}

func RecordAudit(ctx context.Context, db *sql.DB, ev AuditEvent) error {
	if db == nil {
		return nil
	}
	if ev.ID == "" {
		ev.ID = ulid.Make().String()
	}
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = time.Now().UTC()
	}
	if ev.UserID == "" || ev.SessionID == "" || ev.CorrelationID == "" || ev.IdempotencyKey == "" {
		userID, sessionID, correlationID, idempotencyKey := auditContextFields(ctx)
		if ev.UserID == "" {
			ev.UserID = userID
		}
		if ev.SessionID == "" {
			ev.SessionID = sessionID
		}
		if ev.CorrelationID == "" {
			ev.CorrelationID = correlationID
		}
		if ev.IdempotencyKey == "" {
			ev.IdempotencyKey = idempotencyKey
		}
	}
	detailsJSON, _ := json.Marshal(ev.Details)
	_, err := (aiSQL{db}).ExecContext(ctx, `
INSERT INTO _kora_ai_audit (
	id, site, run_id, step_id, conversation_id, kind, name, status, user_id, session_id, correlation_id, idempotency_key, details, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ID, ev.Site, ev.RunID, ev.StepID, ev.ConversationID, ev.Kind, ev.Name, ev.Status, ev.UserID, ev.SessionID, ev.CorrelationID, ev.IdempotencyKey, string(detailsJSON), ev.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("record ai audit: %w", err)
	}
	return nil
}

// ReserveBudget records a token reservation if enough unreserved budget remains.
func ReserveBudget(ctx context.Context, db *sql.DB, site, model string, requestedTokens, budget int, note string) (BudgetReservation, error) {
	if db == nil {
		return BudgetReservation{}, nil
	}
	if requestedTokens <= 0 {
		requestedTokens = 1
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return BudgetReservation{}, fmt.Errorf("reserve ai budget: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var used int
	if err := tx.QueryRowContext(ctx, bindAIQuery(ctx, `
SELECT COALESCE(SUM(requested_tokens), 0)
FROM _kora_ai_budget_reservation
WHERE site = ? AND model = ? AND status = 'reserved'`),
		site, model).Scan(&used); err != nil {
		return BudgetReservation{}, fmt.Errorf("reserve ai budget: %w", err)
	}
	if budget > 0 && used+requestedTokens > budget {
		return BudgetReservation{}, fmt.Errorf("ai budget exceeded")
	}
	res := BudgetReservation{
		ID:              ulid.Make().String(),
		Site:            site,
		Model:           model,
		RequestedTokens: requestedTokens,
		Note:            note,
	}
	_, err = tx.ExecContext(ctx, bindAIQuery(ctx, `
INSERT INTO _kora_ai_budget_reservation (id, site, model, requested_tokens, status, note, reserved_at)
VALUES (?, ?, ?, ?, 'reserved', ?, ?)`),
		res.ID, res.Site, res.Model, res.RequestedTokens, res.Note, time.Now().UTC())
	if err != nil {
		return BudgetReservation{}, fmt.Errorf("reserve ai budget: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return BudgetReservation{}, fmt.Errorf("reserve ai budget: %w", err)
	}
	return res, nil
}

// ReleaseBudget marks a reservation as released.
func ReleaseBudget(ctx context.Context, db *sql.DB, res BudgetReservation) error {
	if db == nil || res.ID == "" {
		return nil
	}
	_, err := (aiSQL{db}).ExecContext(ctx, `
UPDATE _kora_ai_budget_reservation
SET status = 'released', released_at = ?
WHERE id = ? AND status = 'reserved'`,
		time.Now().UTC(), res.ID)
	if err != nil {
		return fmt.Errorf("release ai budget: %w", err)
	}
	return nil
}

// FinalizeBudget marks a reservation as consumed with the actual token count.
func FinalizeBudget(ctx context.Context, db *sql.DB, res BudgetReservation, consumedTokens int) error {
	if db == nil || res.ID == "" {
		return nil
	}
	if consumedTokens < 0 {
		consumedTokens = 0
	}
	_, err := (aiSQL{db}).ExecContext(ctx, `
UPDATE _kora_ai_budget_reservation
SET status = 'finalized', consumed_tokens = ?, released_at = ?
WHERE id = ? AND status = 'reserved'`,
		consumedTokens, time.Now().UTC(), res.ID)
	if err != nil {
		return fmt.Errorf("finalize ai budget: %w", err)
	}
	return nil
}
