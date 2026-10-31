package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/notifications"
)

const maxClaimBatch = 100

type Store struct {
	database *sql.DB
	dialect  db.Dialect
}

var _ notifications.Store = (*Store)(nil)

func New(database *sql.DB, dialect db.Dialect) (*Store, error) {
	if database == nil || dialect == nil {
		return nil, fmt.Errorf("notification SQL store requires a database and dialect")
	}
	if _, err := SchemaStatements(dialect); err != nil {
		return nil, err
	}
	return &Store{database: database, dialect: dialect}, nil
}

func (s *Store) HasEvent(ctx context.Context, tenant contract.TenantID, eventID string) (bool, error) {
	if ctx == nil || eventID == "" || len(eventID) > 160 {
		return false, contract.NewError(contract.CodeValidationFailed, "notification event receipt key is invalid")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return false, err
	}
	var exists int
	err := s.database.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT 1 FROM _kora_notification_event WHERE tenant_id = ? AND event_id = ?`), string(tenant), eventID).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read notification event receipt: %w", err)
	}
	return true, nil
}

// RecordEvent commits the consumer receipt and all planned deliveries together.
// A duplicate event is a no-op even when the outer outbox receipt was lost.
func (s *Store) RecordEvent(ctx context.Context, tenant contract.TenantID, eventID string, deliveries []notifications.Delivery, at time.Time) (bool, error) {
	if ctx == nil || at.IsZero() || len(eventID) == 0 || len(eventID) > 160 || len(deliveries) > notifications.MaxPlannedDeliveries {
		return false, contract.NewError(contract.CodeValidationFailed, "notification event receipt is invalid")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return false, err
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin notification event transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	insert := `INSERT INTO _kora_notification_event (tenant_id, event_id, created_at) VALUES (?, ?, ?)`
	if driver, ok := s.dialect.(interface{ DriverName() string }); ok {
		switch driver.DriverName() {
		case "mysql":
			insert = `INSERT IGNORE INTO _kora_notification_event (tenant_id, event_id, created_at) VALUES (?, ?, ?)`
		case "postgres", "libsql":
			insert += ` ON CONFLICT (tenant_id, event_id) DO NOTHING`
		}
	}
	result, err := tx.ExecContext(ctx, db.Rebind(s.dialect, insert), string(tenant), eventID, at.UTC())
	if err != nil {
		return false, fmt.Errorf("record notification event receipt: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("check notification event receipt: %w", err)
	}
	if inserted == 0 {
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("commit duplicate notification receipt: %w", err)
		}
		committed = true
		return false, nil
	}
	for _, delivery := range deliveries {
		if delivery.Tenant != tenant || delivery.EventID != eventID || delivery.ID == "" || delivery.Recipient == "" || delivery.RequestID == "" || !validChannel(delivery.Channel) || delivery.AvailableAt.IsZero() {
			return false, contract.NewError(contract.CodeValidationFailed, "notification delivery does not match its event")
		}
		if _, err := contract.ParseRequestID(string(delivery.ID)); err != nil {
			return false, contract.NewError(contract.CodeValidationFailed, "notification ID is invalid")
		}
		if _, err := contract.ParsePrincipalID(string(delivery.Recipient)); err != nil {
			return false, contract.NewError(contract.CodeValidationFailed, "notification recipient is invalid")
		}
		if _, err := contract.ParseRequestID(string(delivery.RequestID)); err != nil {
			return false, contract.NewError(contract.CodeValidationFailed, "notification request ID is invalid")
		}
		_, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `INSERT INTO _kora_notification_delivery
			(id, tenant_id, recipient_id, request_id, channel, event_id, status, attempts, available_at,
			 lease_owner, lease_token, lease_until, provider_reference, accepted_at, last_error, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, '', 0, NULL, '', NULL, '', ?, ?)`),
			string(delivery.ID), string(tenant), string(delivery.Recipient), string(delivery.RequestID), string(delivery.Channel), eventID,
			notifications.DeliveryPending, delivery.AvailableAt.UTC(), at.UTC(), at.UTC())
		if err != nil {
			return false, fmt.Errorf("insert notification delivery: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit notification event and deliveries: %w", err)
	}
	committed = true
	return true, nil
}

func (s *Store) Claim(ctx context.Context, tenant contract.TenantID, owner string, now, leaseUntil time.Time, limit int) ([]notifications.Delivery, error) {
	if ctx == nil || strings.TrimSpace(owner) == "" || len(owner) > 160 || now.IsZero() || !leaseUntil.After(now) || leaseUntil.Sub(now) > notifications.MaxLeaseDuration || limit < 1 || limit > maxClaimBatch {
		return nil, contract.NewError(contract.CodeValidationFailed, "notification claim bounds are invalid")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return nil, err
	}
	rows, err := s.database.QueryContext(ctx, db.Rebind(s.dialect, `SELECT id FROM _kora_notification_delivery
		WHERE tenant_id = ? AND ((status = ? AND available_at <= ?) OR (status = ? AND lease_until <= ?))
		ORDER BY available_at, id LIMIT ?`), string(tenant), notifications.DeliveryPending, now.UTC(), notifications.DeliverySending, now.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("select due notification deliveries: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan due notification ID: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("read due notification IDs: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close due notification rows: %w", err)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin notification claims: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	claimed := make([]notifications.Delivery, 0, len(ids))
	for _, id := range ids {
		result, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_notification_delivery SET status = ?, attempts = attempts + 1,
			lease_owner = ?, lease_token = lease_token + 1, lease_until = ?, updated_at = ?
			WHERE tenant_id = ? AND id = ? AND ((status = ? AND available_at <= ?) OR (status = ? AND lease_until <= ?))`),
			notifications.DeliverySending, owner, leaseUntil.UTC(), now.UTC(), string(tenant), id,
			notifications.DeliveryPending, now.UTC(), notifications.DeliverySending, now.UTC())
		if err != nil {
			return nil, fmt.Errorf("claim notification %s: %w", id, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("check notification claim %s: %w", id, err)
		}
		if affected == 0 {
			continue
		}
		var item notifications.Delivery
		var attempts, token int64
		var leaseUntil, availableAt, createdAt any
		if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT id, tenant_id, recipient_id, request_id, channel, event_id,
			attempts, status, lease_owner, lease_token, lease_until, available_at, created_at
			FROM _kora_notification_delivery WHERE tenant_id = ? AND id = ? AND lease_owner = ?`),
			string(tenant), id, owner).Scan(&item.ID, &item.Tenant, &item.Recipient, &item.RequestID, &item.Channel, &item.EventID,
			&attempts, &item.Status, &item.ClaimOwner, &token, &leaseUntil, &availableAt, &createdAt); err != nil {
			return nil, fmt.Errorf("load claimed notification %s: %w", id, err)
		}
		parsedLeaseUntil, err := notificationDBTime(leaseUntil)
		if err != nil {
			return nil, fmt.Errorf("decode notification lease timestamp %s: %w", id, err)
		}
		item.LeaseUntil = &parsedLeaseUntil
		if item.AvailableAt, err = notificationDBTime(availableAt); err != nil {
			return nil, fmt.Errorf("decode notification availability timestamp %s: %w", id, err)
		}
		if item.CreatedAt, err = notificationDBTime(createdAt); err != nil {
			return nil, fmt.Errorf("decode notification creation timestamp %s: %w", id, err)
		}
		if attempts < 1 || attempts > int64(^uint16(0)) || token < 1 {
			return nil, contract.NewError(contract.CodeInternal, "stored notification claim counters are invalid")
		}
		item.Attempt, item.ClaimToken = uint16(attempts), token
		claimed = append(claimed, item)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit notification claims: %w", err)
	}
	committed = true
	return claimed, nil
}

func notificationDBTime(value any) (time.Time, error) {
	switch raw := value.(type) {
	case time.Time:
		return raw, nil
	case string:
		return parseNotificationTime(raw)
	case []byte:
		return parseNotificationTime(string(raw))
	case nil:
		return time.Time{}, errors.New("timestamp is null")
	default:
		return time.Time{}, fmt.Errorf("unsupported timestamp value %T", value)
	}
}

func parseNotificationTime(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05.999999", "2006-01-02 15:04:05.999", "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported SQL timestamp %q", value)
}

func (s *Store) Accept(ctx context.Context, tenant contract.TenantID, id notifications.NotificationID, owner string, token int64, result notifications.DeliveryResult, at time.Time) error {
	if err := validateClaimInput(ctx, tenant, id, owner, token, at); err != nil {
		return err
	}
	if len(result.ProviderReference) > 512 {
		return contract.NewError(contract.CodeValidationFailed, "notification provider reference exceeds limit")
	}
	if result.AcceptedAt.IsZero() {
		result.AcceptedAt = at
	}
	if result.AcceptedAt.After(at.Add(time.Minute)) {
		return contract.NewError(contract.CodeValidationFailed, "notification acceptance time is in the future")
	}
	res, err := s.database.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_notification_delivery SET status = ?, provider_reference = ?, accepted_at = ?,
		lease_owner = '', lease_until = NULL, last_error = '', updated_at = ?
		WHERE tenant_id = ? AND id = ? AND status = ? AND lease_owner = ? AND lease_token = ?`),
		notifications.DeliveryAccepted, result.ProviderReference, result.AcceptedAt.UTC(), at.UTC(), string(tenant), string(id), notifications.DeliverySending, owner, token)
	return requireClaim(res, err, id)
}

func (s *Store) Retry(ctx context.Context, tenant contract.TenantID, id notifications.NotificationID, owner string, token int64, next time.Time, cause string, exhausted bool, at time.Time) error {
	if err := validateClaimInput(ctx, tenant, id, owner, token, at); err != nil {
		return err
	}
	if next.IsZero() || next.Before(at) {
		return contract.NewError(contract.CodeValidationFailed, "notification retry time is invalid")
	}
	status := notifications.DeliveryPending
	if exhausted {
		status = notifications.DeliveryExhausted
	}
	cause = truncateUTF8(cause, notifications.MaxErrorBytes)
	res, err := s.database.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_notification_delivery SET status = ?, available_at = ?,
		lease_owner = '', lease_until = NULL, last_error = ?, updated_at = ?
		WHERE tenant_id = ? AND id = ? AND status = ? AND lease_owner = ? AND lease_token = ?`),
		status, next.UTC(), cause, at.UTC(), string(tenant), string(id), notifications.DeliverySending, owner, token)
	return requireClaim(res, err, id)
}

func validateClaimInput(ctx context.Context, tenant contract.TenantID, id notifications.NotificationID, owner string, token int64, at time.Time) error {
	if ctx == nil || at.IsZero() || token < 1 || strings.TrimSpace(owner) == "" || len(owner) > 160 {
		return contract.NewError(contract.CodeValidationFailed, "notification claim acknowledgment is invalid")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return err
	}
	if _, err := contract.ParseRequestID(string(id)); err != nil {
		return contract.NewError(contract.CodeValidationFailed, "notification ID is invalid")
	}
	return nil
}

func requireClaim(result sql.Result, err error, id notifications.NotificationID) error {
	if err != nil {
		return fmt.Errorf("update notification %s: %w", id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check notification update %s: %w", id, err)
	}
	if rows != 1 {
		return contract.NewError(contract.CodeConflict, "notification claim was lost")
	}
	return nil
}

func validChannel(channel notifications.Channel) bool {
	return channel == notifications.ChannelInbox || channel == notifications.ChannelEmail || channel == notifications.ChannelPush
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
