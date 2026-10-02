package site

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"

	sqlDialect "github.com/asenawritescode/kora/db"
)

var pendingDeletionLocks sync.Map

// RetryPendingSiteDeletions resumes only sites whose durable lifecycle state is
// already "deleting". SQL advisory locks serialize cleanup across Engine
// processes sharing a MySQL or PostgreSQL platform registry.
func RetryPendingSiteDeletions(ctx context.Context, platformDB *sql.DB, platformDBType string, common *CommonConfig) error {
	if platformDB == nil {
		return errors.New("platform site registry unavailable")
	}
	registry := NewSQLSiteRegistry(platformDB, platformDBType)
	descriptors, err := registry.GetDirectoryDescriptors()
	if err != nil {
		return fmt.Errorf("list site deletion intents: %w", err)
	}
	var failures []error
	for _, descriptor := range descriptors {
		if descriptor.Status != "deleting" || descriptor.SiteID == "" {
			continue
		}
		lockValue, _ := pendingDeletionLocks.LoadOrStore(descriptor.SiteID, &sync.Mutex{})
		localLock := lockValue.(*sync.Mutex)
		localLock.Lock()
		locked, release, err := acquireSiteDeletionLock(ctx, platformDB, platformDBType, descriptor.SiteID)
		if err != nil {
			localLock.Unlock()
			failures = append(failures, fmt.Errorf("site %s: acquire cleanup lock: %w", descriptor.SiteID, err))
			continue
		}
		if !locked {
			localLock.Unlock()
			continue
		}
		if err := retryOneSiteDeletion(registry, descriptor.SiteID, platformDB, platformDBType, common); err != nil {
			failures = append(failures, fmt.Errorf("site %s: %w", descriptor.SiteID, err))
		}
		release()
		localLock.Unlock()
	}
	return errors.Join(failures...)
}

// DeleteRegisteredSite durably fences a canonical site ID before deleting its
// tenant storage. Repeated calls are safe and never derive a database name
// from a hostname.
func DeleteRegisteredSite(ctx context.Context, platformDB *sql.DB, platformDBType, siteID, expectedHostname string, common *CommonConfig) error {
	if platformDB == nil || strings.TrimSpace(siteID) == "" || strings.TrimSpace(expectedHostname) == "" {
		return errors.New("platform registry, canonical site id, and expected hostname are required")
	}
	registry := NewSQLSiteRegistry(platformDB, platformDBType)
	info, err := registry.GetByID(siteID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // already deleted; this makes a Cloud retry idempotent
		}
		return fmt.Errorf("read canonical site before deletion: %w", err)
	}
	matchedHostname := strings.EqualFold(NormalizeSiteAlias(info.Name), NormalizeSiteAlias(expectedHostname))
	for _, alias := range info.Domains {
		matchedHostname = matchedHostname || strings.EqualFold(NormalizeSiteAlias(alias), NormalizeSiteAlias(expectedHostname))
	}
	if !matchedHostname {
		return fmt.Errorf("hostname %q is not an alias of canonical site %s", expectedHostname, siteID)
	}
	if info.Status == "deleted" {
		return fmt.Errorf("site %s remains in the registry with deleted status; manual reconciliation is required", siteID)
	}
	if info.Status != "deleting" {
		if _, err := registry.SetStatusAndGetDescriptor(siteID, "deleting"); err != nil {
			return fmt.Errorf("persist site deletion intent: %w", err)
		}
	}
	lockValue, _ := pendingDeletionLocks.LoadOrStore(siteID, &sync.Mutex{})
	localLock := lockValue.(*sync.Mutex)
	localLock.Lock()
	defer localLock.Unlock()
	locked, release, err := acquireSiteDeletionLock(ctx, platformDB, platformDBType, siteID)
	if err != nil {
		return fmt.Errorf("acquire site deletion lock: %w", err)
	}
	if !locked {
		return fmt.Errorf("site deletion is already running for %s; retry shortly", siteID)
	}
	defer release()
	return retryOneSiteDeletion(registry, siteID, platformDB, platformDBType, common)
}

func retryOneSiteDeletion(registry *SQLSiteRegistry, siteID string, platformDB *sql.DB, platformDBType string, common *CommonConfig) error {
	info, err := registry.GetByID(siteID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("read deletion target: %w", err)
	}
	if info.Status != "deleting" {
		return nil
	}
	cfg := ReconstructSiteConfigFromDBInfo(info, common)
	dbName := strings.TrimSpace(info.DBName)
	if dbName == "" {
		return fmt.Errorf("refusing to infer tenant database name for site %s", siteID)
	}
	var tenantDB *sql.DB
	if strings.EqualFold(cfg.DBType, "libsql") {
		tenantDB, err = Connect(cfg)
		if err != nil {
			return fmt.Errorf("connect to tenant for cleanup: %w", err)
		}
	}
	return DeleteSite(DeleteSiteInput{
		DB: tenantDB, Dialect: sqlDialect.Resolve(cfg.DBType), Hostname: info.Name, SiteID: info.SiteID,
		PlatformDB: platformDB, PlatformDBType: platformDBType,
		DBType: cfg.DBType, DBName: dbName, DBHost: cfg.DBHost, DBPort: cfg.DBPort,
		DBUser: cfg.DBUser, DBPassword: cfg.DBPassword, DBDSN: cfg.DSN(),
	})
}

func acquireSiteDeletionLock(ctx context.Context, platformDB *sql.DB, platformDBType, siteID string) (bool, func(), error) {
	conn, err := platformDB.Conn(ctx)
	if err != nil {
		return false, nil, err
	}
	key := "kora:site-delete:" + siteID
	var acquired bool
	switch strings.ToLower(platformDBType) {
	case "postgres", "postgresql":
		err = conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, key).Scan(&acquired)
	case "mysql", "mariadb":
		var result sql.NullInt64
		err = conn.QueryRowContext(ctx, `SELECT GET_LOCK(?, 0)`, key).Scan(&result)
		acquired = result.Valid && result.Int64 == 1
	default:
		conn.Close()
		return false, nil, fmt.Errorf("cross-process deletion locks are unsupported for platform database %q", platformDBType)
	}
	if err != nil {
		conn.Close()
		return false, nil, err
	}
	if !acquired {
		conn.Close()
		return false, nil, nil
	}
	release := func() {
		defer conn.Close()
		var ignored any
		if strings.EqualFold(platformDBType, "postgres") || strings.EqualFold(platformDBType, "postgresql") {
			_ = conn.QueryRowContext(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, key).Scan(&ignored)
		} else {
			_ = conn.QueryRowContext(context.Background(), `SELECT RELEASE_LOCK(?)`, key).Scan(&ignored)
		}
	}
	return true, release, nil
}
