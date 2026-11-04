package configstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

const (
	ActivationRequested = "requested"
	ActivationSnapshot  = "snapshot_saved"
	ActivationApplying  = "applying"
	ActivationValidated = "validated"
	ActivationActivated = "activated"
	ActivationCompleted = "completed"
	ActivationFailed    = "failed"
)

// ActivationOperation records enough immutable input and progress to retry a
// config activation by its original version identity after a process restart.
type ActivationOperation struct {
	Site           string
	VersionID      string
	Kind           string
	Phase          string
	TargetSnapshot string
	DDLPlan        string
	RecoveryPlan   string
	LastError      string
}

func ActivationJournalSchema(dialect db.Dialect) []string {
	switch dialect.DriverName() {
	case "postgres":
		return []string{`CREATE TABLE IF NOT EXISTS _kora_config_operation (
			site TEXT NOT NULL, version_id TEXT NOT NULL, kind TEXT NOT NULL,
			phase TEXT NOT NULL, target_snapshot TEXT NOT NULL, ddl_plan TEXT NOT NULL DEFAULT '',
			recovery_plan TEXT NOT NULL DEFAULT '', last_error TEXT NOT NULL DEFAULT '',
			updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (site, version_id, kind))`}
	case "libsql":
		return []string{`CREATE TABLE IF NOT EXISTS _kora_config_operation (
			site TEXT NOT NULL, version_id TEXT NOT NULL, kind TEXT NOT NULL,
			phase TEXT NOT NULL, target_snapshot TEXT NOT NULL, ddl_plan TEXT NOT NULL DEFAULT '',
			recovery_plan TEXT NOT NULL DEFAULT '', last_error TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (site, version_id, kind))`}
	default:
		return []string{`CREATE TABLE IF NOT EXISTS _kora_config_operation (
			site VARCHAR(140) NOT NULL, version_id VARCHAR(36) NOT NULL, kind VARCHAR(24) NOT NULL,
			phase VARCHAR(24) NOT NULL, target_snapshot LONGTEXT NOT NULL, ddl_plan LONGTEXT NOT NULL,
			recovery_plan LONGTEXT NOT NULL, last_error TEXT NOT NULL,
			updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			PRIMARY KEY (site, version_id, kind)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`}
	}
}

// EnsureActivationOperation creates the durable operation record once. A
// retry with the same identity must carry the exact same target snapshot.
func EnsureActivationOperation(database *sql.DB, dialect db.Dialect, site, versionID, kind string, snapshot *doctype.ConfigSnapshot) (*ActivationOperation, error) {
	if database == nil || dialect == nil || snapshot == nil || site == "" || versionID == "" || kind == "" {
		return nil, fmt.Errorf("activation journal requires database, dialect, tenant, version, kind, and snapshot")
	}
	target, err := json.Marshal(snapshot)
	if err != nil {
		return nil, fmt.Errorf("marshal activation snapshot: %w", err)
	}
	query := `INSERT INTO _kora_config_operation
		(site, version_id, kind, phase, target_snapshot, ddl_plan, recovery_plan, last_error)
		VALUES (?, ?, ?, ?, ?, '', '', '')`
	switch dialect.DriverName() {
	case "mysql":
		query = `INSERT IGNORE` + query[len("INSERT"):]
	case "libsql":
		query = `INSERT OR IGNORE` + query[len("INSERT"):]
	case "postgres":
		query += ` ON CONFLICT (site, version_id, kind) DO NOTHING`
	}
	if _, err := database.Exec(db.Rebind(dialect, query), site, versionID, kind, ActivationRequested, string(target)); err != nil {
		return nil, fmt.Errorf("persist activation request: %w", err)
	}
	op, err := LoadActivationOperation(database, dialect, site, versionID, kind)
	if err != nil {
		return nil, err
	}
	if op.TargetSnapshot != string(target) {
		return nil, fmt.Errorf("activation identity %q already exists with different target content", versionID)
	}
	return op, nil
}

func LoadActivationOperation(database *sql.DB, dialect db.Dialect, site, versionID, kind string) (*ActivationOperation, error) {
	op := &ActivationOperation{}
	err := database.QueryRow(db.Rebind(dialect, `SELECT site, version_id, kind, phase, target_snapshot,
		COALESCE(ddl_plan, ''), COALESCE(recovery_plan, ''), COALESCE(last_error, '')
		FROM _kora_config_operation WHERE site = ? AND version_id = ? AND kind = ?`), site, versionID, kind).
		Scan(&op.Site, &op.VersionID, &op.Kind, &op.Phase, &op.TargetSnapshot, &op.DDLPlan, &op.RecoveryPlan, &op.LastError)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, fmt.Errorf("load activation journal: %w", err)
	}
	return op, nil
}

func UpdateActivationOperation(database *sql.DB, dialect db.Dialect, site, versionID, kind, phase, ddlPlan, recoveryPlan, lastError string) error {
	result, err := database.Exec(db.Rebind(dialect, `UPDATE _kora_config_operation SET phase = ?, ddl_plan = ?, recovery_plan = ?, last_error = ?, updated_at = CURRENT_TIMESTAMP WHERE site = ? AND version_id = ? AND kind = ? AND (phase NOT IN (?, ?) OR (? = ? AND phase = ?))`), phase, ddlPlan, recoveryPlan, lastError, site, versionID, kind, ActivationActivated, ActivationCompleted, phase, ActivationCompleted, ActivationActivated)
	if err != nil {
		return fmt.Errorf("update activation journal: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("verify activation journal update: %w", err)
	} else if affected == 0 {
		var currentPhase string
		if err := database.QueryRow(db.Rebind(dialect, `SELECT phase FROM _kora_config_operation WHERE site = ? AND version_id = ? AND kind = ?`), site, versionID, kind).Scan(&currentPhase); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("activation journal disappeared for %q", versionID)
			}
			return fmt.Errorf("verify activation journal state: %w", err)
		}
		if currentPhase == phase {
			return nil
		}
		return fmt.Errorf("activation operation %q is already terminal in phase %q", versionID, currentPhase)
	}
	return nil
}

func UpdateActivationOperationTx(tx *sql.Tx, dialect db.Dialect, site, versionID, kind, phase, ddlPlan, recoveryPlan string) error {
	result, err := tx.Exec(db.Rebind(dialect, `UPDATE _kora_config_operation SET phase = ?, ddl_plan = ?, recovery_plan = ?, last_error = '', updated_at = CURRENT_TIMESTAMP WHERE site = ? AND version_id = ? AND kind = ? AND phase = ?`), phase, ddlPlan, recoveryPlan, site, versionID, kind, ActivationValidated)
	if err != nil {
		return fmt.Errorf("update activation journal transactionally: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("verify transactional activation journal update: %w", err)
	} else if affected != 1 {
		return fmt.Errorf("activation journal disappeared for %q", versionID)
	}
	return nil
}

// UpdateActivationOperationPhaseTx advances a non-terminal operation in the
// same transaction as its associated config-version writes. It is used by
// config import so a retry can distinguish partial upserts from a committed
// version without creating duplicate history rows.
func UpdateActivationOperationPhaseTx(tx *sql.Tx, dialect db.Dialect, site, operationID, kind, fromPhase, phase, recoveryPlan string) error {
	result, err := tx.Exec(db.Rebind(dialect, `UPDATE _kora_config_operation
		SET phase = ?, recovery_plan = ?, last_error = '', updated_at = CURRENT_TIMESTAMP
		WHERE site = ? AND version_id = ? AND kind = ? AND phase = ?`), phase, recoveryPlan, site, operationID, kind, fromPhase)
	if err != nil {
		return fmt.Errorf("advance config operation phase transactionally: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("verify config operation phase update: %w", err)
	} else if affected != 1 {
		return fmt.Errorf("config operation %q is no longer in phase %q", operationID, fromPhase)
	}
	return nil
}

// CompleteManagementActivation records successful local runtime convergence
// only for the current worker fence. Activation and runtime replacement span
// separate in-memory/SQL steps, so a retry may safely finish this phase after
// rechecking the already-committed activation journal.
func CompleteManagementActivation(ctx context.Context, database *sql.DB, dialect db.Dialect, site, operationID string, fence, expectedRevision uint64, digest string) error {
	if ctx == nil || database == nil || dialect == nil || site == "" || operationID == "" {
		return fmt.Errorf("management activation completion requires context, database, dialect, tenant, and operation")
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin management activation completion: %w", err)
	}
	defer tx.Rollback()
	if err := ValidateManagementActivationFenceTx(ctx, tx, dialect, site, operationID, fence, expectedRevision, digest); err != nil {
		return err
	}
	query := `SELECT phase FROM _kora_config_operation WHERE site = ? AND version_id = ? AND kind = ?`
	if dialect.DriverName() == "mysql" || dialect.DriverName() == "postgres" {
		query += " FOR UPDATE"
	}
	var phase string
	if err := tx.QueryRowContext(ctx, db.Rebind(dialect, query), site, operationID, "management_package").Scan(&phase); err != nil {
		return fmt.Errorf("read management activation phase before completion: %w", err)
	}
	if phase == ActivationCompleted {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit idempotent management activation completion: %w", err)
		}
		return nil
	}
	if phase != ActivationActivated {
		return fmt.Errorf("management activation cannot complete from phase %q", phase)
	}
	if err := UpdateActivationOperationPhaseTx(tx, dialect, site, operationID, "management_package", ActivationActivated, ActivationCompleted, "runtime generation replaced"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit management activation completion: %w", err)
	}
	return nil
}
