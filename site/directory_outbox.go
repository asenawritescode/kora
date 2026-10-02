package site

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/oklog/ulid/v2"
)

// SiteDirectoryChange is a credential-free, versioned directory projection.
// Cursor is DB-allocated for global ordering across Engine processes;
// consumers still compare per-site ConfigRevision because replay is expected.
type SiteDirectoryChange struct {
	Cursor     uint64                  `json:"cursor"`
	ID         string                  `json:"id"`
	Descriptor contract.SiteDescriptor `json:"descriptor"`
	CreatedAt  time.Time               `json:"created_at"`
}

var directoryChangeIDMu sync.Mutex
var directoryChangeEntropy = ulid.Monotonic(rand.Reader, 0)

func platformDirectoryOutboxDDL(dialect db.Dialect) []string {
	switch dialect.(type) {
	case *db.PostgresDialect:
		return []string{
			`CREATE TABLE IF NOT EXISTS "_kora_site_directory_meta" ("id" INTEGER PRIMARY KEY, "directory_revision" BIGINT NOT NULL DEFAULT 0)`,
			`INSERT INTO "_kora_site_directory_meta" ("id", "directory_revision") VALUES (1, 0) ON CONFLICT ("id") DO NOTHING`,
			`CREATE TABLE IF NOT EXISTS "_kora_site_directory_outbox" ("id" VARCHAR(26) PRIMARY KEY, "directory_revision" BIGINT NOT NULL DEFAULT 0, "site_id" VARCHAR(64) NOT NULL, "config_revision" BIGINT NOT NULL, "payload" JSONB NOT NULL, "created_at" TIMESTAMP NOT NULL DEFAULT NOW())`,
			`ALTER TABLE "_kora_site_directory_outbox" ADD COLUMN "directory_revision" BIGINT NOT NULL DEFAULT 0`,
			`CREATE INDEX IF NOT EXISTS "idx_site_directory_outbox_site_revision" ON "_kora_site_directory_outbox" ("site_id", "config_revision")`,
			`CREATE TABLE IF NOT EXISTS "_kora_site_directory_consumers" ("consumer_id" VARCHAR(180) PRIMARY KEY, "directory_revision" BIGINT NOT NULL DEFAULT 0, "updated_at" TIMESTAMP NOT NULL DEFAULT NOW(), "lease_owner" VARCHAR(180), "lease_expires_at" TIMESTAMP)`,
			`ALTER TABLE "_kora_site_directory_consumers" ADD COLUMN "lease_owner" VARCHAR(180)`,
			`ALTER TABLE "_kora_site_directory_consumers" ADD COLUMN "lease_expires_at" TIMESTAMP`,
		}
	case *db.LibSQLDialect:
		return []string{
			`CREATE TABLE IF NOT EXISTS "_kora_site_directory_outbox" ("id" TEXT PRIMARY KEY, "directory_revision" INTEGER NOT NULL DEFAULT 0, "site_id" TEXT NOT NULL, "config_revision" INTEGER NOT NULL, "payload" TEXT NOT NULL, "created_at" TEXT NOT NULL DEFAULT (datetime('now')))`,
			`CREATE UNIQUE INDEX IF NOT EXISTS "idx_site_directory_outbox_revision" ON "_kora_site_directory_outbox" ("directory_revision")`,
			`CREATE TABLE IF NOT EXISTS "_kora_site_directory_meta" ("id" INTEGER PRIMARY KEY, "directory_revision" INTEGER NOT NULL DEFAULT 0)`,
			`INSERT OR IGNORE INTO "_kora_site_directory_meta" ("id", "directory_revision") VALUES (1, 0)`,
			`ALTER TABLE "_kora_site_directory_outbox" ADD COLUMN "directory_revision" INTEGER NOT NULL DEFAULT 0`,
			`CREATE UNIQUE INDEX IF NOT EXISTS "idx_site_directory_outbox_revision" ON "_kora_site_directory_outbox" ("directory_revision")`,
			`CREATE INDEX IF NOT EXISTS "idx_site_directory_outbox_site_revision" ON "_kora_site_directory_outbox" ("site_id", "config_revision")`,
			`CREATE TABLE IF NOT EXISTS "_kora_site_directory_consumers" ("consumer_id" TEXT PRIMARY KEY, "directory_revision" INTEGER NOT NULL DEFAULT 0, "updated_at" TEXT NOT NULL DEFAULT (datetime('now')), "lease_owner" TEXT, "lease_expires_at" TEXT)`,
			`ALTER TABLE "_kora_site_directory_consumers" ADD COLUMN "lease_owner" TEXT`,
			`ALTER TABLE "_kora_site_directory_consumers" ADD COLUMN "lease_expires_at" TEXT`,
		}
	default:
		return []string{
			`CREATE TABLE IF NOT EXISTS _kora_site_directory_meta (id INTEGER PRIMARY KEY, directory_revision BIGINT NOT NULL DEFAULT 0) ENGINE=InnoDB`,
			`INSERT IGNORE INTO _kora_site_directory_meta (id, directory_revision) VALUES (1, 0)`,
			`CREATE TABLE IF NOT EXISTS _kora_site_directory_outbox (id VARCHAR(26) PRIMARY KEY, directory_revision BIGINT NOT NULL DEFAULT 0, site_id VARCHAR(64) NOT NULL, config_revision BIGINT NOT NULL, payload JSON NOT NULL, created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), UNIQUE INDEX idx_site_directory_outbox_revision (directory_revision), INDEX idx_site_directory_outbox_site_revision (site_id, config_revision)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
			`ALTER TABLE _kora_site_directory_outbox ADD COLUMN directory_revision BIGINT NOT NULL DEFAULT 0`,
			`CREATE UNIQUE INDEX idx_site_directory_outbox_revision ON _kora_site_directory_outbox (directory_revision)`,
			`CREATE TABLE IF NOT EXISTS _kora_site_directory_consumers (consumer_id VARCHAR(180) PRIMARY KEY, directory_revision BIGINT NOT NULL DEFAULT 0, updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), lease_owner VARCHAR(180) NULL, lease_expires_at DATETIME(6) NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
			`ALTER TABLE _kora_site_directory_consumers ADD COLUMN lease_owner VARCHAR(180) NULL`,
			`ALTER TABLE _kora_site_directory_consumers ADD COLUMN lease_expires_at DATETIME(6) NULL`,
		}
	}
}

func newDirectoryChangeID() (string, error) {
	directoryChangeIDMu.Lock()
	defer directoryChangeIDMu.Unlock()
	id, err := ulid.New(ulid.Timestamp(time.Now()), directoryChangeEntropy)
	if err != nil {
		return "", fmt.Errorf("generate site directory change id: %w", err)
	}
	return id.String(), nil
}

// ChangesAfter returns a bounded, replayable feed. The numeric cursor is exclusive;
// callers persist the last fully-applied cursor and periodically reconcile a full
// snapshot to recover from retention or consumer mistakes.
func (r *SQLSiteRegistry) ChangesAfter(cursor uint64, limit int) ([]SiteDirectoryChange, error) {
	if r == nil || r.database == nil {
		return nil, errors.New("site registry unavailable")
	}
	if limit <= 0 || limit > 500 {
		return nil, errors.New("site directory change limit must be between 1 and 500")
	}
	query := `SELECT directory_revision, id, payload, created_at FROM _kora_site_directory_outbox WHERE directory_revision > ? ORDER BY directory_revision LIMIT ?`
	if strings.EqualFold(r.dialect, "postgres") {
		query = `SELECT directory_revision, id, payload, created_at FROM _kora_site_directory_outbox WHERE directory_revision > $1 ORDER BY directory_revision LIMIT $2`
	}
	rows, err := r.database.Query(query, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	changes := make([]SiteDirectoryChange, 0)
	for rows.Next() {
		var change SiteDirectoryChange
		var payload []byte
		var createdAt any
		if err := rows.Scan(&change.Cursor, &change.ID, &payload, &createdAt); err != nil {
			return nil, err
		}
		switch value := createdAt.(type) {
		case time.Time:
			change.CreatedAt = value
		case []byte:
			change.CreatedAt, err = parseDirectoryChangeTime(string(value))
		case string:
			change.CreatedAt, err = parseDirectoryChangeTime(value)
		default:
			err = fmt.Errorf("unsupported created_at value %T", createdAt)
		}
		if err != nil {
			return nil, fmt.Errorf("decode directory change timestamp: %w", err)
		}
		if err := json.Unmarshal(payload, &change.Descriptor); err != nil {
			return nil, fmt.Errorf("decode directory change %s: %w", change.ID, err)
		}
		changes = append(changes, change)
	}
	return changes, rows.Err()
}

func parseDirectoryChangeTime(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999", "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized timestamp format")
}

func appendDirectoryChange(tx *sql.Tx, dialect string, info DBSiteInfo) error {
	id, err := newDirectoryChangeID()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE _kora_site_directory_meta SET directory_revision = directory_revision + 1 WHERE id = 1`); err != nil {
		return fmt.Errorf("advance directory revision: %w", err)
	}
	var cursor uint64
	if err := tx.QueryRow(`SELECT directory_revision FROM _kora_site_directory_meta WHERE id = 1`).Scan(&cursor); err != nil {
		return fmt.Errorf("read directory revision: %w", err)
	}
	descriptor := DescriptorFromDBSiteInfo(info)
	descriptor.DirectoryRevision = cursor
	payload, err := json.Marshal(descriptor)
	if err != nil {
		return fmt.Errorf("encode site directory change: %w", err)
	}
	query := `INSERT INTO _kora_site_directory_outbox (id, directory_revision, site_id, config_revision, payload) VALUES (?, ?, ?, ?, ?)`
	if strings.EqualFold(dialect, "postgres") {
		query = `INSERT INTO _kora_site_directory_outbox (id, directory_revision, site_id, config_revision, payload) VALUES ($1, $2, $3, $4, $5)`
	}
	if _, err := tx.Exec(query, id, cursor, info.SiteID, info.ConfigRevision, string(payload)); err != nil {
		return fmt.Errorf("append site directory change: %w", err)
	}
	return nil
}
