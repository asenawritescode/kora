package site

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	kdb "github.com/asenawritescode/kora/db"
)

// SiteRegistry is the Engine-owned directory boundary. DBSiteInfo is internal
// to Engine; callers must not expose it as an API model because it can contain
// resolved database credentials.
type SiteRegistry interface {
	GetSnapshot() ([]DBSiteInfo, error)
	GetSnapshotForCell(cellID string) ([]DBSiteInfo, error)
	GetDirectorySnapshot() ([]DBSiteInfo, error)
	GetDirectorySnapshotForCell(cellID string) ([]DBSiteInfo, error)
	ResolveAlias(alias string) (DBSiteInfo, error)
	GetByID(siteID string) (DBSiteInfo, error)
	GetDescriptorByID(siteID string) (SiteDescriptor, error)
	Upsert(config *SiteConfig) error
	SetStatus(siteID, status string) error
	SetRuntimeCellID(siteID, cellID string) (SiteDescriptor, error)
	ChangesAfter(cursor uint64, limit int) ([]SiteDirectoryChange, error)
	CurrentDirectoryRevision() (uint64, error)
	LoadConsumerCursor(consumerID string, initialCursor uint64) (uint64, error)
	AcquireConsumerLease(consumerID, ownerID string, duration time.Duration) error
	VerifyConsumerLease(consumerID, ownerID string) error
	RenewConsumerLease(consumerID, ownerID string, duration time.Duration) error
	ReleaseConsumerLease(consumerID, ownerID string) error
	AdvanceConsumerCursor(consumerID, ownerID string, expected, next uint64) error
	ReconcileConsumerCursor(consumerID, ownerID string, expected, snapshotRevision uint64) error
	PruneOutboxBefore(cutoff time.Time) (int64, error)
}

var ErrDirectoryCursorConflict = errors.New("site directory consumer cursor changed concurrently")
var ErrDirectoryConsumerLeaseHeld = errors.New("site directory consumer lease is held by another replica")
var ErrDirectoryConsumerLeaseLost = errors.New("site directory consumer lease was lost")

// CurrentDirectoryRevision returns the feed position. Engine startup captures
// this before reading/loading its eager snapshot and uses it as the initial
// cursor, so all changes committed after the boundary are replayed.
func (r *SQLSiteRegistry) CurrentDirectoryRevision() (uint64, error) {
	return r.CurrentDirectoryRevisionContext(context.Background())
}

func (r *SQLSiteRegistry) CurrentDirectoryRevisionContext(ctx context.Context) (uint64, error) {
	if r == nil || r.database == nil {
		return 0, errors.New("site registry unavailable")
	}
	if ctx == nil {
		return 0, errors.New("reading directory revision requires context")
	}
	var cursor uint64
	if err := r.database.QueryRowContext(ctx, `SELECT directory_revision FROM _kora_site_directory_meta WHERE id = 1`).Scan(&cursor); err != nil {
		return 0, fmt.Errorf("read current site directory revision: %w", err)
	}
	return cursor, nil
}

// LoadConsumerCursor creates a durable cursor at the startup boundary on first
// use. Existing consumer cursors are never reset, preserving restart progress.
func (r *SQLSiteRegistry) LoadConsumerCursor(consumerID string, initialCursor uint64) (uint64, error) {
	return r.LoadConsumerCursorContext(context.Background(), consumerID, initialCursor)
}

func (r *SQLSiteRegistry) LoadConsumerCursorContext(ctx context.Context, consumerID string, initialCursor uint64) (uint64, error) {
	if r == nil || r.database == nil || strings.TrimSpace(consumerID) == "" {
		return 0, errors.New("site directory consumer id and registry are required")
	}
	insert := `INSERT INTO _kora_site_directory_consumers (consumer_id, directory_revision) VALUES (?, ?) ON DUPLICATE KEY UPDATE consumer_id = VALUES(consumer_id)`
	selectQuery := `SELECT directory_revision FROM _kora_site_directory_consumers WHERE consumer_id = ?`
	if strings.EqualFold(r.dialect, "postgres") || strings.EqualFold(r.dialect, "libsql") {
		insert = `INSERT INTO _kora_site_directory_consumers (consumer_id, directory_revision) VALUES (?, ?) ON CONFLICT (consumer_id) DO NOTHING`
	}
	if strings.EqualFold(r.dialect, "postgres") {
		insert = `INSERT INTO _kora_site_directory_consumers (consumer_id, directory_revision) VALUES ($1, $2) ON CONFLICT (consumer_id) DO NOTHING`
		selectQuery = `SELECT directory_revision FROM _kora_site_directory_consumers WHERE consumer_id = $1`
	}
	if strings.EqualFold(r.dialect, "mysql") {
		insert = `INSERT IGNORE INTO _kora_site_directory_consumers (consumer_id, directory_revision) VALUES (?, ?)`
	}
	if _, err := r.database.ExecContext(ctx, insert, consumerID, initialCursor); err != nil {
		return 0, fmt.Errorf("initialize site directory consumer cursor: %w", err)
	}
	var cursor uint64
	if err := r.database.QueryRowContext(ctx, selectQuery, consumerID).Scan(&cursor); err != nil {
		return 0, fmt.Errorf("read site directory consumer cursor: %w", err)
	}
	return cursor, nil
}

// AcquireConsumerLease elects one process as the owner of a durable consumer
// cursor. The database clock arbitrates expiry so replicas with skewed clocks
// cannot steal a live lease. Reacquisition by the same owner renews its lease.
func (r *SQLSiteRegistry) AcquireConsumerLease(consumerID, ownerID string, duration time.Duration) error {
	return r.AcquireConsumerLeaseContext(context.Background(), consumerID, ownerID, duration)
}

func (r *SQLSiteRegistry) AcquireConsumerLeaseContext(ctx context.Context, consumerID, ownerID string, duration time.Duration) error {
	if r == nil || r.database == nil || strings.TrimSpace(consumerID) == "" || strings.TrimSpace(ownerID) == "" || duration < time.Second {
		return errors.New("site directory consumer, owner, registry, and lease duration of at least one second are required")
	}
	seconds := int64(duration / time.Second)
	query := `UPDATE _kora_site_directory_consumers SET lease_owner = ?, lease_expires_at = TIMESTAMPADD(SECOND, ?, CURRENT_TIMESTAMP(6)), updated_at = CURRENT_TIMESTAMP(6) WHERE consumer_id = ? AND (lease_owner = ? OR lease_owner IS NULL OR lease_expires_at IS NULL OR lease_expires_at <= CURRENT_TIMESTAMP(6))`
	args := []any{ownerID, seconds, consumerID, ownerID}
	switch strings.ToLower(r.dialect) {
	case "postgres":
		query = `UPDATE _kora_site_directory_consumers SET lease_owner = $1, lease_expires_at = CURRENT_TIMESTAMP + ($2 * INTERVAL '1 second'), updated_at = CURRENT_TIMESTAMP WHERE consumer_id = $3 AND (lease_owner = $1 OR lease_owner IS NULL OR lease_expires_at IS NULL OR lease_expires_at <= CURRENT_TIMESTAMP)`
		args = []any{ownerID, seconds, consumerID}
	case "libsql":
		query = `UPDATE _kora_site_directory_consumers SET lease_owner = ?, lease_expires_at = datetime('now', '+' || ? || ' seconds'), updated_at = datetime('now') WHERE consumer_id = ? AND (lease_owner = ? OR lease_owner IS NULL OR lease_expires_at IS NULL OR datetime(lease_expires_at) <= datetime('now'))`
	}
	result, err := r.database.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("acquire site directory consumer lease: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read site directory consumer lease result: %w", err)
	}
	if changed == 0 {
		return ErrDirectoryConsumerLeaseHeld
	}
	return nil
}

// VerifyConsumerLease fences a process that lost ownership before it reads or
// applies another outbox record. This read-only check avoids turning every
// short poll into a lease heartbeat write.
func (r *SQLSiteRegistry) VerifyConsumerLease(consumerID, ownerID string) error {
	return r.VerifyConsumerLeaseContext(context.Background(), consumerID, ownerID)
}

func (r *SQLSiteRegistry) VerifyConsumerLeaseContext(ctx context.Context, consumerID, ownerID string) error {
	if r == nil || r.database == nil || strings.TrimSpace(consumerID) == "" || strings.TrimSpace(ownerID) == "" {
		return errors.New("site directory consumer, owner, and registry are required")
	}
	query := `SELECT 1 FROM _kora_site_directory_consumers WHERE consumer_id = ? AND lease_owner = ? AND lease_expires_at > CURRENT_TIMESTAMP`
	if strings.EqualFold(r.dialect, "postgres") {
		query = `SELECT 1 FROM _kora_site_directory_consumers WHERE consumer_id = $1 AND lease_owner = $2 AND lease_expires_at > CURRENT_TIMESTAMP`
	} else if strings.EqualFold(r.dialect, "libsql") {
		query = `SELECT 1 FROM _kora_site_directory_consumers WHERE consumer_id = ? AND lease_owner = ? AND datetime(lease_expires_at) > datetime('now')`
	}
	var valid int
	if err := r.database.QueryRowContext(ctx, query, consumerID, ownerID).Scan(&valid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrDirectoryConsumerLeaseLost
		}
		return fmt.Errorf("verify site directory consumer lease: %w", err)
	}
	return nil
}

// RenewConsumerLease refreshes both the liveness timestamp used by compaction
// and the exclusive lease. It only succeeds for the current fencing owner.
func (r *SQLSiteRegistry) RenewConsumerLease(consumerID, ownerID string, duration time.Duration) error {
	return r.RenewConsumerLeaseContext(context.Background(), consumerID, ownerID, duration)
}

func (r *SQLSiteRegistry) RenewConsumerLeaseContext(ctx context.Context, consumerID, ownerID string, duration time.Duration) error {
	if r == nil || r.database == nil || strings.TrimSpace(consumerID) == "" || strings.TrimSpace(ownerID) == "" || duration < time.Second {
		return errors.New("site directory consumer, owner, registry, and lease duration of at least one second are required")
	}
	seconds := int64(duration / time.Second)
	query := `UPDATE _kora_site_directory_consumers SET lease_expires_at = TIMESTAMPADD(SECOND, ?, CURRENT_TIMESTAMP(6)), updated_at = CURRENT_TIMESTAMP(6) WHERE consumer_id = ? AND lease_owner = ?`
	args := []any{seconds, consumerID, ownerID}
	switch strings.ToLower(r.dialect) {
	case "postgres":
		query = `UPDATE _kora_site_directory_consumers SET lease_expires_at = CURRENT_TIMESTAMP + ($1 * INTERVAL '1 second'), updated_at = CURRENT_TIMESTAMP WHERE consumer_id = $2 AND lease_owner = $3`
	case "libsql":
		query = `UPDATE _kora_site_directory_consumers SET lease_expires_at = datetime('now', '+' || ? || ' seconds'), updated_at = datetime('now') WHERE consumer_id = ? AND lease_owner = ?`
	}
	result, err := r.database.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("renew site directory consumer lease: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read renewed site directory consumer lease result: %w", err)
	}
	if changed == 0 {
		return ErrDirectoryConsumerLeaseLost
	}
	return nil
}

func (r *SQLSiteRegistry) ReleaseConsumerLease(consumerID, ownerID string) error {
	return r.ReleaseConsumerLeaseContext(context.Background(), consumerID, ownerID)
}

func (r *SQLSiteRegistry) ReleaseConsumerLeaseContext(ctx context.Context, consumerID, ownerID string) error {
	if r == nil || r.database == nil || strings.TrimSpace(consumerID) == "" || strings.TrimSpace(ownerID) == "" {
		return errors.New("site directory consumer, owner, and registry are required")
	}
	query := `UPDATE _kora_site_directory_consumers SET lease_owner = NULL, lease_expires_at = NULL WHERE consumer_id = ? AND lease_owner = ?`
	if strings.EqualFold(r.dialect, "postgres") {
		query = `UPDATE _kora_site_directory_consumers SET lease_owner = NULL, lease_expires_at = NULL WHERE consumer_id = $1 AND lease_owner = $2`
	}
	if _, err := r.database.ExecContext(ctx, query, consumerID, ownerID); err != nil {
		return fmt.Errorf("release site directory consumer lease: %w", err)
	}
	return nil
}

// AdvanceConsumerCursor commits exactly one applied revision. If another
// worker using the same consumer identity won the race, only an identical
// already-committed value is accepted; gaps and stale writes are rejected.
func (r *SQLSiteRegistry) AdvanceConsumerCursor(consumerID, ownerID string, expected, next uint64) error {
	return r.AdvanceConsumerCursorContext(context.Background(), consumerID, ownerID, expected, next)
}

func (r *SQLSiteRegistry) AdvanceConsumerCursorContext(ctx context.Context, consumerID, ownerID string, expected, next uint64) error {
	if r == nil || r.database == nil || strings.TrimSpace(consumerID) == "" || strings.TrimSpace(ownerID) == "" || next != expected+1 {
		return errors.New("consumer cursor must advance one revision at a time")
	}
	query := `UPDATE _kora_site_directory_consumers SET directory_revision = ?, updated_at = CURRENT_TIMESTAMP WHERE consumer_id = ? AND lease_owner = ? AND lease_expires_at > CURRENT_TIMESTAMP AND directory_revision = ?`
	args := []any{next, consumerID, ownerID, expected}
	if strings.EqualFold(r.dialect, "postgres") {
		query = `UPDATE _kora_site_directory_consumers SET directory_revision = $1, updated_at = CURRENT_TIMESTAMP WHERE consumer_id = $2 AND lease_owner = $3 AND lease_expires_at > CURRENT_TIMESTAMP AND directory_revision = $4`
	} else if strings.EqualFold(r.dialect, "libsql") {
		query = `UPDATE _kora_site_directory_consumers SET directory_revision = ?, updated_at = datetime('now') WHERE consumer_id = ? AND lease_owner = ? AND datetime(lease_expires_at) > datetime('now') AND directory_revision = ?`
	}
	result, err := r.database.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("advance site directory consumer cursor: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read site directory cursor update result: %w", err)
	}
	if changed > 0 {
		return nil
	}
	current, currentOwner, leaseActive, err := r.readConsumerFenceContext(ctx, consumerID)
	if err != nil {
		return err
	}
	if currentOwner != ownerID || !leaseActive {
		return ErrDirectoryConsumerLeaseLost
	}
	if current == next {
		return nil
	}
	return fmt.Errorf("consumer %q cursor is %d; expected %d: %w", consumerID, current, expected, ErrDirectoryCursorConflict)
}

// ReconcileConsumerCursor may jump forward only after the caller has applied a
// complete authoritative snapshot at snapshotRevision. Its compare-and-swap
// prevents a concurrent consumer from having its progress overwritten.
func (r *SQLSiteRegistry) ReconcileConsumerCursor(consumerID, ownerID string, expected, snapshotRevision uint64) error {
	return r.ReconcileConsumerCursorContext(context.Background(), consumerID, ownerID, expected, snapshotRevision)
}

func (r *SQLSiteRegistry) ReconcileConsumerCursorContext(ctx context.Context, consumerID, ownerID string, expected, snapshotRevision uint64) error {
	if r == nil || r.database == nil || strings.TrimSpace(consumerID) == "" || strings.TrimSpace(ownerID) == "" || snapshotRevision < expected {
		return errors.New("invalid site directory cursor reconciliation")
	}
	query := `UPDATE _kora_site_directory_consumers SET directory_revision = ?, updated_at = CURRENT_TIMESTAMP WHERE consumer_id = ? AND lease_owner = ? AND lease_expires_at > CURRENT_TIMESTAMP AND directory_revision = ?`
	args := []any{snapshotRevision, consumerID, ownerID, expected}
	if strings.EqualFold(r.dialect, "postgres") {
		query = `UPDATE _kora_site_directory_consumers SET directory_revision = $1, updated_at = CURRENT_TIMESTAMP WHERE consumer_id = $2 AND lease_owner = $3 AND lease_expires_at > CURRENT_TIMESTAMP AND directory_revision = $4`
	} else if strings.EqualFold(r.dialect, "libsql") {
		query = `UPDATE _kora_site_directory_consumers SET directory_revision = ?, updated_at = datetime('now') WHERE consumer_id = ? AND lease_owner = ? AND datetime(lease_expires_at) > datetime('now') AND directory_revision = ?`
	}
	result, err := r.database.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("reconcile site directory cursor: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read reconciled site directory cursor: %w", err)
	}
	if changed > 0 || snapshotRevision == expected {
		return nil
	}
	current, currentOwner, leaseActive, err := r.readConsumerFenceContext(ctx, consumerID)
	if err != nil {
		return err
	}
	if currentOwner != ownerID || !leaseActive {
		return ErrDirectoryConsumerLeaseLost
	}
	if current == snapshotRevision {
		return nil
	}
	return fmt.Errorf("consumer %q cursor is %d; expected %d: %w", consumerID, current, expected, ErrDirectoryCursorConflict)
}

func (r *SQLSiteRegistry) readConsumerFence(consumerID string) (uint64, string, bool, error) {
	return r.readConsumerFenceContext(context.Background(), consumerID)
}

func (r *SQLSiteRegistry) readConsumerFenceContext(ctx context.Context, consumerID string) (uint64, string, bool, error) {
	query := `SELECT directory_revision, lease_owner, lease_expires_at > CURRENT_TIMESTAMP FROM _kora_site_directory_consumers WHERE consumer_id = ?`
	if strings.EqualFold(r.dialect, "postgres") {
		query = `SELECT directory_revision, lease_owner, lease_expires_at > CURRENT_TIMESTAMP FROM _kora_site_directory_consumers WHERE consumer_id = $1`
	} else if strings.EqualFold(r.dialect, "libsql") {
		query = `SELECT directory_revision, lease_owner, datetime(lease_expires_at) > datetime('now') FROM _kora_site_directory_consumers WHERE consumer_id = ?`
	}
	var cursor uint64
	var owner sql.NullString
	var active sql.NullBool
	if err := r.database.QueryRowContext(ctx, query, consumerID).Scan(&cursor, &owner, &active); err != nil {
		return 0, "", false, fmt.Errorf("read site directory consumer fencing state: %w", err)
	}
	return cursor, owner.String, active.Valid && active.Bool, nil
}

// PruneOutboxBefore removes records older than cutoff that have been applied
// by every live consumer. Consumers that have not polled within the retention
// window are treated as inactive; their durable cursors remain and force a
// snapshot reconciliation if they return after history has been compacted.
func (r *SQLSiteRegistry) PruneOutboxBefore(cutoff time.Time) (int64, error) {
	return r.PruneOutboxBeforeContext(context.Background(), cutoff)
}

// PruneOutboxBeforeContext is the cancellation-aware form of
// PruneOutboxBefore. Callers should apply platform work admission before this
// transaction because pruning competes with directory publication/consumers.
func (r *SQLSiteRegistry) PruneOutboxBeforeContext(ctx context.Context, cutoff time.Time) (int64, error) {
	if r == nil || r.database == nil {
		return 0, errors.New("site registry unavailable")
	}
	if ctx == nil {
		return 0, errors.New("site directory pruning requires a context")
	}
	tx, err := r.database.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin site directory compaction: %w", err)
	}
	defer tx.Rollback()
	var eligible, latest uint64
	eligibleQuery := `SELECT COALESCE(MAX(directory_revision), 0) FROM _kora_site_directory_outbox WHERE created_at < ?`
	if strings.EqualFold(r.dialect, "postgres") {
		eligibleQuery = `SELECT COALESCE(MAX(directory_revision), 0) FROM _kora_site_directory_outbox WHERE created_at < $1`
	}
	if err := tx.QueryRowContext(ctx, eligibleQuery, cutoff).Scan(&eligible); err != nil {
		return 0, fmt.Errorf("read eligible directory history: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT directory_revision FROM _kora_site_directory_meta WHERE id = 1`).Scan(&latest); err != nil {
		return 0, fmt.Errorf("read current directory revision: %w", err)
	}
	var minimum sql.NullInt64
	minimumQuery := `SELECT MIN(directory_revision) FROM _kora_site_directory_consumers WHERE updated_at >= ?`
	if strings.EqualFold(r.dialect, "postgres") {
		minimumQuery = `SELECT MIN(directory_revision) FROM _kora_site_directory_consumers WHERE updated_at >= $1`
	} else if strings.EqualFold(r.dialect, "libsql") {
		minimumQuery = `SELECT MIN(directory_revision) FROM _kora_site_directory_consumers WHERE datetime(updated_at) >= datetime(?)`
	}
	if err := tx.QueryRowContext(ctx, minimumQuery, cutoff).Scan(&minimum); err != nil {
		return 0, fmt.Errorf("read minimum consumer cursor: %w", err)
	}
	safeThrough := eligible
	if minimum.Valid && uint64(minimum.Int64) < safeThrough {
		safeThrough = uint64(minimum.Int64)
	}
	if safeThrough > latest {
		return 0, fmt.Errorf("eligible directory cursor %d exceeds current revision %d", safeThrough, latest)
	}
	if safeThrough == 0 {
		return 0, tx.Commit()
	}
	query := `DELETE FROM _kora_site_directory_outbox WHERE directory_revision <= ? AND created_at < ?`
	args := []any{safeThrough, cutoff}
	if strings.EqualFold(r.dialect, "postgres") {
		query = `DELETE FROM _kora_site_directory_outbox WHERE directory_revision <= $1 AND created_at < $2`
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("prune site directory outbox: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read pruned directory row count: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit site directory compaction: %w", err)
	}
	return deleted, nil
}

// SQLSiteRegistry is the default OSS implementation. It depends only on the
// configured platform SQL database; Cloud and distributed services are absent.
type SQLSiteRegistry struct {
	database *sql.DB
	dialect  string
}

var _ SiteRegistry = (*SQLSiteRegistry)(nil)
var _ DirectoryChangeFeed = (*SQLSiteRegistry)(nil)
var _ ContextDirectoryChangeFeed = (*SQLSiteRegistry)(nil)

func NewSQLSiteRegistry(database *sql.DB, dialect string) *SQLSiteRegistry {
	return &SQLSiteRegistry{database: database, dialect: dialect}
}

func (r *SQLSiteRegistry) GetSnapshot() ([]DBSiteInfo, error) {
	if r == nil || r.database == nil {
		return nil, errors.New("site registry unavailable")
	}
	return DiscoverSitesFromDB(r.database)
}

// GetSnapshotForCell returns the active sites assigned to one Engine cell.
// An empty cell ID preserves the single-node OSS behavior (load every site).
// Unassigned legacy rows belong to the default cell only.
func (r *SQLSiteRegistry) GetSnapshotForCell(cellID string) ([]DBSiteInfo, error) {
	if r == nil || r.database == nil {
		return nil, errors.New("site registry unavailable")
	}
	return discoverRegistrySitesForCell(r.database, true, cellID, r.dialect)
}

// GetDirectorySnapshot returns every persisted lifecycle state (including
// suspended/deleting sites) for reconciliation; GetSnapshot remains the
// active-site startup view.
func (r *SQLSiteRegistry) GetDirectorySnapshot() ([]DBSiteInfo, error) {
	if r == nil || r.database == nil {
		return nil, errors.New("site registry unavailable")
	}
	return discoverAllSitesFromRegistry(r.database)
}

// GetDirectorySnapshotForCell returns every lifecycle state for the sites this
// Engine cell owns, without reading credentials for sites assigned elsewhere.
func (r *SQLSiteRegistry) GetDirectorySnapshotForCell(cellID string) ([]DBSiteInfo, error) {
	if r == nil || r.database == nil {
		return nil, errors.New("site registry unavailable")
	}
	return discoverRegistrySitesForCell(r.database, false, cellID, r.dialect)
}

// GetDirectoryDescriptors returns a credential-free projection for Cloud and
// audit consumers. It intentionally does not query or decrypt tenant database
// credentials or storage connection details.
func (r *SQLSiteRegistry) GetDirectoryDescriptors() ([]SiteDescriptor, error) {
	if r == nil || r.database == nil {
		return nil, errors.New("site registry unavailable")
	}
	rows, err := r.database.Query(`SELECT site_id, COALESCE(runtime_cell_id, ''), site, COALESCE(domains_json, '[]'), status, config_revision FROM _kora_site_registry ORDER BY site`)
	if err != nil {
		return nil, fmt.Errorf("read credential-free site directory descriptors: %w", err)
	}
	defer rows.Close()
	var descriptors []SiteDescriptor
	for rows.Next() {
		var info DBSiteInfo
		var domainsJSON string
		if err := rows.Scan(&info.SiteID, &info.RuntimeCellID, &info.Name, &domainsJSON, &info.Status, &info.ConfigRevision); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(domainsJSON), &info.Domains); err != nil {
			return nil, fmt.Errorf("decode aliases for site %s: %w", info.SiteID, err)
		}
		descriptors = append(descriptors, DescriptorFromDBSiteInfo(info))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return descriptors, nil
}

// GetDirectoryDescriptorPageContext reads a bounded credential-free page of
// locally assigned tenant routing metadata. afterSiteID is an exclusive keyset
// cursor; callers persist the last returned ID only after processing the page.
func (r *SQLSiteRegistry) GetDirectoryDescriptorPageContext(ctx context.Context, cellID, afterSiteID string, limit uint16) ([]SiteDescriptor, error) {
	if r == nil || r.database == nil || ctx == nil || limit == 0 || limit > 500 {
		return nil, errors.New("directory page requires registry, context, and a limit from 1 to 500")
	}
	query := `SELECT site_id, COALESCE(runtime_cell_id, ''), site, COALESCE(domains_json, '[]'), status, config_revision FROM _kora_site_registry WHERE site_id > ?`
	args := []any{afterSiteID}
	if strings.TrimSpace(cellID) != "" {
		query += ` AND COALESCE(NULLIF(runtime_cell_id, ''), 'default') = ?`
		args = append(args, strings.TrimSpace(cellID))
	}
	query += ` ORDER BY site_id LIMIT ?`
	args = append(args, int64(limit))
	query = kdb.Rebind(kdb.Resolve(r.dialect), query)
	rows, err := r.database.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read tenant directory page: %w", err)
	}
	defer rows.Close()
	page := make([]SiteDescriptor, 0, limit)
	for rows.Next() {
		var info DBSiteInfo
		var domainsJSON string
		if err := rows.Scan(&info.SiteID, &info.RuntimeCellID, &info.Name, &domainsJSON, &info.Status, &info.ConfigRevision); err != nil {
			return nil, fmt.Errorf("scan tenant directory page: %w", err)
		}
		if err := json.Unmarshal([]byte(domainsJSON), &info.Domains); err != nil {
			return nil, fmt.Errorf("decode aliases for site %s: %w", info.SiteID, err)
		}
		page = append(page, DescriptorFromDBSiteInfo(info))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read tenant directory page rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close tenant directory page: %w", err)
	}
	return page, nil
}

func (r *SQLSiteRegistry) ResolveAlias(alias string) (DBSiteInfo, error) {
	if r == nil || r.database == nil {
		return DBSiteInfo{}, errors.New("site registry unavailable")
	}
	id, err := ResolveSiteAlias(r.database, r.dialect, alias)
	if err != nil {
		return DBSiteInfo{}, err
	}
	return r.GetByID(id)
}

func (r *SQLSiteRegistry) GetByID(siteID string) (DBSiteInfo, error) {
	return r.GetByIDContext(context.Background(), siteID)
}

// GetByIDContext resolves the credential-bearing tenant connection details for
// one directory entry under the caller's bounded scan or activation context.
func (r *SQLSiteRegistry) GetByIDContext(ctx context.Context, siteID string) (DBSiteInfo, error) {
	if r == nil || r.database == nil {
		return DBSiteInfo{}, errors.New("site registry unavailable")
	}
	if ctx == nil || strings.TrimSpace(siteID) == "" {
		return DBSiteInfo{}, errors.New("site id is required")
	}
	query := `SELECT site_id, COALESCE(runtime_cell_id, ''), site, db_type, db_host, db_port, db_name, db_user, COALESCE(db_password, ''), db_password_encrypted, COALESCE(domains_json, '[]'), COALESCE(file_storage, 'local'), COALESCE(storage_bucket, ''), status, config_revision FROM _kora_site_registry WHERE site_id = ?`
	if strings.EqualFold(r.dialect, "postgres") {
		query = `SELECT site_id, COALESCE(runtime_cell_id, ''), site, db_type, db_host, db_port, db_name, db_user, COALESCE(db_password, ''), db_password_encrypted, COALESCE(domains_json, '[]'), COALESCE(file_storage, 'local'), COALESCE(storage_bucket, ''), status, config_revision FROM _kora_site_registry WHERE site_id = $1`
	}
	var info DBSiteInfo
	var encryptedNumeric int
	var domainsJSON string
	if err := r.database.QueryRowContext(ctx, query, siteID).Scan(&info.SiteID, &info.RuntimeCellID, &info.Name, &info.DBType, &info.DBHost, &info.DBPort, &info.DBName, &info.DBUser, &info.DBPassword, &encryptedNumeric, &domainsJSON, &info.FileStorage, &info.StorageBucket, &info.Status, &info.ConfigRevision); err != nil {
		return DBSiteInfo{}, err
	}
	info.DBPasswordEncrypted = encryptedNumeric == 1
	if info.DBPasswordEncrypted && info.DBPassword != "" {
		plain, err := decryptPassword(info.DBPassword)
		if err != nil {
			return DBSiteInfo{}, fmt.Errorf("decrypting db password for site %s: %w", info.Name, err)
		}
		info.DBPassword = plain
	}
	if err := json.Unmarshal([]byte(domainsJSON), &info.Domains); err != nil {
		return DBSiteInfo{}, fmt.Errorf("decode site domains for %s: %w", info.Name, err)
	}
	if len(info.Domains) == 0 {
		info.Domains = []string{info.Name}
	}
	if info.FileStorage == "s3" && info.StorageBucket == "" {
		info.StorageBucket = BucketNameForSite(info.Name)
	}
	return info, nil
}

// GetDescriptorByID reads current directory metadata without touching tenant
// connection coordinates or credentials.
func (r *SQLSiteRegistry) GetDescriptorByID(siteID string) (SiteDescriptor, error) {
	return r.GetDescriptorByIDContext(context.Background(), siteID)
}

func (r *SQLSiteRegistry) GetDescriptorByIDContext(ctx context.Context, siteID string) (SiteDescriptor, error) {
	if r == nil || r.database == nil || strings.TrimSpace(siteID) == "" {
		return SiteDescriptor{}, errors.New("site registry and site id are required")
	}
	if ctx == nil {
		return SiteDescriptor{}, errors.New("reading site descriptor requires context")
	}
	query := `SELECT site_id, COALESCE(runtime_cell_id, ''), site, db_type, db_host, db_port, db_name, COALESCE(domains_json, '[]'), COALESCE(file_storage, 'local'), COALESCE(storage_bucket, ''), status, config_revision FROM _kora_site_registry WHERE site_id = ?`
	if strings.EqualFold(r.dialect, "postgres") {
		query = `SELECT site_id, COALESCE(runtime_cell_id, ''), site, db_type, db_host, db_port, db_name, COALESCE(domains_json, '[]'), COALESCE(file_storage, 'local'), COALESCE(storage_bucket, ''), status, config_revision FROM _kora_site_registry WHERE site_id = $1`
	}
	var info DBSiteInfo
	var domainsJSON string
	if err := r.database.QueryRowContext(ctx, query, siteID).Scan(&info.SiteID, &info.RuntimeCellID, &info.Name, &info.DBType, &info.DBHost, &info.DBPort, &info.DBName, &domainsJSON, &info.FileStorage, &info.StorageBucket, &info.Status, &info.ConfigRevision); err != nil {
		return SiteDescriptor{}, err
	}
	if err := json.Unmarshal([]byte(domainsJSON), &info.Domains); err != nil {
		return SiteDescriptor{}, fmt.Errorf("decode site aliases for %s: %w", siteID, err)
	}
	return DescriptorFromDBSiteInfo(info), nil
}

// ResolveDescriptorByAlias resolves stable identity without reading tenant
// connection coordinates or credentials.
func (r *SQLSiteRegistry) ResolveDescriptorByAlias(alias string) (SiteDescriptor, error) {
	return r.ResolveDescriptorByAliasContext(context.Background(), alias)
}

func (r *SQLSiteRegistry) ResolveDescriptorByAliasContext(ctx context.Context, alias string) (SiteDescriptor, error) {
	if r == nil || r.database == nil {
		return SiteDescriptor{}, errors.New("site registry is required")
	}
	if ctx == nil {
		return SiteDescriptor{}, errors.New("resolving site descriptor requires context")
	}
	siteID, err := ResolveSiteAliasContext(ctx, r.database, r.dialect, alias)
	if err != nil {
		return SiteDescriptor{}, err
	}
	return r.GetDescriptorByIDContext(ctx, siteID)
}

// SetRuntimeCellID changes placement as a revisioned directory event. Blank is
// the backwards-compatible default assignment; readers treat it as "default"
// only when a process explicitly opts into cell mode.
func (r *SQLSiteRegistry) SetRuntimeCellID(siteID, cellID string) (SiteDescriptor, error) {
	if r == nil || strings.TrimSpace(siteID) == "" {
		return SiteDescriptor{}, errors.New("site registry and site id are required")
	}
	current, err := r.GetDescriptorByID(siteID)
	if err != nil {
		return SiteDescriptor{}, err
	}
	return r.SetRuntimeCellIDAtRevisionContext(context.Background(), siteID, cellID, current.ConfigRevision)
}

// SetRuntimeCellIDAtRevisionContext atomically changes the sole persisted
// placement assignment only if the directory entry still has the revision
// observed by the caller. This prevents a stale deployment/cell move from
// overwriting a concurrent assignment or lifecycle update.
func (r *SQLSiteRegistry) SetRuntimeCellIDAtRevisionContext(ctx context.Context, siteID, cellID string, expectedRevision uint64) (SiteDescriptor, error) {
	if r == nil || r.database == nil || strings.TrimSpace(siteID) == "" {
		return SiteDescriptor{}, errors.New("site registry and site id are required")
	}
	if ctx == nil {
		return SiteDescriptor{}, errors.New("setting site placement requires a context")
	}
	cellID, err := normalizeRuntimeCellID(cellID)
	if err != nil {
		return SiteDescriptor{}, err
	}
	if expectedRevision == ^uint64(0) {
		return SiteDescriptor{}, errors.New("site config revision cannot be incremented beyond uint64")
	}
	tx, err := r.database.BeginTx(ctx, nil)
	if err != nil {
		return SiteDescriptor{}, fmt.Errorf("begin revision-fenced site placement update: %w", err)
	}
	defer tx.Rollback()
	update := `UPDATE _kora_site_registry SET runtime_cell_id = ?, config_revision = config_revision + 1, updated_at = CURRENT_TIMESTAMP WHERE site_id = ? AND config_revision = ?`
	selectQuery := `SELECT site_id, COALESCE(runtime_cell_id, ''), site, COALESCE(domains_json, '[]'), status, config_revision FROM _kora_site_registry WHERE site_id = ?`
	if strings.EqualFold(r.dialect, "postgres") {
		update = `UPDATE _kora_site_registry SET runtime_cell_id = $1, config_revision = config_revision + 1, updated_at = CURRENT_TIMESTAMP WHERE site_id = $2 AND config_revision = $3`
		selectQuery = `SELECT site_id, COALESCE(runtime_cell_id, ''), site, COALESCE(domains_json, '[]'), status, config_revision FROM _kora_site_registry WHERE site_id = $1`
	}
	result, err := tx.ExecContext(ctx, update, cellID, siteID, expectedRevision)
	if err != nil {
		return SiteDescriptor{}, fmt.Errorf("update revision-fenced site placement: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return SiteDescriptor{}, fmt.Errorf("read revision-fenced site placement result: %w", err)
	}
	if count == 0 {
		var currentID, currentCellID, currentName, currentDomains, currentStatus string
		var currentRevision uint64
		if err := tx.QueryRowContext(ctx, selectQuery, siteID).Scan(&currentID, &currentCellID, &currentName, &currentDomains, &currentStatus, &currentRevision); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return SiteDescriptor{}, sql.ErrNoRows
			}
			return SiteDescriptor{}, fmt.Errorf("check revision-fenced site placement: %w", err)
		}
		return SiteDescriptor{}, ErrSiteRevisionConflict
	}
	var info DBSiteInfo
	var domainsJSON string
	if err := tx.QueryRowContext(ctx, selectQuery, siteID).Scan(&info.SiteID, &info.RuntimeCellID, &info.Name, &domainsJSON, &info.Status, &info.ConfigRevision); err != nil {
		return SiteDescriptor{}, fmt.Errorf("read assigned site descriptor: %w", err)
	}
	if err := json.Unmarshal([]byte(domainsJSON), &info.Domains); err != nil {
		return SiteDescriptor{}, fmt.Errorf("decode assigned site aliases: %w", err)
	}
	if err := appendDirectoryChange(tx, r.dialect, info); err != nil {
		return SiteDescriptor{}, err
	}
	if err := tx.Commit(); err != nil {
		return SiteDescriptor{}, fmt.Errorf("commit revision-fenced site placement update: %w", err)
	}
	return DescriptorFromDBSiteInfo(info), nil
}

func normalizeRuntimeCellID(cellID string) (string, error) {
	cellID = strings.ToLower(strings.TrimSpace(cellID))
	if len(cellID) > 80 {
		return "", errors.New("runtime cell id must be at most 80 characters")
	}
	for _, r := range cellID {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return "", errors.New("runtime cell id may contain only letters, numbers, dot, dash, and underscore")
		}
	}
	return cellID, nil
}

// NormalizeRuntimeCellID validates and canonicalizes an Engine cell setting.
func NormalizeRuntimeCellID(cellID string) (string, error) {
	return normalizeRuntimeCellID(cellID)
}

func (r *SQLSiteRegistry) Upsert(config *SiteConfig) error {
	if r == nil || r.database == nil {
		return errors.New("site registry unavailable")
	}
	return ensurePlatformSiteRegistration(r.database, r.dialect, config)
}

func (r *SQLSiteRegistry) SetStatus(siteID, status string) error {
	_, err := r.SetStatusContext(context.Background(), siteID, status)
	return err
}

// SetStatusAndGetDescriptor commits a lifecycle revision and returns the exact
// credential-free routing view from the same transaction. Local routers can
// apply this result without guessing a revision or racing a concurrent update.
func (r *SQLSiteRegistry) SetStatusAndGetDescriptor(siteID, status string) (SiteDescriptor, error) {
	return r.SetStatusContext(context.Background(), siteID, status)
}

func (r *SQLSiteRegistry) SetStatusContext(ctx context.Context, siteID, status string) (SiteDescriptor, error) {
	if r == nil || r.database == nil {
		return SiteDescriptor{}, errors.New("site registry unavailable")
	}
	if ctx == nil {
		return SiteDescriptor{}, errors.New("site status update requires context")
	}
	if err := ctx.Err(); err != nil {
		return SiteDescriptor{}, err
	}
	status = strings.ToLower(strings.TrimSpace(status))
	switch status {
	case "provisioning", "active", "degraded", "suspended", "deleting", "deleted":
	default:
		return SiteDescriptor{}, fmt.Errorf("unsupported site status %q", status)
	}
	tx, err := r.database.BeginTx(ctx, nil)
	if err != nil {
		return SiteDescriptor{}, fmt.Errorf("begin site status update: %w", err)
	}
	defer tx.Rollback()
	query := `UPDATE _kora_site_registry SET status = ?, config_revision = config_revision + 1, updated_at = CURRENT_TIMESTAMP WHERE site_id = ?`
	if strings.EqualFold(r.dialect, "postgres") {
		query = `UPDATE _kora_site_registry SET status = $1, config_revision = config_revision + 1, updated_at = CURRENT_TIMESTAMP WHERE site_id = $2`
	}
	result, err := tx.Exec(query, status, siteID)
	if err != nil {
		return SiteDescriptor{}, fmt.Errorf("update site status: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return SiteDescriptor{}, fmt.Errorf("read status update result: %w", err)
	}
	if count == 0 {
		return SiteDescriptor{}, sql.ErrNoRows
	}
	selectQuery := `SELECT site, COALESCE(runtime_cell_id, ''), COALESCE(domains_json, '[]'), status, config_revision FROM _kora_site_registry WHERE site_id = ?`
	if strings.EqualFold(r.dialect, "postgres") {
		selectQuery = `SELECT site, COALESCE(runtime_cell_id, ''), COALESCE(domains_json, '[]'), status, config_revision FROM _kora_site_registry WHERE site_id = $1`
	}
	var info DBSiteInfo
	var domainsJSON []byte
	if err := tx.QueryRow(selectQuery, siteID).Scan(&info.Name, &info.RuntimeCellID, &domainsJSON, &info.Status, &info.ConfigRevision); err != nil {
		return SiteDescriptor{}, fmt.Errorf("read updated site status: %w", err)
	}
	info.SiteID = siteID
	if err := json.Unmarshal(domainsJSON, &info.Domains); err != nil {
		return SiteDescriptor{}, fmt.Errorf("decode site aliases for directory change: %w", err)
	}
	if err := appendDirectoryChange(tx, r.dialect, info); err != nil {
		return SiteDescriptor{}, err
	}
	if err := tx.Commit(); err != nil {
		return SiteDescriptor{}, fmt.Errorf("commit site status update: %w", err)
	}
	return DescriptorFromDBSiteInfo(info), nil
}

// SetStatusAtRevision applies an operator-requested active/suspended state
// against the config revision Cloud inspected. Replaying the same target state
// is idempotent, including after the directory update committed but before the
// management operation journal was finished.
func (r *SQLSiteRegistry) SetStatusAtRevision(ctx context.Context, siteID, status string, expectedRevision uint64) (SiteDescriptor, error) {
	if r == nil || r.database == nil || strings.TrimSpace(siteID) == "" {
		return SiteDescriptor{}, errors.New("site registry and site id are required")
	}
	if ctx == nil {
		return SiteDescriptor{}, errors.New("setting site status requires context")
	}
	status = strings.ToLower(strings.TrimSpace(status))
	if status != "active" && status != "suspended" {
		return SiteDescriptor{}, fmt.Errorf("management can request only active or suspended site status")
	}
	tx, err := r.database.BeginTx(ctx, nil)
	if err != nil {
		return SiteDescriptor{}, fmt.Errorf("begin revision-fenced site status update: %w", err)
	}
	defer tx.Rollback()
	info, err := loadSiteStatusTx(ctx, tx, r.dialect, siteID)
	if err != nil {
		return SiteDescriptor{}, err
	}
	if info.Status == status {
		if err := tx.Commit(); err != nil {
			return SiteDescriptor{}, fmt.Errorf("commit idempotent site status replay: %w", err)
		}
		return DescriptorFromDBSiteInfo(info), nil
	}
	if info.ConfigRevision != expectedRevision {
		return SiteDescriptor{}, ErrSiteRevisionConflict
	}
	if info.ConfigRevision == ^uint64(0) {
		return SiteDescriptor{}, errors.New("site config revision cannot be incremented beyond uint64")
	}
	query := `UPDATE _kora_site_registry SET status = ?, config_revision = config_revision + 1, updated_at = CURRENT_TIMESTAMP WHERE site_id = ? AND config_revision = ?`
	if strings.EqualFold(r.dialect, "postgres") {
		query = `UPDATE _kora_site_registry SET status = $1, config_revision = config_revision + 1, updated_at = CURRENT_TIMESTAMP WHERE site_id = $2 AND config_revision = $3`
	}
	result, err := tx.ExecContext(ctx, query, status, siteID, expectedRevision)
	if err != nil {
		return SiteDescriptor{}, fmt.Errorf("update revision-fenced site status: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return SiteDescriptor{}, fmt.Errorf("read revision-fenced site status result: %w", err)
	}
	if changed != 1 {
		current, readErr := loadSiteStatusTx(ctx, tx, r.dialect, siteID)
		if readErr != nil {
			return SiteDescriptor{}, readErr
		}
		if current.Status == status {
			if err := tx.Commit(); err != nil {
				return SiteDescriptor{}, fmt.Errorf("commit idempotent site status replay: %w", err)
			}
			return DescriptorFromDBSiteInfo(current), nil
		}
		return SiteDescriptor{}, ErrSiteRevisionConflict
	}
	info.Status = status
	info.ConfigRevision++
	if err := appendDirectoryChange(tx, r.dialect, info); err != nil {
		return SiteDescriptor{}, fmt.Errorf("append site status directory change: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return SiteDescriptor{}, fmt.Errorf("commit revision-fenced site status update: %w", err)
	}
	return DescriptorFromDBSiteInfo(info), nil
}

var ErrSiteRevisionConflict = errors.New("site config revision changed before lifecycle update")
var ErrSiteAllocationConflict = errors.New("site data allocation changed before lifecycle update")

// SetDeletingAtAllocation verifies the directory allocation and fences the
// tenant into deleting in the same transaction. Provisioning locks the same
// directory row and refuses to replace a deleting tenant, so an allocation
// cannot be swapped between checkpoint verification and teardown.
func (r *SQLSiteRegistry) SetDeletingAtAllocation(ctx context.Context, siteID string, expectedRevision uint64, expectedDataPlaneRef string) (SiteDescriptor, error) {
	if r == nil || r.database == nil || strings.TrimSpace(siteID) == "" || strings.TrimSpace(expectedDataPlaneRef) == "" {
		return SiteDescriptor{}, errors.New("site registry, tenant ID, and expected data allocation are required")
	}
	if ctx == nil {
		return SiteDescriptor{}, errors.New("setting tenant deleting requires context")
	}
	tx, err := r.database.BeginTx(ctx, nil)
	if err != nil {
		return SiteDescriptor{}, fmt.Errorf("begin allocation-fenced tenant deletion: %w", err)
	}
	defer tx.Rollback()
	info, err := loadSiteStatusTx(ctx, tx, r.dialect, siteID)
	if err != nil {
		return SiteDescriptor{}, err
	}
	if info.SiteID != siteID || DataPlaneReference(info) != expectedDataPlaneRef {
		return SiteDescriptor{}, ErrSiteAllocationConflict
	}
	if info.Status == "deleting" {
		if err := tx.Commit(); err != nil {
			return SiteDescriptor{}, fmt.Errorf("commit idempotent allocation-fenced deletion: %w", err)
		}
		return DescriptorFromDBSiteInfo(info), nil
	}
	if info.Status != "active" && info.Status != "suspended" {
		return SiteDescriptor{}, fmt.Errorf("cannot delete tenant in directory state %q", info.Status)
	}
	if info.ConfigRevision != expectedRevision {
		return SiteDescriptor{}, ErrSiteRevisionConflict
	}
	if info.ConfigRevision == ^uint64(0) {
		return SiteDescriptor{}, errors.New("site config revision cannot be incremented beyond uint64")
	}
	query := `UPDATE _kora_site_registry SET status = 'deleting', config_revision = config_revision + 1, updated_at = CURRENT_TIMESTAMP WHERE site_id = ? AND config_revision = ?`
	args := []any{siteID, expectedRevision}
	if strings.EqualFold(r.dialect, "postgres") {
		query = `UPDATE _kora_site_registry SET status = 'deleting', config_revision = config_revision + 1, updated_at = CURRENT_TIMESTAMP WHERE site_id = $1 AND config_revision = $2`
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return SiteDescriptor{}, fmt.Errorf("fence tenant allocation for deletion: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return SiteDescriptor{}, fmt.Errorf("read allocation-fenced deletion result: %w", err)
	}
	if changed != 1 {
		return SiteDescriptor{}, ErrSiteRevisionConflict
	}
	info.Status = "deleting"
	info.ConfigRevision++
	if err := appendDirectoryChange(tx, r.dialect, info); err != nil {
		return SiteDescriptor{}, fmt.Errorf("append tenant deletion directory change: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return SiteDescriptor{}, fmt.Errorf("commit allocation-fenced tenant deletion: %w", err)
	}
	return DescriptorFromDBSiteInfo(info), nil
}

func loadSiteStatusTx(ctx context.Context, tx *sql.Tx, dialect, siteID string) (DBSiteInfo, error) {
	query := `SELECT site_id, COALESCE(runtime_cell_id, ''), site, db_type, db_host, db_port, db_name, COALESCE(domains_json, '[]'), COALESCE(file_storage, 'local'), COALESCE(storage_bucket, ''), status, config_revision FROM _kora_site_registry WHERE site_id = ?`
	if strings.EqualFold(dialect, "postgres") {
		query = `SELECT site_id, COALESCE(runtime_cell_id, ''), site, db_type, db_host, db_port, db_name, COALESCE(domains_json, '[]'), COALESCE(file_storage, 'local'), COALESCE(storage_bucket, ''), status, config_revision FROM _kora_site_registry WHERE site_id = $1`
	}
	if strings.EqualFold(dialect, "mysql") || strings.EqualFold(dialect, "postgres") {
		query += ` FOR UPDATE`
	}
	var info DBSiteInfo
	var domainsJSON string
	if err := tx.QueryRowContext(ctx, query, siteID).Scan(&info.SiteID, &info.RuntimeCellID, &info.Name, &info.DBType, &info.DBHost, &info.DBPort, &info.DBName, &domainsJSON, &info.FileStorage, &info.StorageBucket, &info.Status, &info.ConfigRevision); err != nil {
		return DBSiteInfo{}, err
	}
	if err := json.Unmarshal([]byte(domainsJSON), &info.Domains); err != nil {
		return DBSiteInfo{}, fmt.Errorf("decode site aliases for %s: %w", siteID, err)
	}
	return info, nil
}

var _ SiteRegistry = (*SQLSiteRegistry)(nil)
