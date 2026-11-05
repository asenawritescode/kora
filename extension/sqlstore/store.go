// Package sqlstore persists tenant extensions in the tenant database.
package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/extension"
)

// Store reads and writes _kora_extension for one tenant database.
type Store struct {
	database *sql.DB
	dialect  db.QueryDialect
}

// New binds a store to a tenant database. It is cheap; callers may open one per request.
func New(database *sql.DB, dialect db.QueryDialect) *Store {
	return &Store{database: database, dialect: dialect}
}

func (s *Store) query(q string) string { return db.Rebind(s.dialect, q) }

// VerifyAccessToken resolves the active extension owning tokenHash.
func (s *Store) VerifyAccessToken(ctx context.Context, site, tokenHash string) (extension.Credential, error) {
	var name string
	var permissions sql.NullString
	err := s.database.QueryRowContext(ctx, s.query(`SELECT name, api_permissions FROM _kora_extension WHERE site = ? AND access_token_hash = ? AND is_active = 1`), site, tokenHash).Scan(&name, &permissions)
	if errors.Is(err, sql.ErrNoRows) {
		return extension.Credential{}, extension.ErrNotFound
	}
	if err != nil {
		return extension.Credential{}, fmt.Errorf("verify extension credential: %w", err)
	}
	return extension.Credential{Name: name, APIPermissions: permissions.String}, nil
}

// List returns the site's extensions, newest first.
func (s *Store) List(ctx context.Context, site string) ([]extension.Summary, error) {
	rows, err := s.database.QueryContext(ctx, s.query(
		`SELECT name, site, display_name, description, endpoint_url, is_active, subscriptions, api_permissions,
		 secret_count, consecutive_failures, installed_at, last_delivery_at, last_error
		 FROM _kora_extension WHERE site = ? ORDER BY installed_at DESC`), site)
	if err != nil {
		return nil, fmt.Errorf("list extensions: %w", err)
	}
	defer rows.Close()
	var out []extension.Summary
	for rows.Next() {
		var e extension.Summary
		var rowSite string
		var desc, lastErr, subs, perms, installedAt, lastDeliveryAt sql.NullString
		if err := rows.Scan(&e.Name, &rowSite, &e.DisplayName, &desc, &e.EndpointURL, &e.IsActive, &subs, &perms,
			&e.SecretCount, &e.ConsecutiveFailures, &installedAt, &lastDeliveryAt, &lastErr); err != nil {
			return nil, fmt.Errorf("read extension: %w", err)
		}
		e.Description, e.Subscriptions, e.APIPermissions = desc.String, subs.String, perms.String
		e.InstalledAt, e.LastDeliveryAt, e.LastError = installedAt.String, lastDeliveryAt.String, lastErr.String
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read extensions: %w", err)
	}
	return out, nil
}

// Create installs a new extension.
func (s *Store) Create(ctx context.Context, r extension.Registration) error {
	_, err := s.database.ExecContext(ctx, s.query(`INSERT INTO _kora_extension (name, site, display_name, description, endpoint_url, secret, access_token, access_token_hash, subscriptions, api_permissions, installed_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, '', ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`),
		r.Name, r.Site, r.DisplayName, r.Description, r.EndpointURL, r.Secret, r.AccessTokenHash, r.Subscriptions, r.APIPermissions)
	if err != nil {
		return fmt.Errorf("create extension: %w", err)
	}
	return nil
}

// Delete removes an extension and its delivery log in one transaction.
func (s *Store) Delete(ctx context.Context, site, name string) error {
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin extension delete: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, s.query(`DELETE FROM _kora_extension WHERE site = ? AND name = ?`), site, name)
	if err != nil {
		return fmt.Errorf("delete extension: %w", err)
	}
	if err := requireOneRow(result); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, s.query(`DELETE FROM _kora_webhook_delivery WHERE site = ? AND extension_name = ?`), site, name); err != nil {
		return fmt.Errorf("delete extension deliveries: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit extension delete: %w", err)
	}
	return nil
}

// RotateSecret installs a new signing secret and keeps the previous one until its expiry.
func (s *Store) RotateSecret(ctx context.Context, r extension.SecretRotation) error {
	result, err := s.database.ExecContext(ctx, s.query(`UPDATE _kora_extension SET old_secret = secret, old_secret_expires_at = ?,
		secret = ?, secret_count = secret_count + 1, updated_at = CURRENT_TIMESTAMP WHERE site = ? AND name = ?`),
		r.PreviousExpiresAt, r.Secret, r.Site, r.Name)
	if err != nil {
		return fmt.Errorf("rotate extension secret: %w", err)
	}
	return requireOneRow(result)
}

func requireOneRow(result sql.Result) error {
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check affected extension: %w", err)
	}
	if n == 0 {
		return extension.ErrNotFound
	}
	return nil
}
