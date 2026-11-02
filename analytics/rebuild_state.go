package analytics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
)

type rebuildCheckpoint struct {
	doctype  string
	from     string
	sequence int64
	scope    contract.BusinessScope
}

// lockRebuildStateTx serializes an analytics rebuild with rollup flushes for
// one tenant. Business writers use the independent source-sequence row, so the
// analytics lock never holds up business transactions or outbox publication.
func lockRebuildStateTx(ctx context.Context, tx *sql.Tx, dialect db.QueryDialect, site string) ([]rebuildCheckpoint, error) {
	driver := driverName(dialect)
	insert := `INSERT INTO _kora_analytics_rebuild_lock (site, fence) VALUES (?, 0)`
	if driver == "mysql" {
		insert += ` ON DUPLICATE KEY UPDATE site = VALUES(site)`
	} else {
		insert += ` ON CONFLICT (site) DO NOTHING`
	}
	if _, err := tx.ExecContext(ctx, db.Rebind(dialect, insert), site); err != nil {
		return nil, fmt.Errorf("ensure analytics rebuild lock: %w", err)
	}
	result, err := tx.ExecContext(ctx, db.Rebind(dialect,
		`UPDATE _kora_analytics_rebuild_lock SET fence = fence + 1 WHERE site = ?`), site)
	if err != nil {
		return nil, fmt.Errorf("lock analytics rebuild state: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil {
		return nil, fmt.Errorf("verify analytics rebuild lock: %w", err)
	} else if rows != 1 {
		return nil, fmt.Errorf("analytics rebuild lock row missing for tenant %q", site)
	}
	rows, err := tx.QueryContext(ctx, db.Rebind(dialect,
		`SELECT doctype, from_date, last_source_sequence, scope_kind, legal_entity_id FROM _kora_analytics_rebuild_checkpoint WHERE site = ?`), site)
	if err != nil {
		return nil, fmt.Errorf("load analytics rebuild checkpoints: %w", err)
	}
	defer rows.Close()
	var checkpoints []rebuildCheckpoint
	for rows.Next() {
		var checkpoint rebuildCheckpoint
		if err := rows.Scan(&checkpoint.doctype, &checkpoint.from, &checkpoint.sequence, &checkpoint.scope.Kind, &checkpoint.scope.LegalEntityID); err != nil {
			return nil, fmt.Errorf("scan analytics rebuild checkpoint: %w", err)
		}
		checkpoint.scope.TenantID = contract.TenantID(site)
		checkpoints = append(checkpoints, checkpoint)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate analytics rebuild checkpoints: %w", err)
	}
	return checkpoints, nil
}

func currentSourceSequenceTx(ctx context.Context, tx *sql.Tx, dialect db.QueryDialect) (int64, error) {
	var sequence int64
	err := tx.QueryRowContext(ctx, db.Rebind(dialect,
		`SELECT last_sequence FROM _kora_tenant_event_sequence WHERE id = 1`)).Scan(&sequence)
	if errors.Is(err, sql.ErrNoRows) {
		// Before the first post-migration outbox write, existing installations
		// have only route-keyed sequence rows. Their maximum is the safe cutoff
		// for this isolated tenant database and seeds the singleton on append.
		err = tx.QueryRowContext(ctx, db.Rebind(dialect,
			`SELECT COALESCE(MAX(last_sequence), 0) FROM _kora_event_sequence`)).Scan(&sequence)
	}
	if err != nil {
		return 0, fmt.Errorf("read analytics source cutoff: %w", err)
	}
	return sequence, nil
}

func rebuildFenceTx(ctx context.Context, tx *sql.Tx, dialect db.QueryDialect, site string) (int64, error) {
	var fence int64
	err := tx.QueryRowContext(ctx, db.Rebind(dialect, `SELECT fence FROM _kora_analytics_rebuild_lock WHERE site = ?`), site).Scan(&fence)
	if err != nil {
		return 0, fmt.Errorf("read analytics rebuild fence: %w", err)
	}
	return fence, nil
}

func saveRebuildCheckpointTx(ctx context.Context, tx *sql.Tx, dialect db.QueryDialect, site, doctype, from string, sequence int64, scopes ...contract.BusinessScope) error {
	scope := contract.BusinessScope{TenantID: contract.TenantID(site), Kind: contract.ScopeKindTenant}
	if len(scopes) > 0 {
		scope = scopes[0]
	}
	query := `INSERT INTO _kora_analytics_rebuild_checkpoint (site, doctype, from_date, scope_kind, legal_entity_id, last_source_sequence, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`
	if driverName(dialect) == "mysql" {
		query += ` ON DUPLICATE KEY UPDATE last_source_sequence = VALUES(last_source_sequence), updated_at = VALUES(updated_at)`
	} else {
		query += ` ON CONFLICT (site, doctype, from_date, scope_kind, legal_entity_id) DO UPDATE SET last_source_sequence = excluded.last_source_sequence, updated_at = excluded.updated_at`
	}
	if _, err := tx.ExecContext(ctx, db.Rebind(dialect, query), site, doctype, from, string(scope.Kind), scope.LegalEntityID, sequence, time.Now().UTC()); err != nil {
		return fmt.Errorf("save analytics rebuild checkpoint: %w", err)
	}
	return nil
}

func driverName(dialect db.QueryDialect) string {
	if named, ok := dialect.(interface{ DriverName() string }); ok {
		return named.DriverName()
	}
	return ""
}

func eventAtOrBeforeSequence(eventSequence string, checkpointSequence int64) (bool, error) {
	if eventSequence == "" {
		return false, nil
	}
	sequence, err := strconv.ParseInt(eventSequence, 10, 64)
	if err != nil || sequence < 0 || strconv.FormatInt(sequence, 10) != eventSequence {
		return false, fmt.Errorf("invalid analytics event sequence %q", eventSequence)
	}
	return sequence <= checkpointSequence, nil
}

func contributionOutsideRebuiltRange(contribution eventContribution, eventSequence string, checkpoints []rebuildCheckpoint) (map[deltaKey]float64, error) {
	accepted := make(map[deltaKey]float64, len(contribution.deltas))
	for key, delta := range contribution.deltas {
		skip := false
		for _, checkpoint := range checkpoints {
			if checkpoint.doctype != "" && checkpoint.doctype != key.Doctype || key.Date < checkpoint.from || checkpoint.scope.TenantID != "" && checkpoint.scope != key.BusinessScope {
				continue
			}
			atOrBefore, err := eventAtOrBeforeSequence(eventSequence, checkpoint.sequence)
			if err != nil {
				return nil, err
			}
			if atOrBefore {
				skip = true
				break
			}
		}
		if !skip {
			accepted[key] += delta
		}
	}
	return accepted, nil
}
