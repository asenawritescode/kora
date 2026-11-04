package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/asenawritescode/kora/db"
)

var ErrDurableScanCursorMoved = errors.New("durable work scan cursor moved concurrently")

type durableScanStateStore struct {
	database *sql.DB
	dialect  db.Dialect
}

func newDurableScanStateStore(database *sql.DB, dialect db.Dialect) (*durableScanStateStore, error) {
	if database == nil || dialect == nil {
		return nil, errors.New("durable scan state requires platform database and dialect")
	}
	return &durableScanStateStore{database: database, dialect: dialect}, nil
}

func (s *durableScanStateStore) Ensure(ctx context.Context) error {
	if ctx == nil {
		return errors.New("durable scan schema requires context")
	}
	statement := `CREATE TABLE IF NOT EXISTS _kora_durable_scan_cursor (scan_key VARCHAR(255) PRIMARY KEY, after_site_id VARCHAR(255) NOT NULL DEFAULT '', last_completed_at TIMESTAMP NULL, updated_at TIMESTAMP NOT NULL)`
	switch s.dialect.DriverName() {
	case "mysql":
		statement = `CREATE TABLE IF NOT EXISTS _kora_durable_scan_cursor (scan_key VARCHAR(255) PRIMARY KEY, after_site_id VARCHAR(255) NOT NULL DEFAULT '', last_completed_at DATETIME(6) NULL, updated_at DATETIME(6) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`
	case "libsql":
		statement = `CREATE TABLE IF NOT EXISTS _kora_durable_scan_cursor (scan_key TEXT PRIMARY KEY, after_site_id TEXT NOT NULL DEFAULT '', last_completed_at TEXT, updated_at TEXT NOT NULL)`
	case "postgres":
		statement = `CREATE TABLE IF NOT EXISTS _kora_durable_scan_cursor (scan_key VARCHAR(255) PRIMARY KEY, after_site_id VARCHAR(255) NOT NULL DEFAULT '', last_completed_at TIMESTAMP NULL, updated_at TIMESTAMP NOT NULL)`
	default:
		return fmt.Errorf("unsupported durable scan platform dialect %q", s.dialect.DriverName())
	}
	if _, err := s.database.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("ensure durable scan cursor schema: %w", err)
	}
	return nil
}

func (s *durableScanStateStore) Load(ctx context.Context, key string) (string, time.Time, error) {
	if key == "" {
		return "", time.Time{}, errors.New("durable scan key is required")
	}
	var cursor string
	var completedRaw any
	load := func() error {
		return s.database.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT after_site_id,last_completed_at FROM _kora_durable_scan_cursor WHERE scan_key=?`), key).Scan(&cursor, &completedRaw)
	}
	err := load()
	if errors.Is(err, sql.ErrNoRows) {
		insert := `INSERT INTO _kora_durable_scan_cursor (scan_key,after_site_id,updated_at) VALUES (?,'',` + s.dialect.NowTimestamp() + `)`
		switch s.dialect.DriverName() {
		case "mysql":
			insert = `INSERT IGNORE INTO _kora_durable_scan_cursor (scan_key,after_site_id,updated_at) VALUES (?,'',` + s.dialect.NowTimestamp() + `)`
		case "postgres", "libsql":
			insert += ` ON CONFLICT (scan_key) DO NOTHING`
		}
		if _, err := s.database.ExecContext(ctx, db.Rebind(s.dialect, insert), key); err != nil {
			return "", time.Time{}, fmt.Errorf("initialize durable scan cursor: %w", err)
		}
		err = load()
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("load durable scan cursor: %w", err)
	}
	var completed time.Time
	if completedRaw != nil {
		var stamp db.Timestamp
		if err := stamp.Scan(completedRaw); err != nil {
			return "", time.Time{}, fmt.Errorf("parse durable scan completion time: %w", err)
		}
		completed = stamp.Time.UTC()
	}
	return cursor, completed, nil
}

func (s *durableScanStateStore) Advance(ctx context.Context, key, expected, next string, sweepComplete bool) error {
	updatedAt := s.dialect.NowTimestamp()
	query := `UPDATE _kora_durable_scan_cursor SET after_site_id=?,updated_at=` + updatedAt + ` WHERE scan_key=? AND after_site_id=?`
	if sweepComplete {
		query = `UPDATE _kora_durable_scan_cursor SET after_site_id=?,last_completed_at=` + updatedAt + `,updated_at=` + updatedAt + ` WHERE scan_key=? AND after_site_id=?`
	}
	result, err := s.database.ExecContext(ctx, db.Rebind(s.dialect, query), next, key, expected)
	if err != nil {
		return fmt.Errorf("advance durable scan cursor: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read durable scan cursor result: %w", err)
	}
	if changed != 1 {
		if changed == 0 && sweepComplete && expected == next {
			return nil
		}
		return ErrDurableScanCursorMoved
	}
	return nil
}
