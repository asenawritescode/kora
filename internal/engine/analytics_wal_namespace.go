package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/asenawritescode/kora/internal/analytics"
	"github.com/asenawritescode/kora/internal/site"
)

const analyticsWALMigrationPageSize uint16 = 500

type analyticsWALNamespaceOwner struct {
	tenantID  string
	siteNames []string
}

func migrateAnalyticsWALNamespaces(
	ctx context.Context,
	baseDir string,
	directory TenantRuntimeDirectory,
	cellID string,
	workAdmission interface {
		Acquire(context.Context) (func(), error)
	},
) error {
	if strings.TrimSpace(baseDir) == "" {
		return nil
	}
	if ctx == nil || directory == nil {
		return errors.New("analytics WAL migration requires context and tenant directory")
	}
	var owners []analyticsWALNamespaceOwner
	cursor := ""
	for {
		var page []site.SiteDescriptor
		err := withAnalyticsWALMigrationAdmission(ctx, workAdmission, func() error {
			var err error
			page, err = directory.GetDirectoryDescriptorPageContext(ctx, cellID, cursor, analyticsWALMigrationPageSize)
			return err
		})
		if err != nil {
			return fmt.Errorf("list tenant directory for analytics WAL migration: %w", err)
		}
		if len(page) > int(analyticsWALMigrationPageSize) {
			return fmt.Errorf("tenant directory returned %d descriptors for analytics WAL migration page limit %d", len(page), analyticsWALMigrationPageSize)
		}
		for _, descriptor := range page {
			if strings.TrimSpace(descriptor.SiteID) == "" {
				return errors.New("tenant directory returned an empty tenant ID during analytics WAL migration")
			}
			var info site.DBSiteInfo
			err := withAnalyticsWALMigrationAdmission(ctx, workAdmission, func() error {
				var err error
				info, err = directory.GetByIDContext(ctx, descriptor.SiteID)
				return err
			})
			if err != nil {
				return fmt.Errorf("load tenant %q for analytics WAL migration: %w", descriptor.SiteID, err)
			}
			if info.SiteID != descriptor.SiteID {
				return fmt.Errorf("tenant directory identity mismatch during analytics WAL migration: descriptor %q, record %q", descriptor.SiteID, info.SiteID)
			}
			if strings.TrimSpace(info.Name) == "" {
				return fmt.Errorf("tenant %q has no canonical site name for legacy analytics WAL migration", descriptor.SiteID)
			}
			siteNames := append([]string{info.Name}, descriptor.Aliases...)
			owners = append(owners, analyticsWALNamespaceOwner{tenantID: info.SiteID, siteNames: siteNames})
		}
		if len(page) < int(analyticsWALMigrationPageSize) {
			break
		}
		next := page[len(page)-1].SiteID
		if next == "" || next <= cursor {
			return fmt.Errorf("tenant directory cursor did not advance during analytics WAL migration: %q -> %q", cursor, next)
		}
		cursor = next
	}
	return migrateAnalyticsWALDirectories(baseDir, owners)
}

func withAnalyticsWALMigrationAdmission(
	ctx context.Context,
	admission interface {
		Acquire(context.Context) (func(), error)
	},
	work func() error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if admission == nil {
		return work()
	}
	release, err := admission.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return err
	}
	return work()
}

func migrateAnalyticsWALDirectories(baseDir string, owners []analyticsWALNamespaceOwner) error {
	baseDir = filepath.Clean(strings.TrimSpace(baseDir))
	_, err := os.ReadDir(baseDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect analytics WAL root %q: %w", baseDir, err)
	}
	if pending, err := analytics.WALHasPending(baseDir); err != nil {
		return fmt.Errorf("inspect unpartitioned analytics WAL in %q: %w", baseDir, err)
	} else if pending {
		return fmt.Errorf("unpartitioned analytics WAL at %q has pending events and no stable tenant owner; preserve it and reconcile before starting Engine", baseDir)
	}

	legacyOwners := make(map[string]string, len(owners))
	stableDirs := make(map[string]struct{}, len(owners))
	for _, owner := range owners {
		if strings.TrimSpace(owner.tenantID) == "" || len(owner.siteNames) == 0 {
			return errors.New("analytics WAL migration owner requires tenant ID and site name")
		}
		stable := filepath.Clean(AnalyticsTenantWALDir(baseDir, owner.tenantID))
		for _, siteName := range owner.siteNames {
			if strings.TrimSpace(siteName) == "" {
				continue
			}
			legacy := filepath.Clean(AnalyticsSiteWALDir(baseDir, siteName))
			if legacy == stable {
				return fmt.Errorf("legacy analytics WAL path unexpectedly equals stable tenant path for %q", owner.tenantID)
			}
			if previous, exists := legacyOwners[legacy]; exists && previous != owner.tenantID {
				return fmt.Errorf("legacy analytics WAL path %q maps to multiple tenants %q and %q", legacy, previous, owner.tenantID)
			}
			legacyOwners[legacy] = owner.tenantID
		}
		stableDirs[stable] = struct{}{}
	}

	for legacy, tenantID := range legacyOwners {
		if _, err := os.Stat(legacy); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return fmt.Errorf("inspect legacy analytics WAL path %q: %w", legacy, err)
		}
		pending, err := analytics.WALHasPending(legacy)
		if err != nil {
			return fmt.Errorf("inspect legacy analytics WAL for tenant %q at %q: %w", tenantID, legacy, err)
		}
		if !pending {
			continue
		}
		stable := AnalyticsTenantWALDir(baseDir, tenantID)
		if _, err := os.Stat(stable); err == nil {
			stablePending, inspectErr := analytics.WALHasPending(stable)
			if inspectErr != nil {
				return fmt.Errorf("inspect stable analytics WAL for tenant %q at %q: %w", tenantID, stable, inspectErr)
			}
			if stablePending {
				return fmt.Errorf("both legacy and stable analytics WALs contain pending events for tenant %q (%q and %q); refusing to merge unordered logs", tenantID, legacy, stable)
			}
			if err := removeEmptyAnalyticsWALDirectory(stable); err != nil {
				return fmt.Errorf("prepare stable analytics WAL path for tenant %q: %w", tenantID, err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect stable analytics WAL path %q: %w", stable, err)
		}
		if err := os.Rename(legacy, stable); err != nil {
			return fmt.Errorf("move legacy analytics WAL for tenant %q from %q to %q: %w", tenantID, legacy, stable, err)
		}
		if err := syncAnalyticsWALDirectory(baseDir); err != nil {
			return fmt.Errorf("sync analytics WAL root after migrating tenant %q: %w", tenantID, err)
		}
	}

	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return fmt.Errorf("verify analytics WAL namespace migration in %q: %w", baseDir, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(baseDir, entry.Name())
		if _, ok := stableDirs[path]; ok {
			continue
		}
		if _, ok := legacyOwners[path]; ok {
			continue // empty or already-acknowledged legacy directory
		}
		pending, err := analytics.WALHasPending(path)
		if err != nil {
			return fmt.Errorf("inspect unassigned analytics WAL directory %q: %w", path, err)
		}
		if pending {
			return fmt.Errorf("unassigned analytics WAL directory %q has pending events; map its legacy site name to a stable tenant before starting Engine", path)
		}
	}
	return nil
}

func removeEmptyAnalyticsWALDirectory(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return fmt.Errorf("stable path contains unexpected directory %q", entry.Name())
		}
		name := entry.Name()
		if name != "events.jsonl" && !strings.HasSuffix(name, ".flushing") && !strings.HasPrefix(name, ".wal-ack-") {
			return fmt.Errorf("stable path contains unrecognized file %q", name)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() != 0 {
			return fmt.Errorf("stable path file %q is nonempty", name)
		}
		if err := os.Remove(filepath.Join(path, name)); err != nil {
			return err
		}
	}
	return os.Remove(path)
}

func syncAnalyticsWALDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}
