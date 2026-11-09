package cli

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/site"
)

type cliWorkAdmission interface {
	Acquire(context.Context) (func(), error)
}

type cliMaintenanceAdmission interface {
	AcquireMaintenance(context.Context) (func(), error)
}

func acquireCLIWork(ctx context.Context, driver, dsn string, maintenance bool) (func(), error) {
	admission, err := dbaccess.AdmissionFor(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("resolve CLI database work admission: %w", err)
	}
	if maintenance {
		maintenanceAdmission, ok := any(admission).(cliMaintenanceAdmission)
		if !ok {
			return nil, fmt.Errorf("database endpoint does not provide maintenance admission")
		}
		release, err := maintenanceAdmission.AcquireMaintenance(ctx)
		if err != nil {
			return nil, fmt.Errorf("admit CLI database maintenance: %w", err)
		}
		return release, nil
	}
	release, err := admission.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("admit CLI database work: %w", err)
	}
	return release, nil
}

func acquireSiteCLIWork(ctx context.Context, config *site.Config, maintenance bool) (func(), error) {
	admission, err := site.ConnectionAdmission(config)
	if err != nil {
		return nil, fmt.Errorf("resolve tenant CLI database work admission: %w", err)
	}
	if maintenance {
		release, err := admission.AcquireMaintenance(ctx)
		if err != nil {
			return nil, fmt.Errorf("admit tenant CLI database maintenance: %w", err)
		}
		return release, nil
	}
	release, err := admission.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("admit tenant CLI database work: %w", err)
	}
	return release, nil
}

func acquireSiteCreationWork(ctx context.Context, tenantConfig *site.Config, platformDriver, platformDSN string) (func(), error) {
	if ctx == nil || tenantConfig == nil {
		return nil, fmt.Errorf("site creation admission requires context and tenant configuration")
	}
	tenantAdmission, err := site.ConnectionAdmission(tenantConfig)
	if err != nil {
		return nil, fmt.Errorf("resolve tenant site-creation admission: %w", err)
	}
	tenantDriver := strings.TrimSpace(tenantConfig.DBType)
	if tenantDriver == "" {
		tenantDriver = "mysql"
	}
	tenantDSN := tenantConfig.DSN()
	if envDSN := strings.TrimSpace(os.Getenv("DB_DSN")); envDSN != "" && (strings.EqualFold(tenantDriver, "libsql") || tenantConfig.DBUser == "") {
		tenantDSN = envDSN
	}
	entries := []struct {
		key       string
		admission cliMaintenanceAdmission
	}{{key: dbaccess.ServerKey(tenantDriver, tenantDSN), admission: tenantAdmission}}
	if strings.TrimSpace(platformDSN) != "" {
		if strings.TrimSpace(platformDriver) == "" {
			platformDriver = tenantDriver
		}
		platformAdmission, err := dbaccess.AdmissionFor(platformDriver, platformDSN)
		if err != nil {
			return nil, fmt.Errorf("resolve platform site-creation admission: %w", err)
		}
		platformKey := dbaccess.ServerKey(platformDriver, platformDSN)
		if platformKey != entries[0].key {
			entries = append(entries, struct {
				key       string
				admission cliMaintenanceAdmission
			}{key: platformKey, admission: platformAdmission})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].key < entries[j].key })
	releases := make([]func(), 0, len(entries))
	for _, entry := range entries {
		release, err := entry.admission.AcquireMaintenance(ctx)
		if err != nil {
			for i := len(releases) - 1; i >= 0; i-- {
				releases[i]()
			}
			return nil, fmt.Errorf("admit site creation maintenance: %w", err)
		}
		releases = append(releases, release)
	}
	return func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}, nil
}

func runCLIWork(ctx context.Context, admission cliWorkAdmission, work func() error) error {
	if ctx == nil || admission == nil || work == nil {
		return fmt.Errorf("CLI database work requires context, admission, and operation")
	}
	release, err := admission.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	return work()
}
