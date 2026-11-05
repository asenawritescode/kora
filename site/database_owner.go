package site

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/asenawritescode/kora/db"
)

// ErrDatabaseOwnedByAnotherTenant reports that a tenant database is already
// bound to a different tenant. Business tables carry no tenant prefix, so two
// tenants sharing one database would read and write each other's records.
var ErrDatabaseOwnedByAnotherTenant = errors.New("tenant database is already owned by another tenant")

// ClaimDatabaseOwner binds database to tenantID on first use and refuses any
// later claim by a different tenant.
func ClaimDatabaseOwner(ctx context.Context, database *sql.DB, dialect db.Dialect, tenantID string) error {
	if database == nil || dialect == nil {
		return errors.New("database owner claim requires database and dialect")
	}
	if tenantID == "" {
		return errors.New("database owner claim requires a tenant ID")
	}
	if _, err := database.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS _kora_database_owner (
		id INTEGER PRIMARY KEY,
		tenant_id VARCHAR(255) NOT NULL
	)`); err != nil {
		return fmt.Errorf("creating database owner marker: %w", err)
	}
	owner, err := readDatabaseOwner(ctx, database)
	if errors.Is(err, sql.ErrNoRows) {
		_, insertErr := database.ExecContext(ctx, "INSERT INTO _kora_database_owner (id, tenant_id) VALUES (1, "+dialect.Placeholder(1)+")", tenantID)
		if insertErr == nil {
			return nil
		}
		// A concurrent claim may have won the insert; its row decides.
		owner, err = readDatabaseOwner(ctx, database)
		if err != nil {
			return fmt.Errorf("claiming database owner: %w", errors.Join(insertErr, err))
		}
	} else if err != nil {
		return fmt.Errorf("reading database owner: %w", err)
	}
	if owner != tenantID {
		return fmt.Errorf("%w: database belongs to tenant %q, refusing tenant %q; give each tenant its own database", ErrDatabaseOwnedByAnotherTenant, owner, tenantID)
	}
	return nil
}

func readDatabaseOwner(ctx context.Context, database *sql.DB) (string, error) {
	var owner string
	err := database.QueryRowContext(ctx, `SELECT tenant_id FROM _kora_database_owner WHERE id = 1`).Scan(&owner)
	return owner, err
}
