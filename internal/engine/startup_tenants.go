package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/asenawritescode/kora/internal/api"
	"github.com/asenawritescode/kora/internal/dbaccess"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/storage"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const defaultStartupTenantConcurrency = 8

// StartupTenantConfig carries the validated directory projection and only the
// process-specific storage resolver into Engine's cold tenant activation.
type StartupTenantConfig struct {
	Sites             []site.DBSiteInfo
	Common            *site.CommonConfig
	BinaryVersion     string
	Concurrency       uint16
	SharedPlatformDSN string
	ResolveStorage    func(*site.Config) (storage.Backend, error)
}

// StartupTenants is the coherent cold-start result transferred to the process
// owner after every discovered tenant has either activated or rolled back.
type StartupTenants struct {
	Runtimes  []*knet.LoadedSite
	Domains   []string
	PrimaryDB *sql.DB
}

type startupTenantDependencies struct {
	loadRuntime func(context.Context, site.DBSiteInfo, *site.CommonConfig, string) (*knet.LoadedSite, error)
	admission   func(*site.Config) (*dbaccess.Admission, error)
}

// LoadStartupTenants activates the startup directory snapshot with bounded
// parallelism. On any tenant failure it closes every successful sibling and
// joins rollback failures into the returned error.
func LoadStartupTenants(ctx context.Context, config StartupTenantConfig) (StartupTenants, error) {
	return loadStartupTenantSet(ctx, config, startupTenantDependencies{
		loadRuntime: func(ctx context.Context, info site.DBSiteInfo, common *site.CommonConfig, version string) (*knet.LoadedSite, error) {
			return LoadTenantRuntime(ctx, info, common, nil, version)
		},
		admission: site.ConnectionAdmission,
	})
}

func loadStartupTenantSet(ctx context.Context, config StartupTenantConfig, dependencies startupTenantDependencies) (StartupTenants, error) {
	if config.Common == nil {
		return StartupTenants{}, errors.New("Engine common configuration is required to load tenants")
	}
	if len(config.Sites) > 0 && config.ResolveStorage == nil {
		return StartupTenants{}, errors.New("Engine tenant storage resolver is required")
	}
	if len(config.Sites) > 0 && (dependencies.loadRuntime == nil || dependencies.admission == nil) {
		return StartupTenants{}, errors.New("Engine tenant activation dependencies are required")
	}
	if config.Concurrency == 0 {
		config.Concurrency = defaultStartupTenantConcurrency
	}
	results := loadStartupTenants(ctx, config, dependencies)
	runtimes, domains, primaryDB, err := collectLoadedStartupTenants(results)
	if err != nil {
		return StartupTenants{}, err
	}
	return StartupTenants{Runtimes: runtimes, Domains: domains, PrimaryDB: primaryDB}, nil
}

type startupTenantResult struct {
	name          string
	site          *knet.LoadedSite
	storage       storage.Backend
	workAdmission *dbaccess.Admission
	duration      time.Duration
	poolStats     sql.DBStats
	err           error
}

func collectLoadedStartupTenants(results []startupTenantResult) ([]*knet.LoadedSite, []string, *sql.DB, error) {
	var loadedSites []*knet.LoadedSite
	var domains []string
	var firstDB *sql.DB
	var startupErr error
	for _, result := range results {
		slog.Info("site startup load completed", "site", result.name, "duration_ms", result.duration.Milliseconds(), "loaded", result.site != nil, "db_open_connections", result.poolStats.OpenConnections, "db_idle_connections", result.poolStats.Idle, "db_wait_count", result.poolStats.WaitCount, "db_wait_duration_ms", result.poolStats.WaitDuration.Milliseconds(), "error", result.err)
		if result.err != nil {
			startupErr = errors.Join(startupErr, fmt.Errorf("load tenant %q during startup: %w", result.name, result.err))
		}
	}
	if startupErr != nil {
		for _, completed := range results {
			if completed.site == nil {
				continue
			}
			if err := completed.site.Close(context.Background()); err != nil {
				rollbackErr := fmt.Errorf("close tenant runtime %q during startup rollback: %w", completed.site.Name, err)
				slog.Warn("tenant runtime close failed during startup rollback", "site", completed.site.Name, "error", err)
				startupErr = errors.Join(startupErr, rollbackErr)
			}
		}
		return nil, nil, nil, startupErr
	}
	for _, result := range results {
		if result.site == nil {
			continue
		}
		result.site.RuntimeServices = &api.SiteRuntimeService{Storage: result.storage, WorkAdmission: result.workAdmission}
		loadedSites = append(loadedSites, result.site)
		if firstDB == nil {
			firstDB = result.site.DB
		}
		domains = append(domains, result.site.Config.Domains...)
	}
	return loadedSites, domains, firstDB, nil
}

func loadStartupTenants(parent context.Context, config StartupTenantConfig, dependencies startupTenantDependencies) []startupTenantResult {
	tracer := otel.Tracer("kora/engine/startup")
	ctx, startupSpan := tracer.Start(parent, "engine.load_sites",
		trace.WithAttributes(attribute.Int("kora.site.count", len(config.Sites))))
	defer startupSpan.End()

	results := make([]startupTenantResult, len(config.Sites))
	if len(config.Sites) == 0 {
		return results
	}
	siteKeys := make([]string, len(config.Sites))
	loadLocks := make(map[string]*sync.Mutex, len(config.Sites))
	for index, info := range config.Sites {
		cfg := site.ReconstructSiteConfigFromDBInfo(info, config.Common)
		key := cfg.DSN()
		if config.SharedPlatformDSN != "" && (cfg.DBType == "libsql" || cfg.DBUser == "") {
			key = config.SharedPlatformDSN
		}
		siteKeys[index] = key
		if loadLocks[key] == nil {
			loadLocks[key] = &sync.Mutex{}
		}
	}
	workerCount := int(config.Concurrency)
	if workerCount <= 0 {
		workerCount = defaultStartupTenantConcurrency
	}
	if workerCount > len(loadLocks) {
		workerCount = len(loadLocks)
	}
	startupSpan.SetAttributes(attribute.Int("kora.site.startup_workers", workerCount))

	jobs := make(chan int)
	var workers sync.WaitGroup
	var failed atomic.Bool
	for worker := 0; worker < workerCount; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				if failed.Load() {
					continue
				}
				lock := loadLocks[siteKeys[index]]
				lock.Lock()
				started := time.Now()
				result := loadStartupTenant(ctx, config.Sites[index], config, dependencies)
				result.duration = time.Since(started)
				if result.site != nil && result.site.DB != nil {
					result.poolStats = result.site.DB.Stats()
				}
				lock.Unlock()
				results[index] = result
				if result.err != nil {
					failed.Store(true)
				}
			}
		}()
	}
	for index := range config.Sites {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	for _, result := range results {
		if result.err != nil {
			startupSpan.SetStatus(codes.Error, "site initialization failed")
			break
		}
	}
	return results
}

func loadStartupTenant(ctx context.Context, info site.DBSiteInfo, config StartupTenantConfig, dependencies startupTenantDependencies) (result startupTenantResult) {
	result.name = info.Name
	var backend storage.Backend
	var runtime *knet.LoadedSite
	transferred := false
	defer func() {
		if transferred {
			return
		}
		var cleanupErr error
		if runtime != nil {
			cleanupErr = errors.Join(cleanupErr, runtime.Close(context.Background()))
		} else if closer, ok := backend.(io.Closer); ok {
			cleanupErr = errors.Join(cleanupErr, closer.Close())
		}
		if cleanupErr != nil {
			result.err = errors.Join(result.err, fmt.Errorf("rollback tenant startup resources for %s: %w", info.Name, cleanupErr))
		}
	}()

	tracer := otel.Tracer("kora/engine/startup")
	ctx, span := tracer.Start(ctx, "engine.load_site", trace.WithAttributes(attribute.String("kora.site.name", info.Name)))
	started := time.Now()
	defer func() {
		span.SetAttributes(attribute.Int64("kora.site.startup.duration_ms", time.Since(started).Milliseconds()))
		span.End()
	}()
	phase := func(name string) trace.Span {
		_, phaseSpan := tracer.Start(ctx, name, trace.WithAttributes(attribute.String("kora.site.name", info.Name)))
		return phaseSpan
	}

	siteCfg := site.ReconstructSiteConfigFromDBInfo(info, config.Common)
	phaseSpan := phase("engine.site.resolve_storage")
	var err error
	backend, err = config.ResolveStorage(siteCfg)
	endPhase(phaseSpan, err)
	if err != nil {
		return startupTenantResult{name: info.Name, err: fmt.Errorf("configuring storage for %s: %w", info.Name, err)}
	}
	workAdmission, err := dependencies.admission(siteCfg)
	if err != nil {
		return startupTenantResult{name: info.Name, storage: backend, err: fmt.Errorf("allocating work admission for %s: %w", info.Name, err)}
	}
	phaseSpan = phase("engine.site.activate_runtime")
	runtime, err = dependencies.loadRuntime(ctx, info, config.Common, config.BinaryVersion)
	endPhase(phaseSpan, err)
	if err != nil {
		return startupTenantResult{name: info.Name, storage: backend, err: fmt.Errorf("loading tenant runtime for %s: %w", info.Name, err)}
	}
	runtime.RuntimeServices = &api.SiteRuntimeService{Storage: backend, WorkAdmission: workAdmission}
	slog.Info("site loaded", "hostname", info.Name, "domains", siteCfg.Domains(), "doctypes", runtime.Registry.Len())
	transferred = true
	return startupTenantResult{name: info.Name, site: runtime, storage: backend, workAdmission: workAdmission}
}

func endPhase(span trace.Span, err error) {
	if err != nil {
		// Driver errors can embed DSNs or other connection details. Keep trace
		// status useful without serializing provider error text or credentials.
		span.SetStatus(codes.Error, "site initialization phase failed")
	}
	span.End()
}
