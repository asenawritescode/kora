package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/configstore"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	knet "github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/schema"
	"github.com/asenawritescode/kora/site"
	"github.com/asenawritescode/kora/storage"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const siteStartupParallelism = 8

type startupSiteResult struct {
	name      string
	site      *knet.LoadedSite
	storage   storage.Backend
	duration  time.Duration
	poolStats sql.DBStats
	err       error
}

func loadStartupSites(parent context.Context, sites []site.DBSiteInfo, common *site.CommonConfig) []startupSiteResult {
	tracer := otel.Tracer("kora/engine/startup")
	ctx, startupSpan := tracer.Start(parent, "engine.load_sites",
		trace.WithAttributes(attribute.Int("kora.site.count", len(sites))))
	defer startupSpan.End()

	results := make([]startupSiteResult, len(sites))
	if len(sites) == 0 {
		return results
	}
	siteKeys := make([]string, len(sites))
	loadLocks := make(map[string]*sync.Mutex, len(sites))
	for index, info := range sites {
		cfg := site.ReconstructSiteConfigFromDBInfo(info, common)
		key := cfg.DSN()
		if platformDSN := os.Getenv("DB_DSN"); platformDSN != "" && (cfg.DBType == "libsql" || cfg.DBUser == "") {
			key = platformDSN
		}
		siteKeys[index] = key
		if loadLocks[key] == nil {
			loadLocks[key] = &sync.Mutex{}
		}
	}
	workerCount := siteStartupParallelism
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
				result := loadStartupSite(ctx, sites[index], common)
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
	for index := range sites {
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

func loadStartupSite(ctx context.Context, info site.DBSiteInfo, common *site.CommonConfig) startupSiteResult {
	tracer := otel.Tracer("kora/engine/startup")
	ctx, span := tracer.Start(ctx, "engine.load_site",
		trace.WithAttributes(attribute.String("kora.site.name", info.Name)))
	started := time.Now()
	defer func() {
		span.SetAttributes(attribute.Int64("kora.site.startup.duration_ms", time.Since(started).Milliseconds()))
	}()
	defer span.End()

	phase := func(name string) trace.Span {
		_, phaseSpan := tracer.Start(ctx, name, trace.WithAttributes(attribute.String("kora.site.name", info.Name)))
		return phaseSpan
	}

	siteCfg := site.ReconstructSiteConfigFromDBInfo(info, common)
	dialect := kdb.Resolve(siteCfg.DBType)
	span.SetAttributes(attribute.String("db.system", siteCfg.DBType))

	phaseSpan := phase("engine.site.resolve_storage")
	stBackend, err := resolveStorage(siteCfg)
	endPhase(phaseSpan, err)
	if err != nil {
		return startupSiteResult{name: info.Name, err: fmt.Errorf("configuring storage for %s: %w", info.Name, err)}
	}

	slog.Info("connecting to database", "site", info.Name, "db", siteCfg.DBName, "db_type", siteCfg.DBType)
	phaseSpan = phase("engine.site.connect")
	database, err := site.Connect(siteCfg)
	endPhase(phaseSpan, err)
	if err != nil {
		return startupSiteResult{name: info.Name, storage: stBackend, err: fmt.Errorf("connecting site %s: %w", info.Name, err)}
	}

	phaseSpan = phase("engine.site.bootstrap")
	err = site.BootstrapSystemTables(database, dialect)
	endPhase(phaseSpan, err)
	if err != nil {
		database.Close()
		return startupSiteResult{name: info.Name, err: fmt.Errorf("bootstrapping %s: %w", info.Name, err)}
	}

	phaseSpan = phase("engine.site.load_config")
	store := configstore.NewStore(database, dialect)
	doctypes, err := store.LoadAll(info.Name)
	if err != nil {
		endPhase(phaseSpan, err)
		database.Close()
		return startupSiteResult{name: info.Name, err: fmt.Errorf("loading doctypes for %s: %w", info.Name, err)}
	}
	roles, err := store.LoadRoles(info.Name)
	if err != nil {
		endPhase(phaseSpan, err)
		database.Close()
		return startupSiteResult{name: info.Name, err: fmt.Errorf("loading roles for %s: %w", info.Name, err)}
	}
	permissions, err := store.LoadPermissions(info.Name)
	if err != nil {
		endPhase(phaseSpan, err)
		database.Close()
		return startupSiteResult{name: info.Name, err: fmt.Errorf("loading permissions for %s: %w", info.Name, err)}
	}
	workflows, err := store.LoadWorkflows(info.Name)
	if err != nil {
		endPhase(phaseSpan, err)
		database.Close()
		return startupSiteResult{name: info.Name, err: fmt.Errorf("loading workflows for %s: %w", info.Name, err)}
	}
	views, err := store.LoadViews(info.Name)
	if err != nil {
		endPhase(phaseSpan, err)
		database.Close()
		return startupSiteResult{name: info.Name, err: fmt.Errorf("loading views for %s: %w", info.Name, err)}
	}
	minKoraVersion, err := activeConfigMinKoraVersion(database, dialect, info.Name)
	if err != nil {
		endPhase(phaseSpan, err)
		database.Close()
		return startupSiteResult{name: info.Name, err: fmt.Errorf("loading active config version for %s: %w", info.Name, err)}
	}
	endPhase(phaseSpan, nil)
	if minKoraVersion != "" && !doctype.MinVersionOK(Version, minKoraVersion) {
		slog.Warn("active config version requires newer kora binary",
			"site", info.Name, "required", minKoraVersion, "running", Version)
	}

	registry := doctype.NewRegistry()
	registry.LoadFull(doctypes, roles, permissions)
	registry.Views.LoadFromDB(views)
	for _, workflow := range workflows {
		registry.Workflows.Register(workflow)
	}

	phaseSpan = phase("engine.site.migrate_schema")
	err = schema.MigrateSiteFromRegistry(database, siteCfg.DBName, registry, dialect)
	endPhase(phaseSpan, err)
	if err != nil {
		database.Close()
		return startupSiteResult{name: info.Name, err: fmt.Errorf("migrating %s: %w", info.Name, err)}
	}

	phaseSpan = phase("engine.site.analytics_bootstrap")
	analyticsCfg := analytics.LoadConfig()
	var eventBus analytics.EventBus
	var analyticsWorker *analytics.Worker
	if err := analytics.BootstrapTables(database, dialect); err != nil {
		slog.Warn("analytics: bootstrap failed", "site", info.Name, "error", err)
		endPhase(phaseSpan, err)
	} else {
		eventBus = analytics.NewChannelBus(analyticsCfg.ChannelSize, analyticsSiteWALDir(analyticsCfg.WALDir, info.Name))
		analyticsWorker = analytics.NewWorker(eventBus, database, dialect, registry, info.Name, analyticsCfg)
		go analyticsWorker.Start()
		slog.Info("analytics enabled", "site", info.Name)
		endPhase(phaseSpan, nil)
	}

	domains := siteCfg.Domains()
	loaded := &knet.LoadedSite{
		SiteID: info.SiteID, ConfigRevision: info.ConfigRevision, Status: info.Status, DBType: siteCfg.DBType,
		Name: info.Name,
		Config: knet.SiteRouterConfig{
			Hostname: info.Name, Domains: domains,
			FileStorage: siteCfg.FileStorage, StorageBucket: siteCfg.StorageBucket,
		},
		DB: database, Registry: registry,
		AnalyticsEventBus: eventBus, AnalyticsWorker: analyticsWorker,
	}
	slog.Info("site loaded", "hostname", info.Name, "domains", domains, "doctypes", registry.Len())
	return startupSiteResult{name: info.Name, site: loaded, storage: stBackend}
}

func activeConfigMinKoraVersion(database *sql.DB, dialect kdb.Dialect, siteName string) (string, error) {
	var minVersion string
	err := database.QueryRow(kdb.Rebind(dialect,
		"SELECT COALESCE(min_kora_version, '') FROM _kora_config_version WHERE site = ? AND status = 'Active' ORDER BY version DESC LIMIT 1"),
		siteName,
	).Scan(&minVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return minVersion, nil
}

func endPhase(span trace.Span, err error) {
	if err != nil {
		// Driver errors can embed DSNs or other connection details. Keep trace
		// status useful without serializing provider error text or credentials.
		span.SetStatus(codes.Error, "site initialization phase failed")
	}
	span.End()
}
