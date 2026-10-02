package cli

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"gopkg.in/yaml.v3"

	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/api"
	"github.com/asenawritescode/kora/auth"
	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/email"
	"github.com/asenawritescode/kora/ingress"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/natsprovider"
	knet "github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/scheduler"
	"github.com/asenawritescode/kora/schema"
	"github.com/asenawritescode/kora/script"
	"github.com/asenawritescode/kora/secret"
	"github.com/asenawritescode/kora/site"
	"github.com/asenawritescode/kora/storage"
	"github.com/asenawritescode/kora/webhook"
	"github.com/asenawritescode/kora/workspace"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Version is set at build time via -ldflags "-X github.com/asenawritescode/kora/cli.Version=...".
var Version = "dev"

func firstEnv(names ...string) string {
	for _, name := range names {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return ""
}

func firstEnvBool(def bool, names ...string) bool {
	for _, name := range names {
		if v := os.Getenv(name); v != "" {
			return v != "false" && v != "0"
		}
	}
	return def
}

// resolveStorage builds the storage backend (local or S3-compatible) for a site.
// Per-site FileStorage from the registry overrides the global KORA_STORAGE_BACKEND.
func resolveStorage(siteCfg *site.SiteConfig) (storage.Backend, error) {
	backend := firstEnv("KORA_STORAGE_BACKEND")
	s3Configured := firstEnv("KORA_STORAGE_S3_ENDPOINT") != "" ||
		firstEnv("KORA_STORAGE_S3_BUCKET") != "" ||
		firstEnv("KORA_STORAGE_S3_ACCESS_KEY") != "" ||
		firstEnv("KORA_STORAGE_S3_SECRET_KEY") != ""
	cfg := storage.Config{
		Backend:         backend,
		LocalPath:       os.Getenv("KORA_STORAGE_LOCAL_PATH"),
		S3Endpoint:      firstEnv("KORA_STORAGE_S3_ENDPOINT"),
		S3Region:        firstEnv("KORA_STORAGE_S3_REGION"),
		S3Bucket:        firstEnv("KORA_STORAGE_S3_BUCKET"),
		S3AccessKey:     firstEnv("KORA_STORAGE_S3_ACCESS_KEY"),
		S3SecretKey:     firstEnv("KORA_STORAGE_S3_SECRET_KEY"),
		S3UseSSL:        firstEnvBool(true, "KORA_STORAGE_S3_USE_SSL"),
		S3PublicBaseURL: firstEnv("KORA_STORAGE_S3_PUBLIC_URL"),
	}
	if siteCfg != nil && siteCfg.FileStorage != "" {
		cfg.Backend = siteCfg.FileStorage
	}
	if s3Configured {
		// Prefer S3 whenever the deployment provides S3 credentials/config.
		// This forces existing sites off container-local storage without requiring
		// registry edits first.
		cfg.Backend = "s3"
	}
	if siteCfg != nil && cfg.Backend == "s3" {
		if cfg.S3Bucket == "" {
			cfg.S3Bucket = siteCfg.StorageBucket
		}
		if cfg.S3Bucket == "" {
			cfg.S3Bucket = site.BucketNameForSite(siteCfg.Hostname)
		}
	}
	if cfg.Backend == "" && (cfg.S3Endpoint != "" || cfg.S3Bucket != "" || cfg.S3AccessKey != "" || cfg.S3SecretKey != "") {
		cfg.Backend = "s3"
	}
	if cfg.Backend == "" {
		cfg.Backend = "local"
	}
	if cfg.LocalPath == "" {
		cfg.LocalPath = "."
	}
	backendImpl, err := storage.New(cfg)
	if err != nil {
		return nil, err
	}
	if err := backendImpl.EnsureBucket(context.Background()); err != nil {
		return nil, err
	}
	return backendImpl, nil
}

var (
	httpPortFlag int
)

func init() {
	serveCmd.Flags().IntVar(&httpPortFlag, "port", 0, "HTTP port (overrides common config)")
}

func runServe() error {
	startupStarted := time.Now()
	// Load all config from a single source — validated once.
	sc := site.LoadStartupConfig()
	if err := sc.Validate(); err != nil {
		return err
	}
	if sc.DBDSN == "" {
		return errors.New("DB_DSN is required; Engine serves sites from the canonical platform directory")
	}

	// All config from env vars (no YAML files).
	common := site.CommonConfigFromEnv()
	configureLogging(common.LogLevel, common.LogFormat)
	otelShutdown, err := initOpenTelemetry(context.Background())
	if err != nil {
		return fmt.Errorf("initializing OpenTelemetry: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := otelShutdown(ctx); err != nil {
			slog.Warn("OpenTelemetry shutdown failed", "error", err)
		}
	}()
	startupCtx, startupSpan := otel.Tracer("kora/engine/startup").Start(context.Background(), "engine.startup")
	startupSpanEnded := false
	defer func() {
		if !startupSpanEnded {
			startupSpan.End()
		}
	}()

	// Validate platform DB credentials for site creation via console.
	if common.DBUser == "" || common.DBPassword == "" {
		slog.Warn("platform db_user or db_password not set — site creation from console UI will fail. Set KORA_DB_USER / KORA_DB_PASSWORD env vars.")
	}

	// Startup DB connection check. Keep connection open for console site creation.
	platformDB, err := sql.Open(sc.DBType, sc.DBDSN)
	if err != nil {
		slog.Error("startup db check: failed to open", "type", sc.DBType, "error", err)
		return fmt.Errorf("failed to open %s connection: %w", sc.DBType, err)
	}
	if err := platformDB.Ping(); err != nil {
		platformDB.Close()
		return fmt.Errorf("failed to ping %s: %w", sc.DBType, err)
	}
	tunePlatformDBPool(platformDB, sc.DBType)
	slog.Info("database connected", "type", sc.DBType)

	// Bootstrap the _kora_site_registry table on the platform database so
	// site creation via the console persists metadata that survives restarts.
	if err := site.BootstrapPlatformRegistry(platformDB, kdb.Resolve(common.DBType)); err != nil {
		return fmt.Errorf("bootstrapping platform registry: %w", err)
	}
	directory := site.NewSQLSiteRegistry(platformDB, common.DBType)
	var directoryStartCursor uint64
	// Capture the feed boundary before reading/loading the eager startup
	// snapshot. Any later commit is replayed; earlier commits are visible in
	// the snapshot, avoiding a snapshot/cursor race during a slow boot.
	directoryStartCursor, err = directory.CurrentDirectoryRevision()
	if err != nil {
		return fmt.Errorf("reading site directory startup revision: %w", err)
	}

	defer platformDB.Close()

	// Discover sites from the database (single source of truth).
	var dbSites []site.DBSiteInfo
	engineCellID, err := site.NormalizeRuntimeCellID(os.Getenv("KORA_ENGINE_CELL_ID"))
	if err != nil {
		return fmt.Errorf("invalid KORA_ENGINE_CELL_ID: %w", err)
	}
	dbSites, err = directory.GetSnapshotForCell(engineCellID)
	if err != nil {
		return fmt.Errorf("site discovery from platform directory failed: %w", err)
	}
	if len(dbSites) > 0 {
		slog.Info("sites discovered from database", "count", len(dbSites), "runtime_cell_configured", engineCellID != "")
	}
	if len(dbSites) == 0 {
		slog.Warn("no sites found — console-only mode. Use /console to create your first site.")
	}

	// Load tenant runtimes with a small fixed concurrency limit. Each tenant is
	// independent; bounded parallelism shortens startup without saturating MySQL.
	var loadedSites []*knet.LoadedSite
	var allDomains []string
	var firstDB *sql.DB
	siteStorages := make(map[string]storage.Backend)
	// Runtime-owned sidecars are indexed by canonical runtime name so directory
	// tombstones can stop them together with routing and the tenant DB pool.
	siteWebhookWorkers := make(map[string]*webhook.Worker)
	siteBuses := make(map[string]analytics.EventBus)
	siteMultiBuses := make(map[string]*analytics.MultiBus)
	siteRealtimeProviders := make(map[string]*natsprovider.Provider)
	siteOutboxes := make(map[string]outbox.Writer)
	siteCloudRelays := make(map[string]*analytics.CloudRelay)
	siteRuntimeContexts := make(map[string]context.Context)
	siteRuntimeCancels := make(map[string]context.CancelFunc)
	siteScriptStores := make(map[string]*script.Store)
	siteSecretStores := make(map[string]*secret.Store)
	var runtimeServices *api.SiteRuntimeServices
	var directoryConsumer *site.DirectoryConsumer
	var directoryConsumerID string
	var installRuntimeServices func(site.DBSiteInfo, *knet.LoadedSite) error
	var replaceRuntimeSite func(context.Context, site.DBSiteInfo) error
	var cancelDirectoryConsumer context.CancelFunc
	var directoryConsumerDone chan struct{}
	startupResults := loadStartupSites(startupCtx, dbSites, common)
	for _, result := range startupResults {
		slog.Info("site startup load completed", "site", result.name, "duration_ms", result.duration.Milliseconds(), "loaded", result.site != nil, "db_open_connections", result.poolStats.OpenConnections, "db_idle_connections", result.poolStats.Idle, "db_wait_count", result.poolStats.WaitCount, "db_wait_duration_ms", result.poolStats.WaitDuration.Milliseconds(), "error", result.err)
		if result.storage != nil {
			// Storage remains registered for sites whose database was unavailable,
			// matching the previous startup behavior.
			siteStorages[result.name] = result.storage
		}
		if result.err != nil {
			for _, completed := range startupResults {
				if completed.site != nil {
					if completed.site.AnalyticsWorker != nil {
						completed.site.AnalyticsWorker.Stop()
					}
					_ = completed.site.DB.Close()
				}
			}
			return result.err
		}
		if result.site == nil {
			continue
		}
		loadedSites = append(loadedSites, result.site)
		if firstDB == nil {
			firstDB = result.site.DB
		}
		allDomains = append(allDomains, result.site.Config.Domains...)
		siteStorages[result.site.Name] = result.storage
	}

	if len(loadedSites) == 0 {
		slog.Warn("no sites loaded — console-only mode. Use /console to create your first site.")
	}

	// Build site router and Gin engine.
	siteRouter := knet.NewSiteRouter(loadedSites)
	var cellGateway *ingress.Gateway
	if engineCellID != "" {
		cellURLs, err := ingress.ParseCellURLs(os.Getenv("KORA_ENGINE_CELL_URLS"))
		if err != nil {
			return fmt.Errorf("parse KORA_ENGINE_CELL_URLS: %w", err)
		}
		cellGateway, err = ingress.New(engineCellID, cellURLs)
		if err != nil {
			return fmt.Errorf("configure Engine cell ingress: %w", err)
		}
		descriptors, err := directory.GetDirectoryDescriptors()
		if err != nil {
			return fmt.Errorf("load credential-free directory snapshot for cell ingress: %w", err)
		}
		if err := cellGateway.ValidateDirectory(descriptors); err != nil {
			return fmt.Errorf("validate cell ingress placement: %w", err)
		}
		cellGateway.ReplaceDirectory(descriptors)
		for _, loaded := range loadedSites {
			cellGateway.MarkLocalReady(loaded.SiteID, true)
		}
		slog.Info("cell ingress ready", "cell_id", engineCellID, "directory_sites", len(descriptors), "remote_cell_urls", len(cellURLs))
	}
	siteRuntimeContext := func(name string) context.Context {
		if ctx := siteRuntimeContexts[name]; ctx != nil {
			return ctx
		}
		ctx, cancel := context.WithCancel(context.Background())
		siteRuntimeContexts[name] = ctx
		siteRuntimeCancels[name] = cancel
		return ctx
	}
	retireSiteRuntime := func(siteID string) {
		if cellGateway != nil {
			cellGateway.MarkLocalReady(siteID, false)
		}
		retired := siteRouter.RemoveSiteByID(siteID)
		if retired == nil {
			return
		}
		services, _ := retired.RuntimeServices.(api.SiteRuntimeService)
		runtimeServices.Remove(retired.Name)
		if cancel := siteRuntimeCancels[runtimeContextKey(retired.Name, retired.ConfigRevision)]; cancel != nil {
			cancel()
			delete(siteRuntimeCancels, runtimeContextKey(retired.Name, retired.ConfigRevision))
			delete(siteRuntimeContexts, runtimeContextKey(retired.Name, retired.ConfigRevision))
		}
		go drainRetiredRuntime(retired, services)
	}
	{
		directoryConsumerID = strings.TrimSpace(os.Getenv("KORA_SITE_DIRECTORY_CONSUMER_ID"))
		if directoryConsumerID == "" {
			hostname, _ := os.Hostname()
			// A stable identity lets the durable SQL cursor survive process restarts.
			// Replicated Engine instances must set KORA_SITE_DIRECTORY_CONSUMER_ID
			// explicitly to a unique, stable value per replica.
			if hostname == "" {
				hostname = "localhost"
			}
			directoryConsumerID = fmt.Sprintf("engine:%s", hostname)
		}
		consumer := &site.DirectoryConsumer{Registry: directory, ID: directoryConsumerID, PageSize: 100, InitialCursor: directoryStartCursor}
		if os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") != "" && !strings.EqualFold(os.Getenv("OTEL_METRICS_EXPORTER"), "none") {
			cellMetricID := engineCellID
			if cellMetricID == "" {
				cellMetricID = "default"
			}
			lagMetrics, metricErr := newDirectoryLagMetrics(cellMetricID)
			if metricErr != nil {
				return fmt.Errorf("initialize site-directory metrics: %w", metricErr)
			}
			monitorCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go monitorDirectoryLag(monitorCtx, directory, directoryConsumerID, lagMetrics)
		}
		consumer.Apply = func(ctx context.Context, change site.SiteDirectoryChange) error {
			descriptor := change.Descriptor
			if engineCellID != "" && descriptor.Status != "deleted" {
				current, err := directory.GetDescriptorByID(descriptor.SiteID)
				if err != nil {
					if errors.Is(err, sql.ErrNoRows) {
						if cellGateway != nil {
							cellGateway.ApplyDescriptor(contract.SiteDescriptor{SiteID: descriptor.SiteID, Status: "deleted"})
						}
						retireSiteRuntime(descriptor.SiteID)
						return nil
					}
					return fmt.Errorf("read current placement for site %s: %w", descriptor.SiteID, err)
				}
				descriptor = current
			}
			if descriptor.Status == "deleted" {
				if cellGateway != nil {
					cellGateway.ApplyDescriptor(descriptor)
				}
				retireSiteRuntime(descriptor.SiteID)
				return nil
			}
			if !site.SiteBelongsToRuntimeCell(descriptor.RuntimeCellID, engineCellID) {
				if cellGateway != nil {
					cellGateway.ApplyDescriptor(descriptor)
				}
				retireSiteRuntime(descriptor.SiteID)
				return nil
			}
			if loaded := siteRouter.SiteByID(descriptor.SiteID); loaded != nil {
				// Startup can have observed a newer snapshot than this consumer's
				// cursor. That makes old events safe no-ops, not replay failures.
				if loaded.ConfigRevision >= descriptor.ConfigRevision {
					if cellGateway != nil {
						cellGateway.ApplyDescriptor(descriptor)
						cellGateway.MarkLocalReady(descriptor.SiteID, true)
					}
					return nil
				}
				if descriptor.Status != "active" && descriptor.Status != "degraded" {
					if err := siteRouter.ApplySiteDescriptor(descriptor); err != nil {
						return err
					}
					if cellGateway != nil {
						cellGateway.ApplyDescriptor(descriptor)
					}
					return nil
				}
				info, err := directory.GetByID(descriptor.SiteID)
				if err != nil {
					return fmt.Errorf("load newer site revision %s: %w", descriptor.SiteID, err)
				}
				if err := replaceRuntimeSite(ctx, info); err != nil {
					return err
				}
				if cellGateway != nil {
					cellGateway.ApplyDescriptor(descriptor)
					cellGateway.MarkLocalReady(descriptor.SiteID, true)
				}
				return nil
			}
			if descriptor.Status != "active" && descriptor.Status != "degraded" {
				if cellGateway != nil {
					cellGateway.ApplyDescriptor(descriptor)
				}
				return nil
			}
			info, err := directory.GetByID(descriptor.SiteID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					// A later delete may have removed this site before a brand-new
					// consumer replays its historical create event. Startup snapshot
					// is authoritative for that absent site.
					if cellGateway != nil {
						cellGateway.ApplyDescriptor(contract.SiteDescriptor{SiteID: descriptor.SiteID, Status: "deleted"})
					}
					return nil
				}
				return fmt.Errorf("load directory site %s: %w", descriptor.SiteID, err)
			}
			if info.Status != "active" && info.Status != "degraded" {
				if cellGateway != nil {
					cellGateway.ApplyDescriptor(site.DescriptorFromDBSiteInfo(info))
				}
				return nil
			}
			loaded, err := loadRuntimeSite(info, common)
			if err != nil {
				return fmt.Errorf("initialize directory site %s: %w", info.Name, err)
			}
			if installRuntimeServices != nil {
				if err := installRuntimeServices(info, loaded); err != nil {
					if loaded.AnalyticsWorker != nil {
						loaded.AnalyticsWorker.Stop()
					}
					_ = loaded.DB.Close()
					return fmt.Errorf("initialize services for directory site %s: %w", info.Name, err)
				}
			}
			if err := siteRouter.AddSite(loaded); err != nil {
				if installRuntimeServices != nil {
					services, _ := runtimeServices.Get(info.Name)
					stopSiteServiceBundle(services)
					runtimeServices.Remove(info.Name)
				}
				key := runtimeContextKey(loaded.Name, loaded.ConfigRevision)
				if cancel := siteRuntimeCancels[key]; cancel != nil {
					cancel()
					delete(siteRuntimeCancels, key)
					delete(siteRuntimeContexts, key)
				}
				if loaded.AnalyticsWorker != nil {
					loaded.AnalyticsWorker.Stop()
				}
				_ = loaded.DB.Close()
				return fmt.Errorf("route directory site %s: %w", info.Name, err)
			}
			if cellGateway != nil {
				cellGateway.MarkLocalReady(info.SiteID, true)
				cellGateway.ApplyDescriptor(descriptor)
			}
			return nil
		}
		consumer.Resync = func(ctx context.Context, directoryRevision uint64) error {
			if cellGateway != nil {
				descriptors, err := directory.GetDirectoryDescriptors()
				if err != nil {
					return fmt.Errorf("read directory descriptors during ingress resync: %w", err)
				}
				if err := cellGateway.ValidateDirectory(descriptors); err != nil {
					return fmt.Errorf("validate ingress placement during directory resync: %w", err)
				}
				for _, descriptor := range descriptors {
					loaded := siteRouter.SiteByID(descriptor.SiteID)
					ready := site.SiteBelongsToRuntimeCell(descriptor.RuntimeCellID, engineCellID) && loaded != nil && loaded.ConfigRevision >= descriptor.ConfigRevision
					cellGateway.MarkLocalReady(descriptor.SiteID, ready)
				}
				cellGateway.ReplaceDirectory(descriptors)
			}
			infos, err := directory.GetDirectorySnapshotForCell(engineCellID)
			if err != nil {
				return fmt.Errorf("read authoritative site directory snapshot: %w", err)
			}
			present := make(map[string]struct{}, len(infos))
			for _, info := range infos {
				if info.SiteID == "" {
					continue
				}
				if !site.SiteBelongsToRuntimeCell(info.RuntimeCellID, engineCellID) {
					continue
				}
				present[info.SiteID] = struct{}{}
				loaded := siteRouter.SiteByID(info.SiteID)
				if loaded != nil {
					if loaded.ConfigRevision >= info.ConfigRevision {
						continue
					}
					if info.Status != "active" && info.Status != "degraded" {
						descriptor := site.DescriptorFromDBSiteInfo(info)
						descriptor.DirectoryRevision = directoryRevision
						if err := siteRouter.ApplySiteDescriptor(descriptor); err != nil {
							return err
						}
						continue
					}
					if err := replaceRuntimeSite(ctx, info); err != nil {
						return err
					}
					continue
				}
				if info.Status != "active" && info.Status != "degraded" {
					continue
				}
				added, err := loadRuntimeSite(info, common)
				if err != nil {
					return fmt.Errorf("load site %s during directory resync: %w", info.Name, err)
				}
				if installRuntimeServices != nil {
					if err := installRuntimeServices(info, added); err != nil {
						if added.AnalyticsWorker != nil {
							added.AnalyticsWorker.Stop()
						}
						_ = added.DB.Close()
						return fmt.Errorf("initialize services for site %s during directory resync: %w", info.Name, err)
					}
				}
				if err := siteRouter.AddSite(added); err != nil {
					if installRuntimeServices != nil {
						services, _ := runtimeServices.Get(info.Name)
						stopSiteServiceBundle(services)
						runtimeServices.Remove(info.Name)
					}
					key := runtimeContextKey(added.Name, added.ConfigRevision)
					if cancel := siteRuntimeCancels[key]; cancel != nil {
						cancel()
						delete(siteRuntimeCancels, key)
						delete(siteRuntimeContexts, key)
					}
					if added.AnalyticsWorker != nil {
						added.AnalyticsWorker.Stop()
					}
					if added.DB != nil {
						_ = added.DB.Close()
					}
					return fmt.Errorf("route site %s during directory resync: %w", info.Name, err)
				}
				if cellGateway != nil {
					cellGateway.MarkLocalReady(info.SiteID, true)
				}
			}
			for _, loaded := range siteRouter.AllSites() {
				if loaded.SiteID == "" {
					continue
				}
				if _, ok := present[loaded.SiteID]; ok {
					continue
				}
				retireSiteRuntime(loaded.SiteID)
			}
			_ = ctx
			return nil
		}
		directoryConsumer = consumer
	}
	if err := siteRouter.ValidateAliases(); err != nil {
		// Keep healthy/non-conflicting sites available, but make every ambiguous
		// host fail closed in middleware instead of routing by load order.
		slog.Error("site directory contains ambiguous identity or aliases; conflicting routes will be rejected", "error", err)
	}
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.RedirectTrailingSlash = false
	if err := router.SetTrustedProxies(common.TrustedProxies); err != nil {
		return fmt.Errorf("configure trusted proxies: %w", err)
	}

	router.Use(gin.Recovery())
	router.Use(knet.RequestIDMiddleware())
	router.Use(knet.SecurityHeadersMiddleware(common.TLSMode != "" && common.TLSMode != "off"))
	router.Use(knet.CORSMiddleware(nil))
	router.Use(siteRouter.Middleware())
	router.Use(knet.NewRateLimiter(float64(common.RateLimitRPS), common.RateLimitBurst).Middleware()) // 6. Per-user rate limiting
	router.POST("/_kora/admin/reload-site", func(c *gin.Context) {
		reloadToken := os.Getenv("KORA_RELOAD_TOKEN")
		if reloadToken == "" {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "reload token is not configured"})
			return
		}
		if c.GetHeader("Authorization") != "Bearer "+reloadToken {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		var req struct {
			Site string `json:"site"`
		}
		if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		req.Site = strings.TrimSpace(req.Site)
		if req.Site == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "site is required"})
			return
		}
		if existing := siteRouter.SiteByName(req.Site); existing != nil {
			// Site already loaded — rebuild its registry from DB to pick up config changes.
			infos, err := site.DiscoverSitesFromDB(platformDB)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			for _, info := range infos {
				if info.Name != req.Site {
					continue
				}
				reloaded, err := loadRuntimeSite(info, common)
				if err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
					return
				}
				if err := siteRouter.AddSite(reloaded); err != nil {
					if reloaded.DB != nil {
						_ = reloaded.DB.Close()
					}
					c.JSON(http.StatusConflict, gin.H{"error": "site_alias_conflict", "message": err.Error()})
					return
				}
				c.JSON(http.StatusOK, gin.H{"status": "reloaded", "site": req.Site})
				return
			}
			c.JSON(http.StatusNotFound, gin.H{"error": "site not found in registry"})
			return
		}
		infos, err := site.DiscoverSitesFromDB(platformDB)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		for _, info := range infos {
			if info.Name != req.Site {
				continue
			}
			loaded, err := loadRuntimeSite(info, common)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			if err := siteRouter.AddSite(loaded); err != nil {
				if loaded.DB != nil {
					_ = loaded.DB.Close()
				}
				c.JSON(http.StatusConflict, gin.H{"error": "site_alias_conflict", "message": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{"status": "loaded", "site": req.Site})
			return
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "site not found in registry"})
	})

	auth.SessionLifetime = time.Duration(common.SessionLifetimeHours) * time.Hour
	doctype.SetAdminRole(common.AdminRole)
	api.AppBranding = api.Branding{AppName: common.AppName, PrimaryColor: common.PrimaryColor}
	api.SetAPILimits(common.APIDefaultLimit, common.APIMaxLimit)
	api.BinaryVersion = Version

	// Fallback registry — used when no sites are loaded. Routes resolve via SiteRouter.
	primaryRegistry := doctype.NewRegistry()
	if len(loadedSites) > 0 {
		primaryRegistry = loadedSites[0].Registry
	}

	// Always register core routes — sites can be hot-added via console.
	sessionMgr := auth.NewSessionManager(firstDB)
	authMailer := newMailer(common)
	auth.RegisterAuthRoutes(router, sessionMgr, firstDB, authMailer)
	siteGuard := auth.NewSiteGuard(firstDB)
	auth.SetCSRFSecure(common.CSRFSecure)
	// Canonical v1 routes
	apiGroup := router.Group("/api/v1")
	apiGroup.Use(siteGuard.Middleware(false))
	apiGroup.Use(knet.CompressMiddleware()) // Gzip API responses
	// Legacy routes — same handlers, no deprecation headers
	apiLegacyGroup := router.Group("/api")
	apiLegacyGroup.Use(siteGuard.Middleware(false))
	apiLegacyGroup.Use(knet.CompressMiddleware()) // Gzip API responses
	txManager := &orm.TxManager{DB: firstDB, Registry: primaryRegistry, Dialect: kdb.Resolve(common.DBType)}

	// Config-defined command resources (KERNEL-008). Loaded from
	// KORA_COMMANDS_DIR when set; invalid definitions fail startup rather
	// than being silently discarded.
	var kernelCommands *kernel.CommandRegistry
	if dir := os.Getenv("KORA_COMMANDS_DIR"); dir != "" {
		reg, err := kernel.LoadCommandDir(dir)
		if err != nil {
			return fmt.Errorf("loading command definitions from %s: %w", dir, err)
		}
		kernelCommands = reg
		slog.Info("command definitions loaded", "dir", dir, "count", len(reg.List()))
	}

	publicV1Group := router.Group("/api/v1")
	publicV1Group.Use(knet.CompressMiddleware())
	publicLegacyGroup := router.Group("/api")
	publicLegacyGroup.Use(knet.CompressMiddleware())

	// Initialize script runner (embedded goja runtime, disabled if no scripts configured).
	var scriptRunner script.Runner

	// Parse HTTP allowlist from env var (comma-separated domains).
	httpAllowlistStr := os.Getenv("KORA_SCRIPTS_HTTP_ALLOWLIST")
	var httpAllowlist []string
	if httpAllowlistStr != "" {
		for _, d := range strings.Split(httpAllowlistStr, ",") {
			d = strings.TrimSpace(d)
			if d != "" {
				httpAllowlist = append(httpAllowlist, d)
			}
		}
	}

	// Check if any site has scripts enabled.
	scriptEnabled := os.Getenv("KORA_SCRIPTS_ENABLED")
	if scriptEnabled == "" || scriptEnabled == "true" {
		scriptRunner = script.NewEmbeddedRunner(script.DefaultEmbeddedConfig())
		slog.Info("script runner initialized", "pool_size", script.DefaultEmbeddedConfig().PoolSize)

		// Create stores per site.
		for _, s := range loadedSites {
			siteScriptStores[s.Name] = &script.Store{DB: s.DB, Dialect: kdb.Resolve(s.DBType)}
			siteSecretStores[s.Name] = secret.NewStore(s.DB, kdb.Resolve(s.DBType))
		}
		if len(httpAllowlist) > 0 {
			slog.Info("script HTTP allowlist configured", "domains", httpAllowlist)
		}
	} else {
		slog.Info("script runner disabled (KORA_SCRIPTS_ENABLED=false)")
	}

	var engineNATSProvider *natsprovider.Provider
	if natsEnabled() {
		natsCfg := natsprovider.FromEnv()
		natsCfg.Name = "kora-engine"
		candidate, connectErr := natsprovider.New(context.Background(), natsCfg)
		if connectErr != nil {
			slog.Warn("NATS unavailable; continuing with SQL directory polling and local event delivery", "error", connectErr)
		} else {
			bootstrapErr := candidate.Bootstrap(context.Background())
			if bootstrapErr == nil && directoryConsumer != nil {
				bootstrapErr = candidate.BootstrapSiteDirectory(context.Background())
			}
			if bootstrapErr != nil {
				candidate.Close()
				slog.Warn("NATS bootstrap failed; continuing with SQL directory polling and local event delivery", "error", bootstrapErr)
			} else {
				engineNATSProvider = candidate
			}
		}
	}
	cloudRelayCfg := analytics.LoadCloudRelayConfig()
	for _, s := range loadedSites {
		if s.AnalyticsEventBus != nil {
			// Wrap in MultiBus for webhook fan-out.
			mb, mbErr := analytics.NewMultiBus(s.AnalyticsEventBus)
			if mbErr == nil {
				siteBuses[s.Name] = mb
				siteMultiBuses[s.Name] = mb
				if cloudRelayCfg != nil {
					relay := analytics.NewCloudRelay(mb, s.Name, *cloudRelayCfg)
					relay.Start()
					siteCloudRelays[s.Name] = relay
				}
				// Start webhook worker for this site.
				w := webhook.NewWorker(s.DB, mb, s.Name)
				w.Start()
				siteWebhookWorkers[s.Name] = w
				slog.Info("webhook worker started", "site", s.Name)
			} else {
				slog.Warn("failed to create multi-bus for webhooks", "site", s.Name, "error", mbErr)
			}
		}
		if engineNATSProvider != nil {
			siteRealtimeProviders[s.Name] = engineNATSProvider
		}
	}
	for siteName, bus := range siteBuses {
		provider := siteRealtimeProviders[siteName]
		if provider == nil || bus == nil {
			continue
		}
		loaded := siteRouter.SiteByName(siteName)
		if loaded != nil {
			go runRealtimeBridge(siteRuntimeContext(runtimeContextKey(siteName, loaded.ConfigRevision)), siteName, bus, provider)
		}
	}
	for _, s := range loadedSites {
		if provider := siteRealtimeProviders[s.Name]; provider != nil {
			go runAnalyticsRebuildConsumer(siteRuntimeContext(runtimeContextKey(s.Name, s.ConfigRevision)), s.Name, provider, s.DB, kdb.Resolve(s.DBType), s.Registry)
		}
	}
	// Transactional outbox (RFC §8.1). It is enabled explicitly or whenever NATS
	// is enabled, because broker-backed deployments need durable cross-instance
	// event delivery.
	outboxEnabled := os.Getenv("KORA_OUTBOX") == "true" || os.Getenv("KORA_OUTBOX") == "1" || natsEnabled()
	if outboxEnabled {
		for _, s := range loadedSites {
			if s.DB == nil {
				continue
			}
			w := outbox.NewSQLWriter(kdb.Resolve(s.DBType))
			siteOutboxes[s.Name] = w

			// Choose the provider explicitly. Local remains the fallback unless the
			// operator sets KORA_EVENT_PROVIDER=nats and provides NATS config.
			var dest contract.EventPublisher
			var natsSideEffects *natsprovider.Provider
			if engineNATSProvider != nil {
				dest = engineNATSProvider
				natsSideEffects = engineNATSProvider
			} else if s.AnalyticsEventBus != nil {
				dest = analytics.NewLocalProvider(s.AnalyticsEventBus)
			} else {
				dest = analytics.NewLocalProvider(analytics.NewChannelBus(1000, analyticsSiteWALDir(analytics.LoadConfig().WALDir, s.Name)))
			}
			p := outbox.NewPublisher(s.DB, dest, kdb.Resolve(s.DBType))
			go runOutboxPublisher(siteRuntimeContext(runtimeContextKey(s.Name, s.ConfigRevision)), p)
			slog.Info("transactional outbox enabled", "site", s.Name)

			// Side effects consume the broker stream and fan back into the site bus.
			// This keeps the DB/outbox as the source of truth while making NATS the
			// transport for analytics and downstream consumers.
			if natsSideEffects != nil && s.AnalyticsEventBus != nil {
				bus := siteBuses[s.Name]
				if bus == nil {
					bus = s.AnalyticsEventBus
				}
				go runNATSOutboxSideEffects(siteRuntimeContext(runtimeContextKey(s.Name, s.ConfigRevision)), s.Name, natsSideEffects, bus)
			}
		}
	}
	runtimeServices = api.NewSiteRuntimeServices()
	for _, s := range loadedSites {
		bundle := api.SiteRuntimeService{
			EventBus: siteBuses[s.Name], Realtime: siteRealtimeProviders[s.Name],
			ScriptStore: siteScriptStores[s.Name], SecretStore: siteSecretStores[s.Name],
			WebhookWorker: siteWebhookWorkers[s.Name], Outbox: siteOutboxes[s.Name], Storage: siteStorages[s.Name],
			AnalyticsRelay: siteCloudRelays[s.Name],
		}
		runtimeServices.Replace(s.Name, bundle)
		s.RuntimeServices = bundle
	}
	installRuntimeServices = func(info site.DBSiteInfo, loaded *knet.LoadedSite) error {
		services := api.SiteRuntimeService{}
		cfg := site.ReconstructSiteConfigFromDBInfo(info, common)
		backend, err := resolveStorage(cfg)
		if err != nil {
			return fmt.Errorf("resolve storage: %w", err)
		}
		services.Storage = backend
		if scriptRunner != nil {
			services.ScriptStore = &script.Store{DB: loaded.DB, Dialect: kdb.Resolve(cfg.DBType)}
			services.SecretStore = secret.NewStore(loaded.DB, kdb.Resolve(cfg.DBType))
		}
		analyticsCfg := analytics.LoadConfig()
		dialect := kdb.Resolve(site.ReconstructSiteConfigFromDBInfo(info, common).DBType)
		if err := analytics.BootstrapTables(loaded.DB, dialect); err == nil {
			baseBus := analytics.NewChannelBus(analyticsCfg.ChannelSize, analyticsSiteWALDir(analyticsCfg.WALDir, loaded.Name))
			loaded.AnalyticsEventBus = baseBus
			loaded.AnalyticsWorker = analytics.NewWorker(baseBus, loaded.DB, dialect, loaded.Registry, loaded.Name, analyticsCfg)
			go loaded.AnalyticsWorker.Start()
			services.EventBus = baseBus
			if multiBus, err := analytics.NewMultiBus(baseBus); err == nil {
				services.EventBus = multiBus
				worker := webhook.NewWorker(loaded.DB, multiBus, loaded.Name)
				worker.Start()
				services.WebhookWorker = worker
				if cloudRelayCfg != nil {
					relay := analytics.NewCloudRelay(multiBus, loaded.Name, *cloudRelayCfg)
					relay.Start()
					services.AnalyticsRelay = relay
				}
			}
		}
		if engineNATSProvider != nil {
			services.Realtime = engineNATSProvider
			runtimeCtx := siteRuntimeContext(runtimeContextKey(loaded.Name, loaded.ConfigRevision))
			if services.EventBus != nil {
				go runRealtimeBridge(runtimeCtx, loaded.Name, services.EventBus, engineNATSProvider)
			}
			go runAnalyticsRebuildConsumer(runtimeCtx, loaded.Name, engineNATSProvider, loaded.DB, dialect, loaded.Registry)
		}
		if outboxEnabled {
			services.Outbox = outbox.NewSQLWriter(dialect)
			var dest contract.EventPublisher
			if engineNATSProvider != nil {
				dest = engineNATSProvider
			} else if services.EventBus != nil {
				dest = analytics.NewLocalProvider(services.EventBus)
			} else {
				localBus := analytics.NewChannelBus(1000, analyticsSiteWALDir(analytics.LoadConfig().WALDir, loaded.Name))
				dest = analytics.NewLocalProvider(localBus)
			}
			runtimeCtx := siteRuntimeContext(runtimeContextKey(loaded.Name, loaded.ConfigRevision))
			go runOutboxPublisher(runtimeCtx, outbox.NewPublisher(loaded.DB, dest, dialect))
			if engineNATSProvider != nil && services.EventBus != nil {
				go runNATSOutboxSideEffects(runtimeCtx, loaded.Name, engineNATSProvider, services.EventBus)
			}
		}
		loaded.RuntimeServices = services
		runtimeServices.Replace(loaded.Name, services)
		return nil
	}
	replaceRuntimeSite = func(ctx context.Context, info site.DBSiteInfo) error {
		next, err := loadRuntimeSite(info, common)
		if err != nil {
			return fmt.Errorf("load refreshed site runtime: %w", err)
		}
		if installRuntimeServices != nil {
			if err := installRuntimeServices(info, next); err != nil {
				if next.AnalyticsWorker != nil {
					next.AnalyticsWorker.Stop()
				}
				_ = next.DB.Close()
				return fmt.Errorf("install refreshed site services: %w", err)
			}
		}
		previous, err := siteRouter.ReplaceSite(next)
		if err != nil {
			services, _ := next.RuntimeServices.(api.SiteRuntimeService)
			stopSiteServiceBundle(services)
			runtimeServices.Remove(next.Name)
			if next.AnalyticsWorker != nil {
				next.AnalyticsWorker.Stop()
			}
			_ = next.DB.Close()
			return fmt.Errorf("publish refreshed site runtime: %w", err)
		}
		if previous != nil {
			key := runtimeContextKey(previous.Name, previous.ConfigRevision)
			if cancel := siteRuntimeCancels[key]; cancel != nil {
				cancel()
				delete(siteRuntimeCancels, key)
				delete(siteRuntimeContexts, key)
			}
			services, _ := previous.RuntimeServices.(api.SiteRuntimeService)
			go drainRetiredRuntime(previous, services)
		}
		_ = ctx
		return nil
	}

	// Start async hook worker (processes after_* hooks in background).
	asyncHookQueue := make(chan orm.AsyncHookRequest, 1000)
	go runAsyncHookWorker(asyncHookQueue, scriptRunner, siteRouter, runtimeServices, common.DBType, httpAllowlist)
	slog.Info("async hook worker started", "queue_size", 1000)
	if directoryConsumer != nil {
		consumerCtx, cancel := context.WithCancel(context.Background())
		cancelDirectoryConsumer = cancel
		directoryConsumerDone = make(chan struct{})
		defer func() {
			cancelDirectoryConsumer()
			<-directoryConsumerDone
		}()
		var workers sync.WaitGroup
		pollInterval := time.Second
		var wakeups <-chan uint64
		if engineNATSProvider != nil {
			wakeups, err = engineNATSProvider.SubscribeSiteDirectoryWakeups(consumerCtx, directoryConsumerID)
			if err != nil {
				slog.Warn("NATS directory wakeups unavailable; retaining one-second SQL polling", "error", err)
			} else {
				pollInterval = 30 * time.Second
			}
			registry := directoryConsumer.Registry
			digest := sha256.Sum256([]byte(directoryConsumerID))
			publisher := &site.DirectoryConsumer{
				Registry: registry, ID: "engine-directory-nats:" + hex.EncodeToString(digest[:]),
				PageSize: 100, InitialCursor: directoryStartCursor,
				Apply: func(ctx context.Context, change site.SiteDirectoryChange) error {
					return engineNATSProvider.PublishSiteDirectoryRevision(ctx, change.Cursor)
				},
				Resync: func(ctx context.Context, revision uint64) error {
					return engineNATSProvider.PublishSiteDirectoryRevision(ctx, revision)
				},
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				publisher.Run(consumerCtx, time.Second, func(err error) {
					slog.Warn("NATS directory relay will retry without advancing its SQL cursor", "error", err)
				})
			}()
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			directoryConsumer.Run(consumerCtx, pollInterval, func(err error) {
				slog.Warn("site directory consumer will retry without advancing its cursor", "consumer_id", directoryConsumer.ID, "error", err)
			})
		}()
		if wakeups != nil {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for {
					select {
					case <-consumerCtx.Done():
						return
					case _, ok := <-wakeups:
						if !ok {
							return
						}
						if _, err := directoryConsumer.ProcessBatch(consumerCtx); err != nil && !errors.Is(err, context.Canceled) {
							slog.Warn("site directory wakeup apply failed; SQL poll will retry", "error", err)
						}
					}
				}
			}()
		}
		go func() {
			workers.Wait()
			close(directoryConsumerDone)
		}()
	}

	api.RegisterRoutesOnGroupWithRuntimeServices(apiGroup, primaryRegistry, txManager, siteBuses, siteRealtimeProviders, scriptRunner, siteScriptStores, siteSecretStores, httpAllowlist, siteWebhookWorkers, asyncHookQueueSink(asyncHookQueue), siteOutboxes, siteStorages, kernelCommands, runtimeServices)
	api.RegisterRoutesOnGroupWithRuntimeServices(apiLegacyGroup, primaryRegistry, txManager, siteBuses, siteRealtimeProviders, scriptRunner, siteScriptStores, siteSecretStores, httpAllowlist, siteWebhookWorkers, asyncHookQueueSink(asyncHookQueue), siteOutboxes, siteStorages, kernelCommands, runtimeServices)
	api.RegisterPublicRoutesOnGroupWithRuntimeServices(publicV1Group, primaryRegistry, txManager, siteStorages, runtimeServices)
	api.RegisterPublicRoutesOnGroupWithRuntimeServices(publicLegacyGroup, primaryRegistry, txManager, siteStorages, runtimeServices)

	workspaceHandler := workspace.NewHandler(primaryRegistry)
	if spaIndex, _ := workspace.SPAFS().Open("index.html"); spaIndex != nil {
		spaIndex.Close()
		slog.Info("serving React SPA at /workspace")
		workspace.RegisterSPARoutes(router, siteRouter)
	} else {
		slog.Info("SPA not built, using HTMX templates at /workspace")
		knet.RegisterPathSiteRoutes(router, siteRouter, nil)
		workspaceGroup := router.Group("/workspace")
		workspaceGroup.Use(siteGuard.Middleware(false))
		workspaceHandler.RegisterRoutesOnGroup(workspaceGroup)
	}

	// System console — file first, fall back to env/baked-in defaults.
	systemGuard, err := auth.LoadSystemGuard("system_credentials.yaml")
	if err != nil {
		systemGuard = auth.LoadSystemGuardFromEnv()
		slog.Info("console using env/baked-in credentials (system_credentials.yaml not found)")
	}
	if systemGuard != nil {
		// Console API (React SPA-driven, Bearer token auth).
		// The /console frontend is served by the SPA via NoRoute handler.
		ch := api.NewConsoleHandler(systemGuard, siteRouter, common.DBType, common.DBHost, common.DBUser, common.DBPassword, common.DBPort, platformDB, sc.AllowConsoleOnboarding)
		ch.SiteStorages = siteStorages
		ch.RuntimeServices = runtimeServices
		ch.ResolveStorage = resolveStorage
		ch.PrepareRuntime = installRuntimeServices
		ch.DiscardRuntime = func(loaded *knet.LoadedSite) {
			if loaded == nil {
				return
			}
			key := runtimeContextKey(loaded.Name, loaded.ConfigRevision)
			if cancel := siteRuntimeCancels[key]; cancel != nil {
				cancel()
				delete(siteRuntimeCancels, key)
				delete(siteRuntimeContexts, key)
			}
			services, _ := loaded.RuntimeServices.(api.SiteRuntimeService)
			stopSiteServiceBundle(services)
			runtimeServices.Remove(loaded.Name)
			if loaded.AnalyticsWorker != nil {
				loaded.AnalyticsWorker.Stop()
			}
		}
		ch.Start()
		router.POST("/api/console/login", ch.HandleLogin)
		router.POST("/api/console/change-password", ch.HandleChangePassword)
		router.POST("/api/console/sites/onboard", ch.HandleOnboard) // public — no auth
		router.GET("/api/console/sites/onboard", ch.HandleOnboardJobs)
		router.GET("/api/console/sites/onboard/:job_id", ch.HandleOnboardStatus)
		router.GET("/api/console/sites", ch.RequireConsoleAuth, ch.HandleListSites)
		router.POST("/api/console/sites", ch.RequireConsoleAuth, ch.HandleCreateSite)
		router.PUT("/api/console/sites/:name", ch.RequireConsoleAuth, ch.HandleUpdateSite)
		router.DELETE("/api/console/sites/:name", ch.RequireConsoleAuth, ch.HandleDeleteSite)
		router.POST("/api/console/sites/:name/reset-password", ch.RequireConsoleAuth, ch.HandleResetSitePassword)
	}

	// Health + ping.
	router.GET("/api/v1/ping", func(c *gin.Context) { c.JSON(200, gin.H{"message": "pong", "version": Version}) })
	router.GET("/api/ping", func(c *gin.Context) { c.JSON(200, gin.H{"message": "pong", "version": Version}) })
	router.GET("/health", func(c *gin.Context) {
		dbStatus := "connected"
		checkDB := firstDB
		if checkDB == nil {
			checkDB = platformDB
		}
		if checkDB != nil {
			if err := checkDB.Ping(); err != nil {
				dbStatus = "disconnected"
			}
		} else {
			dbStatus = "unknown"
		}
		status := "ok"
		if dbStatus != "connected" {
			status = "degraded"
		}
		c.JSON(200, gin.H{"status": status, "db": dbStatus})
	})

	// Scheduler.
	if len(loadedSites) > 0 {
		startScheduler(firstDB, primaryRegistry, txManager, newMailer(common))
	}

	// Server.
	port := common.HTTPPort
	if httpPortFlag > 0 {
		port = httpPortFlag
	}
	addr := fmt.Sprintf(":%d", port)
	tlsCfg := &knet.TLSConfig{Mode: common.TLSMode, Email: common.TLSEmail}
	if len(allDomains) > 0 {
		tlsCfg.Domains = allDomains
	}
	var engineHandler http.Handler = router
	if cellGateway != nil {
		engineHandler = cellGateway.Wrap(engineHandler)
	}
	srv := knet.NewServer(otelhttp.NewHandler(engineHandler, "kora-engine"), addr, tlsCfg)
	if common.ReadTimeout > 0 {
		srv.ReadTimeout = time.Duration(common.ReadTimeout) * time.Second
	}
	if common.WriteTimeout > 0 {
		srv.WriteTimeout = time.Duration(common.WriteTimeout) * time.Second
	}
	if common.IdleTimeout > 0 {
		srv.IdleTimeout = time.Duration(common.IdleTimeout) * time.Second
	}
	var startDeletionWorker sync.Once
	startDeletionWorkerCh := make(chan struct{})
	deletionCtx, cancelDeletion := context.WithCancel(context.Background())
	deletionWorkerDone := make(chan struct{})
	go func() {
		defer close(deletionWorkerDone)
		select {
		case <-deletionCtx.Done():
			return
		case <-startDeletionWorkerCh:
		}
		retry := func() {
			ctx, cancel := context.WithTimeout(deletionCtx, 10*time.Second)
			err := site.RetryPendingSiteDeletions(ctx, platformDB, common.DBType, common)
			cancel()
			if err != nil {
				slog.Warn("pending site deletion cleanup will retry", "error", err)
			}
		}
		retry()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-deletionCtx.Done():
				return
			case <-ticker.C:
				retry()
			}
		}
	}()
	startPendingDeletionWorker := func() {
		startDeletionWorker.Do(func() { close(startDeletionWorkerCh) })
	}
	defer func() {
		cancelDeletion()
		<-deletionWorkerDone
	}()
	onReady := func() {
		var poolOpen, poolInUse, poolIdle, poolWaitCount int
		var poolWaitDuration time.Duration
		for _, loaded := range loadedSites {
			if loaded.DB == nil {
				continue
			}
			stats := loaded.DB.Stats()
			poolOpen += stats.OpenConnections
			poolInUse += stats.InUse
			poolIdle += stats.Idle
			poolWaitCount += int(stats.WaitCount)
			poolWaitDuration += stats.WaitDuration
		}
		stats := platformDB.Stats()
		poolOpen += stats.OpenConnections
		poolInUse += stats.InUse
		poolIdle += stats.Idle
		poolWaitCount += int(stats.WaitCount)
		poolWaitDuration += stats.WaitDuration
		var memory goruntime.MemStats
		goruntime.ReadMemStats(&memory)
		startupDuration := time.Since(startupStarted)
		startupSpan.SetAttributes(
			attribute.Int("kora.site.discovered", len(dbSites)),
			attribute.Int("kora.site.loaded", len(loadedSites)),
			attribute.Int64("kora.engine.startup.duration_ms", startupDuration.Milliseconds()),
			attribute.Int("kora.db.open_connections", poolOpen),
			attribute.Int("kora.db.in_use_connections", poolInUse),
			attribute.Int("kora.db.idle_connections", poolIdle),
			attribute.Int("kora.db.pool_wait_count", poolWaitCount),
			attribute.Int64("kora.db.pool_wait_duration_ms", poolWaitDuration.Milliseconds()),
			attribute.Int64("kora.runtime.heap_alloc_bytes", int64(memory.HeapAlloc)),
			attribute.Int64("kora.runtime.heap_sys_bytes", int64(memory.HeapSys)),
		)
		slog.Info("engine startup ready", "duration_ms", startupDuration.Milliseconds(), "sites_discovered", len(dbSites), "sites_loaded", len(loadedSites), "db_open_connections", poolOpen, "db_in_use_connections", poolInUse, "db_idle_connections", poolIdle, "db_pool_wait_count", poolWaitCount, "db_pool_wait_duration_ms", poolWaitDuration.Milliseconds(), "heap_alloc_bytes", memory.HeapAlloc, "heap_sys_bytes", memory.HeapSys)
		startupSpan.End()
		startupSpanEnded = true
		startPendingDeletionWorker()
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		slog.Info("received signal, shutting down gracefully", "signal", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			slog.Error("server shutdown error", "error", err)
		}
		cancelDeletion()
		<-deletionWorkerDone
		// Stop webhook workers.
		if cancelDirectoryConsumer != nil {
			cancelDirectoryConsumer()
			<-directoryConsumerDone
		}
		for _, s := range siteRouter.AllSites() {
			services, _ := runtimeServices.Get(s.Name)
			if worker := services.WebhookWorker; worker != nil {
				worker.Stop()
			}
			if relay := services.AnalyticsRelay; relay != nil {
				relay.Stop()
			}
			if cancel := siteRuntimeCancels[s.Name]; cancel != nil {
				cancel()
			}
			if s.AnalyticsWorker != nil {
				s.AnalyticsWorker.Stop()
			}
			if bus, ok := services.EventBus.(interface{ Close() error }); ok {
				_ = bus.Close()
			} else if s.AnalyticsEventBus != nil {
				_ = s.AnalyticsEventBus.Close()
			}
			if s.DB != nil {
				_ = s.DB.Close()
			}
		}
		if engineNATSProvider != nil {
			engineNATSProvider.Close()
		}
		if scriptRunner != nil {
			scriptRunner.Close()
		}
		slog.Info("server stopped")
	}()

	serveErr := srv.ListenAndServeReady(onReady)
	if errors.Is(serveErr, http.ErrServerClosed) {
		return nil
	}
	if serveErr != nil {
		startupSpan.RecordError(serveErr)
		startupSpan.SetStatus(codes.Error, "HTTP listener failed")
		if !startupSpanEnded {
			startupSpan.End()
			startupSpanEnded = true
		}
	}
	return serveErr
}

func analyticsSiteWALDir(baseDir, siteName string) string {
	cleanSite := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, strings.TrimSpace(siteName))
	if cleanSite == "" || cleanSite == "." || cleanSite == ".." {
		cleanSite = "unknown-site"
	}
	return filepath.Join(baseDir, cleanSite)
}

func runtimeContextKey(siteName string, revision uint64) string {
	if revision == 0 {
		return siteName
	}
	return fmt.Sprintf("%s#%d", siteName, revision)
}

func drainRetiredRuntime(runtime *knet.LoadedSite, services api.SiteRuntimeService) {
	if runtime == nil {
		return
	}
	if err := runtime.WaitForRequests(context.Background()); err != nil {
		return
	}
	if runtime.AnalyticsWorker != nil {
		runtime.AnalyticsWorker.Stop()
	}
	stopSiteServiceBundle(services)
	if runtime.AnalyticsEventBus != nil && services.EventBus == nil {
		_ = runtime.AnalyticsEventBus.Close()
	}
	if runtime.DB != nil {
		_ = runtime.DB.Close()
	}
}

func runRealtimeBridge(ctx context.Context, siteName string, bus analytics.EventBus, provider *natsprovider.Provider) {
	ch, err := bus.Subscribe()
	if err != nil {
		slog.Warn("realtime bridge subscribe failed", "site", siteName, "error", err)
		return
	}
	subjectPrefix := provider.Config().SubjectPrefix + ".realtime." + siteName + ".changes"
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			payload, err := json.Marshal(map[string]any{
				"id":          event.ID,
				"type":        "change",
				"transport":   "nats",
				"site":        event.Site,
				"resource":    "doctype:" + event.Doctype,
				"doctype":     event.Doctype,
				"doc_name":    event.DocName,
				"operation":   event.Operation,
				"occurred_at": event.Timestamp,
				"payload":     event.Data,
			})
			if err != nil {
				slog.Warn("realtime bridge marshal failed", "site", siteName, "error", err)
				continue
			}
			_ = provider.PublishSubject(ctx, subjectPrefix, payload, contract.NewEventID())
		}
	}
}

func runAnalyticsRebuildConsumer(ctx context.Context, siteName string, provider *natsprovider.Provider, database *sql.DB, dialect kdb.Dialect, registry *doctype.Registry) {
	cfg := provider.Config()
	cfg.ConsumerName = siteName + "-analytics-rebuilds"
	cfg.ConsumerSubject = cfg.SubjectPrefix + ".tasks.analytics-rebuild"
	consumer, err := natsprovider.NewConsumer(provider, cfg)
	if err != nil {
		slog.Warn("analytics rebuild consumer init failed", "site", siteName, "error", err)
		return
	}
	handler := func(ctx context.Context, delivery contract.Delivery) error {
		var request struct {
			JobID   string `json:"job_id"`
			Site    string `json:"site"`
			DocType string `json:"doctype"`
			From    string `json:"from"`
		}
		if err := json.Unmarshal(delivery.Data, &request); err != nil {
			return err
		}
		if request.JobID == "" || request.Site != siteName {
			return fmt.Errorf("invalid analytics rebuild job payload")
		}
		job, err := analytics.GetRebuildJob(database, dialect, siteName, request.JobID)
		if err != nil {
			return err
		}
		if job.Status == "completed" {
			return nil
		}
		if err := analytics.MarkRebuildRunning(database, dialect, siteName, request.JobID); err != nil {
			return err
		}
		from := job.From
		if request.From != "" {
			if parsed, parseErr := time.Parse("2006-01-02", request.From); parseErr == nil {
				from = parsed
			}
		}
		count, err := analytics.Backfill(database, dialect, siteName, registry, from, request.DocType)
		if err != nil {
			_ = analytics.MarkRebuildFailed(database, dialect, siteName, request.JobID, err)
			return err
		}
		return analytics.MarkRebuildCompleted(database, dialect, siteName, request.JobID, count)
	}
	slog.Info("analytics rebuild consumer started", "site", siteName, "consumer", cfg.ConsumerName)
	if err := consumer.Run(ctx, handler); err != nil && ctx.Err() == nil {
		slog.Warn("analytics rebuild consumer stopped", "site", siteName, "error", err)
	}
}

func tunePlatformDBPool(db *sql.DB, driver string) {
	switch driver {
	case "libsql":
		db.SetMaxIdleConns(0)
		db.SetConnMaxLifetime(25 * time.Second)
		db.SetConnMaxIdleTime(20 * time.Second)
	case "mysql":
		db.SetMaxIdleConns(5)
		db.SetConnMaxIdleTime(2 * time.Minute)
		db.SetConnMaxLifetime(10 * time.Minute)
	}
}

func runNATSOutboxSideEffects(ctx context.Context, siteName string, provider *natsprovider.Provider, bus analytics.EventBus) {
	cfg := provider.Config()
	cfg.ConsumerName = siteName + "-sideeffects"
	// The shared stream also carries browser realtime payloads, Cloud events,
	// tasks, and dead letters. Consuming the entire prefix feeds realtime
	// messages back into the analytics bus, which republishes them forever.
	// Outbox event types are canonically prefixed with "kora." before the
	// provider adds its own subject prefix, so subscribe only to that namespace.
	cfg.ConsumerSubject = outboxSideEffectSubject(cfg.SubjectPrefix)
	cfg.MaxDeliver = 5
	consumer, err := natsprovider.NewConsumer(provider, cfg)
	if err != nil {
		slog.Warn("nats side-effect bridge init failed", "site", siteName, "error", err)
		return
	}

	handler := func(ctx context.Context, delivery contract.Delivery) error {
		event, err := decodeOutboxDelivery(delivery)
		if err != nil {
			return err
		}
		// Every site has its own durable consumer on the shared stream. A
		// consumer must acknowledge events for other sites without publishing
		// them into its local bus.
		if event.Site != siteName {
			return nil
		}
		if bus != nil {
			return bus.Publish(event)
		}
		return nil
	}

	slog.Info("nats side-effect bridge started", "site", siteName, "consumer", cfg.ConsumerName)
	if err := consumer.Run(ctx, handler); err != nil {
		slog.Warn("nats side-effect bridge stopped", "site", siteName, "error", err)
	}
}

func outboxSideEffectSubject(prefix string) string {
	return prefix + "." + prefix + ".>"
}

func decodeOutboxDelivery(delivery contract.Delivery) (analytics.ChangeEvent, error) {
	var envelope contract.EventEnvelope
	if err := json.Unmarshal(delivery.Data, &envelope); err != nil {
		return analytics.ChangeEvent{}, err
	}
	if err := envelope.Validate(); err != nil {
		return analytics.ChangeEvent{}, fmt.Errorf("invalid outbox event envelope: %w", err)
	}

	var payload struct {
		Data    map[string]any `json:"data"`
		OldData map[string]any `json:"old_data"`
	}
	if len(envelope.Data) > 0 {
		if err := json.Unmarshal(envelope.Data, &payload); err != nil {
			return analytics.ChangeEvent{}, err
		}
	}

	return analytics.ChangeEvent{
		Site:       envelope.Site,
		Doctype:    envelope.AggregateType,
		DocName:    envelope.AggregateID,
		Operation:  outboxEventOperation(envelope.Type),
		Timestamp:  envelope.OccurredAt,
		ModifiedBy: envelope.Source,
		Data:       payload.Data,
		OldData:    payload.OldData,
	}, nil
}

func outboxEventOperation(eventType string) analytics.EventOp {
	switch {
	case strings.HasSuffix(eventType, ".after_insert"):
		return analytics.EventInsert
	case strings.HasSuffix(eventType, ".after_delete"):
		return analytics.EventDelete
	case strings.HasSuffix(eventType, ".after_submit"):
		return analytics.EventSubmit
	case strings.HasSuffix(eventType, ".after_cancel"):
		return analytics.EventCancel
	default:
		return analytics.EventUpdate
	}
}

func loadRuntimeSite(info site.DBSiteInfo, common *site.CommonConfig) (*knet.LoadedSite, error) {
	siteCfg := site.ReconstructSiteConfigFromDBInfo(info, common)
	dialect := kdb.Resolve(siteCfg.DBType)
	db, err := site.Connect(siteCfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to database: %w", err)
	}
	if err := site.BootstrapSystemTables(db, dialect); err != nil {
		db.Close()
		return nil, fmt.Errorf("bootstrapping system tables: %w", err)
	}
	store := configstore.NewStore(db, dialect)
	doctypes, err := store.LoadAll(info.Name)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("loading doctypes: %w", err)
	}
	roles, err := store.LoadRoles(info.Name)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("loading roles: %w", err)
	}
	permissions, err := store.LoadPermissions(info.Name)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("loading permissions: %w", err)
	}
	workflows, err := store.LoadWorkflows(info.Name)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("loading workflows: %w", err)
	}
	views, err := store.LoadViews(info.Name)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("loading views: %w", err)
	}

	registry := doctype.NewRegistry()
	registry.LoadFull(doctypes, roles, permissions)
	registry.Views.LoadFromDB(views)
	for _, wf := range workflows {
		registry.Workflows.Register(wf)
	}
	if err := schema.MigrateSiteFromRegistry(db, siteCfg.DBName, registry, dialect); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating site: %w", err)
	}
	return &knet.LoadedSite{
		SiteID: info.SiteID, ConfigRevision: info.ConfigRevision, Status: info.Status, DBType: siteCfg.DBType,
		Name: info.Name,
		Config: knet.SiteRouterConfig{
			Hostname:      info.Name,
			Domains:       siteCfg.Domains(),
			FileStorage:   siteCfg.FileStorage,
			StorageBucket: siteCfg.StorageBucket,
		},
		DB:       db,
		Registry: registry,
	}, nil
}

func stopSiteServiceBundle(services api.SiteRuntimeService) {
	if services.WebhookWorker != nil {
		services.WebhookWorker.Stop()
	}
	if services.AnalyticsRelay != nil {
		services.AnalyticsRelay.Stop()
	}
	if bus, ok := services.EventBus.(interface{ Close() error }); ok {
		_ = bus.Close()
	}
}

func startScheduler(db *sql.DB, registry *doctype.Registry, txManager *orm.TxManager, mailer *email.Sender) {
	cfg := loadSchedulerConfig()
	if len(cfg) == 0 {
		slog.Info("scheduler: no jobs configured")
		return
	}
	if mailer == nil {
		mailer = email.NewSender(&email.Config{From: "kora@localhost"})
	}
	sched := scheduler.New(db, registry, txManager, mailer)
	for _, job := range cfg {
		sched.RegisterJob(job)
	}
	sched.Start()
	slog.Info("scheduler started", "jobs", len(cfg))
}

func loadSchedulerConfig() []*scheduler.JobConfig {
	for _, p := range []string{"config/v0/fieldwork/scheduler.yaml", "scheduler.yaml"} {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cfg struct {
			Jobs []*scheduler.JobConfig `yaml:"jobs"`
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			continue
		}
		return cfg.Jobs
	}
	return nil
}

func newMailer(common *site.CommonConfig) *email.Sender {
	if common == nil {
		return email.NewSender(&email.Config{From: "kora@localhost"})
	}
	from := common.SMTPFrom
	if from == "" {
		from = common.SMTPUsername
	}
	if from == "" {
		from = "kora@localhost"
	}
	return email.NewSender(&email.Config{
		Host:     common.SMTPHost,
		Port:     common.SMTPPort,
		Username: common.SMTPUsername,
		Password: common.SMTPPassword,
		From:     from,
		TLSMode:  common.SMTPTLSMode,
	})
}

func configureLogging(level, format string) {
	var logLevel slog.Level
	switch level {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}
	var handler slog.Handler
	if format == "text" {
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})
	} else {
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})
	}
	slog.SetDefault(slog.New(handler))
}

// runAsyncHookWorker processes after_* hook requests from the async queue.
func runAsyncHookWorker(queue chan orm.AsyncHookRequest, runner script.Runner, siteRouter *knet.SiteRouter, runtimeServices *api.SiteRuntimeServices, dbType string, httpAllowlist []string) {
	for req := range queue {
		loadedSite := siteRouter.SiteByName(req.Site)
		if loadedSite == nil || loadedSite.DB == nil || loadedSite.Registry == nil {
			continue
		}

		dt := loadedSite.Registry.Get(req.Doctype)
		if dt == nil {
			slog.Warn("async hook worker: doctype not found", "doctype", req.Doctype, "site", req.Site)
			continue
		}

		loadedDBType := loadedSite.DBType
		if loadedDBType == "" {
			loadedDBType = dbType
		}
		tm := &orm.TxManager{
			DB:              loadedSite.DB,
			Registry:        loadedSite.Registry,
			Dialect:         kdb.Resolve(loadedDBType),
			ScriptRunner:    runner,
			SiteName:        req.Site,
			CurrentUser:     req.User,
			CurrentUserRole: req.UserRole,
			SkipHookScripts: append([]string(nil), req.SkipHookScripts...),
		}
		services, _ := runtimeServices.Get(req.Site)
		tm.ScriptStore = services.ScriptStore
		tm.ScriptProvider = api.NewScriptProvider(tm, loadedSite.Registry, req.Site, nil, httpAllowlist)

		doc := orm.DocumentFromMap(loadedSite.Registry, req.Doctype, req.Doc)
		if doc == nil {
			doc = doctype.NewDocument(req.Doctype)
		}
		var oldDoc *doctype.Document
		if req.OldDoc != nil {
			oldDoc = orm.DocumentFromMap(loadedSite.Registry, req.Doctype, req.OldDoc)
		}

		execReq := script.ExecuteRequest{
			Script:     req.Rec.Script,
			ScriptType: req.Rec.ScriptType,
			ScriptName: req.Rec.Name,
			DocType:    req.Doctype,
			Event:      req.Event,
			Document:   doc.ToMap(),
			User:       req.User,
			UserRoles:  []string{req.UserRole},
			Site:       req.Site,
			Provider:   tm.ScriptProvider,
		}
		if oldDoc != nil {
			execReq.OldDocument = oldDoc.ToMap()
		}

		result, execErr := runner.Execute(context.Background(), execReq)
		status := "success"
		errMsg := ""
		durationMs := 0
		if execErr != nil {
			status = "error"
			errMsg = execErr.Error()
		}
		if result != nil {
			durationMs = int(result.Duration.Milliseconds())
		}

		if services.ScriptStore != nil {
			_ = services.ScriptStore.LogExecution(req.Site, req.Rec, req.Doctype, doc.Name, req.Event, req.User, durationMs, status, errMsg)
		}
	}
}

type asyncHookQueueSink chan orm.AsyncHookRequest

func (q asyncHookQueueSink) Enqueue(ctx context.Context, req orm.AsyncHookRequest) error {
	select {
	case q <- req:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// runOutboxPublisher drains _kora_outbox in a loop, publishing due events through
// the configured destination. It is the Phase 1 background worker; in Phase 2 the
// destination becomes a NATS JetStream publisher behind the same contract.
func runOutboxPublisher(ctx context.Context, p *outbox.Publisher) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			publishCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if _, err := p.PublishDue(publishCtx, 100); err != nil && !errors.Is(err, context.Canceled) {
				slog.Warn("outbox publisher: publish due failed", "error", err)
			}
			cancel()
		}
	}
}
