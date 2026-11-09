package configstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"github.com/asenawritescode/kora/internal/db"
)

var ErrManagementActivationFence = errors.New("management activation claim is stale or does not match its accepted operation")

func ManagementActivationFenceSchema(dialect db.Dialect) []string {
	switch dialect.DriverName() {
	case "postgres":
		return []string{"CREATE TABLE IF NOT EXISTS _kora_management_activation_fence (site TEXT NOT NULL, operation_id TEXT NOT NULL, fence BIGINT NOT NULL CHECK (fence > 0), expected_revision BIGINT NOT NULL CHECK (expected_revision > 0), artifact_digest TEXT NOT NULL, updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP, PRIMARY KEY (site, operation_id))"}
	case "libsql":
		return []string{"CREATE TABLE IF NOT EXISTS _kora_management_activation_fence (site TEXT NOT NULL, operation_id TEXT NOT NULL, fence BIGINT NOT NULL CHECK (fence > 0), expected_revision BIGINT NOT NULL CHECK (expected_revision > 0), artifact_digest TEXT NOT NULL, updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP, PRIMARY KEY (site, operation_id))"}
	default:
		return []string{"CREATE TABLE IF NOT EXISTS _kora_management_activation_fence (site VARCHAR(140) NOT NULL, operation_id VARCHAR(36) NOT NULL, fence BIGINT NOT NULL, expected_revision BIGINT NOT NULL, artifact_digest CHAR(64) NOT NULL, updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), PRIMARY KEY (site, operation_id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"}
	}
}

// RecordManagementActivationFence commits a claim high-water mark before an
// activation begins. Newer claims fence older attempts; operation identity,
// expected revision and artifact digest remain immutable across retries.
func RecordManagementActivationFence(ctx context.Context, database *sql.DB, dialect db.Dialect, tenant, operationID string, fence, expectedRevision uint64, digest string) error {
	if ctx == nil || database == nil || dialect == nil || tenant == "" || len(tenant) > 140 || operationID == "" || len(operationID) > 36 || fence == 0 || fence > math.MaxInt64 || expectedRevision == 0 || expectedRevision > math.MaxInt64 || !validActivationDigest(digest) {
		return fmt.Errorf("management activation fence identity is invalid")
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin management activation fence: %w", err)
	}
	defer tx.Rollback()
	insert := "INSERT INTO _kora_management_activation_fence (site, operation_id, fence, expected_revision, artifact_digest) VALUES (?, ?, ?, ?, ?)"
	switch dialect.DriverName() {
	case "mysql":
		insert = "INSERT IGNORE" + insert[len("INSERT"):]
	case "libsql":
		insert = "INSERT OR IGNORE" + insert[len("INSERT"):]
	case "postgres":
		insert += " ON CONFLICT (site, operation_id) DO NOTHING"
	}
	if _, err := tx.ExecContext(ctx, db.Rebind(dialect, insert), tenant, operationID, fence, expectedRevision, digest); err != nil {
		return fmt.Errorf("create management activation fence: %w", err)
	}
	update := "UPDATE _kora_management_activation_fence SET fence = ?, updated_at = CURRENT_TIMESTAMP WHERE site = ? AND operation_id = ? AND expected_revision = ? AND artifact_digest = ? AND fence < ?"
	if _, err := tx.ExecContext(ctx, db.Rebind(dialect, update), fence, tenant, operationID, expectedRevision, digest, fence); err != nil {
		return fmt.Errorf("advance management activation fence: %w", err)
	}
	current, err := readActivationFence(ctx, tx, dialect, tenant, operationID, true)
	if err != nil {
		return err
	}
	if current.fence != int64(fence) || current.expectedRevision != int64(expectedRevision) || current.digest != digest {
		return ErrManagementActivationFence
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit management activation fence: %w", err)
	}
	return nil
}

// ValidateManagementActivationFenceTx serializes activation commit against a
// newer claim. Call it in the same transaction as the config revision CAS and
// configuration writes.
func ValidateManagementActivationFenceTx(ctx context.Context, tx *sql.Tx, dialect db.Dialect, tenant, operationID string, fence, expectedRevision uint64, digest string) error {
	if ctx == nil || tx == nil || dialect == nil || tenant == "" || operationID == "" || fence == 0 || fence > math.MaxInt64 || expectedRevision == 0 || expectedRevision > math.MaxInt64 || !validActivationDigest(digest) {
		return fmt.Errorf("management activation fence validation identity is invalid")
	}
	current, err := readActivationFence(ctx, tx, dialect, tenant, operationID, true)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrManagementActivationFence
	}
	if err != nil {
		return err
	}
	if current.fence != int64(fence) || current.expectedRevision != int64(expectedRevision) || current.digest != digest {
		return ErrManagementActivationFence
	}
	return nil
}

type activationFence struct {
	fence, expectedRevision int64
	digest                  string
}

type activationFenceQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readActivationFence(ctx context.Context, q activationFenceQuery, dialect db.Dialect, tenant, operationID string, lock bool) (activationFence, error) {
	query := "SELECT fence, expected_revision, artifact_digest FROM _kora_management_activation_fence WHERE site = ? AND operation_id = ?"
	if lock && (dialect.DriverName() == "mysql" || dialect.DriverName() == "postgres") {
		query += " FOR UPDATE"
	}
	var value activationFence
	err := q.QueryRowContext(ctx, db.Rebind(dialect, query), tenant, operationID).Scan(&value.fence, &value.expectedRevision, &value.digest)
	if err != nil {
		return activationFence{}, fmt.Errorf("read management activation fence: %w", err)
	}
	return value, nil
}

func validActivationDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, ch := range value {
		if !(ch >= '0' && ch <= '9') && !(ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}
