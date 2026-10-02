package site

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/asenawritescode/kora/db"
)

// MySQL and PostgreSQL advisory locks coordinate concurrent Engine starts
// across processes. The in-process mutex also serializes LibSQL bootstrap in
// standalone/single-process deployments, where cross-process Engine cells are
// not supported by the current runtime contract.
var platformRegistryBootstrapMu sync.Mutex

func bootstrapPlatformRegistryWithLock(database *sql.DB, dialect db.Dialect) (err error) {
	if database == nil || dialect == nil {
		return errors.New("platform registry database and dialect are required")
	}
	platformRegistryBootstrapMu.Lock()
	defer platformRegistryBootstrapMu.Unlock()

	release, err := acquirePlatformRegistryBootstrapLock(database, dialect)
	if err != nil {
		return err
	}
	defer func() {
		if releaseErr := release(); err == nil && releaseErr != nil {
			err = fmt.Errorf("release platform registry bootstrap lock: %w", releaseErr)
		}
	}()
	return bootstrapPlatformRegistryUnlocked(database, dialect)
}

func acquirePlatformRegistryBootstrapLock(database *sql.DB, dialect db.Dialect) (func() error, error) {
	driver := dialect.DriverName()
	if driver != "mysql" && driver != "postgres" {
		return func() error { return nil }, nil
	}
	// A dedicated connection is required because advisory locks are session
	// scoped. Temporarily allow one extra connection for callers that configured
	// a one-connection platform pool; migration statements continue through DB.
	previousMax := database.Stats().MaxOpenConnections
	if previousMax == 1 {
		database.SetMaxOpenConns(2)
	}
	restorePoolLimit := func() {
		if previousMax == 1 {
			database.SetMaxOpenConns(previousMax)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	connection, err := database.Conn(ctx)
	if err != nil {
		restorePoolLimit()
		return nil, fmt.Errorf("reserve platform registry bootstrap connection: %w", err)
	}
	lockName, err := platformBootstrapLockName(ctx, connection, driver)
	if err != nil {
		_ = connection.Close()
		restorePoolLimit()
		return nil, err
	}
	if driver == "mysql" {
		var acquired sql.NullInt64
		if err := connection.QueryRowContext(ctx, `SELECT GET_LOCK(?, 55)`, lockName).Scan(&acquired); err != nil {
			_ = connection.Close()
			restorePoolLimit()
			return nil, fmt.Errorf("acquire MySQL platform registry bootstrap lock: %w", err)
		}
		if !acquired.Valid || acquired.Int64 != 1 {
			_ = connection.Close()
			restorePoolLimit()
			return nil, errors.New("timed out acquiring MySQL platform registry bootstrap lock")
		}
		release := func() error {
			var released sql.NullInt64
			err := connection.QueryRowContext(context.Background(), `SELECT RELEASE_LOCK(?)`, lockName).Scan(&released)
			closeErr := connection.Close()
			restorePoolLimit()
			if err != nil {
				return err
			}
			if !released.Valid || released.Int64 != 1 {
				return errors.New("MySQL did not release the platform registry bootstrap lock")
			}
			return closeErr
		}
		return release, nil
	}
	var ignored any
	var acquired int
	if err := connection.QueryRowContext(ctx, `SELECT pg_advisory_lock(hashtext($1)::bigint)::text, 1`, lockName).Scan(&ignored, &acquired); err != nil {
		_ = connection.Close()
		restorePoolLimit()
		return nil, fmt.Errorf("acquire PostgreSQL platform registry bootstrap lock: %w", err)
	}
	release := func() error {
		var ignored any
		var unlocked bool
		err := connection.QueryRowContext(context.Background(), `SELECT pg_advisory_unlock(hashtext($1)::bigint)::text, true`, lockName).Scan(&ignored, &unlocked)
		closeErr := connection.Close()
		restorePoolLimit()
		if err != nil {
			return err
		}
		if !unlocked {
			return errors.New("PostgreSQL did not release the platform registry bootstrap lock")
		}
		return closeErr
	}
	return release, nil
}

func platformBootstrapLockName(ctx context.Context, connection *sql.Conn, driver string) (string, error) {
	var databaseIdentity string
	query := `SELECT current_database() || ':' || current_schema()`
	if driver == "mysql" {
		query = `SELECT DATABASE()`
	}
	if err := connection.QueryRowContext(ctx, query).Scan(&databaseIdentity); err != nil {
		return "", fmt.Errorf("identify platform registry database for bootstrap lock: %w", err)
	}
	digest := sha256.Sum256([]byte(databaseIdentity))
	// MySQL lock names are limited to 64 bytes. This is also a stable opaque
	// advisory-lock key for PostgreSQL and never includes credentials.
	return "kora:platform-bootstrap:" + hex.EncodeToString(digest[:16]), nil
}
