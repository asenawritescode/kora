package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"

	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/api"
	"github.com/asenawritescode/kora/auth"
	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/email"
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
	serveSiteFlag string
	httpPortFlag  int
	configDirFlag string
)

func init() {
	serveCmd.Flags().StringVar(&serveSiteFlag, "site", "", "Site hostname to serve (default: all sites)")
	serveCmd.Flags().IntVar(&httpPortFlag, "port", 0, "HTTP port (overrides common config)")
	serveCmd.Flags().StringVar(&configDirFlag, "config-dir", "", "Config directory (env: KORA_CONFIG_DIR). Defaults to current directory.")
}

func runServe() error {
	// Load all config from a single source — validated once.
	sc := site.LoadStartupConfig()
	if err := sc.Validate(); err != nil {
		return err
	}

	configDir := configDirFlag
	if configDir == "" {
		configDir = sc.ConfigDir
	}

	// All config from env vars (no YAML files).
	common := site.CommonConfigFromEnv()
	configureLogging(common.LogLevel, common.LogFormat)

	// Validate platform DB credentials for site creation via console.
	if common.DBUser == "" || common.DBPassword == "" {
		slog.Warn("platform db_user or db_password not set — site creation from console UI will fail. Set KORA_DB_USER / KORA_DB_PASSWORD env vars.")
	}

	// Startup DB connection check. Keep connection open for console site creation.
	var platformDB *sql.DB
	if sc.DBDSN != "" {
		var err error
		platformDB, err = sql.Open(sc.DBType, sc.DBDSN)
		if err != nil {
			slog.Error("startup db check: failed to open", "type", sc.DBType, "error", err)
			return fmt.Errorf("failed to open %s connection: %w", sc.DBType, err)
		}
		if err := platformDB.Ping(); err != nil {
			platformDB.Close()
			slog.Error("startup db check: ping failed", "type", sc.DBType, "error", err)
			return fmt.Errorf("failed to ping %s: %w", sc.DBType, err)
		}
		tunePlatformDBPool(platformDB, sc.DBType)
		slog.Info("database connected", "type", sc.DBType)
	}

	// Bootstrap the _kora_site_registry table on the platform database so
	// site creation via the console persists metadata that survives restarts.
	if platformDB != nil {
		if err := site.BootstrapPlatformRegistry(platformDB, kdb.Resolve(common.DBType)); err != nil {
			return fmt.Errorf("bootstrapping platform registry: %w", err)
		}
	}

	// Close platformDB on shutdown if it was opened.
	if platformDB != nil {
		defer platformDB.Close()
	}

	// Discover sites from the database (single source of truth).
	var dbSites []site.DBSiteInfo
	var err error
	if serveSiteFlag == "" && platformDB != nil {
		dbSites, err = site.DiscoverSitesFromDB(platformDB)
		if err != nil {
			slog.Warn("site discovery from database failed", "error", err)
		} else if len(dbSites) > 0 {
			slog.Info("sites discovered from database", "count", len(dbSites))
		}
	}
	if serveSiteFlag != "" {
		dbSites = []site.DBSiteInfo{{Name: serveSiteFlag}}
	}
	if len(dbSites) == 0 {
		slog.Warn("no sites found — console-only mode. Use /console to create your first site.")
	}

	// Load all sites.
	var loadedSites []*knet.LoadedSite
	var allDomains []string
	var firstDB *sql.DB
	siteStorages := make(map[string]storage.Backend)

	for _, info := range dbSites {
		// Reconstruct site config from persisted registry metadata + platform defaults.
		siteCfg := site.ReconstructSiteConfigFromDBInfo(info, common)

		stBackend, stErr := resolveStorage(siteCfg)
		if stErr != nil {
			return fmt.Errorf("configuring storage for %s: %w", info.Name, stErr)
		}
		siteStorages[info.Name] = stBackend

		slog.Info("connecting to database", "site", info.Name, "db", siteCfg.DBName)
		db, err := site.Connect(siteCfg)
		if err != nil {
			slog.Warn("skipping site", "hostname", info.Name, "error", err)
			continue
		}
		if firstDB == nil {
			firstDB = db
		}

		if err := site.BootstrapSystemTables(db, kdb.Resolve(common.DBType)); err != nil {
			db.Close()
			return fmt.Errorf("bootstrapping %s: %w", info.Name, err)
		}

		store := configstore.NewStore(db, kdb.Resolve(common.DBType))
		doctypes, _ := store.LoadAll(info.Name)
		roles, _ := store.LoadRoles(info.Name)
		permissions, _ := store.LoadPermissions(info.Name)
		workflows, _ := store.LoadWorkflows(info.Name)
		views, _ := store.LoadViews(info.Name)

		// Check min_kora_version on the active config version at startup.
		var minKoraVersion string
		db.QueryRow(
			"SELECT COALESCE(min_kora_version, '') FROM _kora_config_version WHERE site = ? AND status = 'Active' ORDER BY version DESC LIMIT 1",
			info.Name,
		).Scan(&minKoraVersion)
		if minKoraVersion != "" && !doctype.MinVersionOK(Version, minKoraVersion) {
			slog.Warn("active config version requires newer kora binary",
				"site", info.Name,
				"required", minKoraVersion,
				"running", Version,
			)
		}

		registry := doctype.NewRegistry()
		registry.LoadFull(doctypes, roles, permissions)
		registry.Views.LoadFromDB(views)
		for _, wf := range workflows {
			registry.Workflows.Register(wf)
		}

		if err := schema.MigrateSiteFromRegistry(db, siteCfg.DBName, registry, kdb.Resolve(common.DBType)); err != nil {
			db.Close()
			return fmt.Errorf("migrating %s: %w", info.Name, err)
		}

		// Initialize analytics for this site on every deployment.
		analyticsCfg := analytics.LoadConfig()
		var siteEventBus analytics.EventBus
		var siteWorker *analytics.Worker
		if err := analytics.BootstrapTables(db, kdb.Resolve(common.DBType)); err != nil {
			slog.Warn("analytics: bootstrap failed", "site", info.Name, "error", err)
		} else {
			siteEventBus = analytics.NewChannelBus(analyticsCfg.ChannelSize, analyticsCfg.WALDir)
			siteWorker = analytics.NewWorker(siteEventBus, db, kdb.Resolve(common.DBType), registry, info.Name, analyticsCfg)
			go siteWorker.Start()
			slog.Info("analytics enabled", "site", info.Name)
		}

		domains := siteCfg.Domains()
		loadedSites = append(loadedSites, &knet.LoadedSite{
			Name: info.Name, Config: knet.SiteRouterConfig{
				Hostname:      info.Name,
				Domains:       domains,
				FileStorage:   siteCfg.FileStorage,
				StorageBucket: siteCfg.StorageBucket,
			},
			DB: db, Registry: registry, AnalyticsEventBus: siteEventBus, AnalyticsWorker: siteWorker,
		})
		allDomains = append(allDomains, domains...)
		slog.Info("site loaded", "hostname", info.Name, "domains", domains, "doctypes", registry.Len())
	}

	if len(loadedSites) == 0 {
		slog.Warn("no sites loaded — console-only mode. Use /console to create your first site.")
	}

	// Build site router and Gin engine.
	siteRouter := knet.NewSiteRouter(loadedSites)
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.RedirectTrailingSlash = false

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
		if platformDB == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "platform database is not configured"})
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
				siteRouter.AddSite(reloaded)
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
			siteRouter.AddSite(loaded)
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
	siteScriptStores := make(map[string]*script.Store)
	siteSecretStores := make(map[string]*secret.Store)

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
			siteScriptStores[s.Name] = &script.Store{DB: s.DB}
			siteSecretStores[s.Name] = secret.NewStore(s.DB)
		}
		if len(httpAllowlist) > 0 {
			slog.Info("script HTTP allowlist configured", "domains", httpAllowlist)
		}
	} else {
		slog.Info("script runner disabled (KORA_SCRIPTS_ENABLED=false)")
	}

	siteBuses := make(map[string]analytics.EventBus)
	siteMultiBuses := make(map[string]*analytics.MultiBus)
	siteWebhookWorkers := make(map[string]*webhook.Worker)
	siteRealtimeProviders := make(map[string]*natsprovider.Provider)
	cloudRelayCfg := analytics.LoadCloudRelayConfig()
	for _, s := range loadedSites {
		if s.AnalyticsEventBus != nil {
			siteBuses[s.Name] = s.AnalyticsEventBus
			// Wrap in MultiBus for webhook fan-out.
			mb, mbErr := analytics.NewMultiBus(s.AnalyticsEventBus)
			if mbErr == nil {
				siteMultiBuses[s.Name] = mb
				if cloudRelayCfg != nil {
					relay := analytics.NewCloudRelay(mb, s.Name, *cloudRelayCfg)
					relay.Start()
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
		if natsEnabled() {
			natsCfg := natsprovider.FromEnv()
			natsCfg.Name = s.Name + "-realtime"
			p, err := natsprovider.New(context.Background(), natsCfg)
			if err != nil {
				return fmt.Errorf("connecting NATS provider for realtime: %w", err)
			}
			if err := p.Bootstrap(context.Background()); err != nil {
				p.Close()
				return fmt.Errorf("bootstrapping NATS provider for realtime: %w", err)
			}
			siteRealtimeProviders[s.Name] = p
		}
	}
	for siteName, bus := range siteBuses {
		provider := siteRealtimeProviders[siteName]
		if provider == nil || bus == nil {
			continue
		}
		go runRealtimeBridge(context.Background(), siteName, bus, provider)
	}
	// Transactional outbox (RFC §8.1). Opt-in via KORA_OUTBOX=true so the default
	// durability mode never changes silently.
	siteOutboxes := make(map[string]outbox.Writer)
	if v := os.Getenv("KORA_OUTBOX"); v == "true" || v == "1" {
		for _, s := range loadedSites {
			if s.DB == nil {
				continue
			}
			w := outbox.NewSQLWriter()
			siteOutboxes[s.Name] = w

			// Choose the provider explicitly. Local remains the fallback unless the
			// operator sets KORA_EVENT_PROVIDER=nats and provides NATS config.
			var dest contract.EventPublisher
			var natsSideEffects *natsprovider.Provider
			if natsEnabled() {
				natsCfg := natsprovider.FromEnv()
				natsCfg.Name = s.Name + "-outbox"
				p, err := natsprovider.New(context.Background(), natsCfg)
				if err != nil {
					return fmt.Errorf("connecting NATS provider for outbox: %w", err)
				}
				if err := p.Bootstrap(context.Background()); err != nil {
					p.Close()
					return fmt.Errorf("bootstrapping NATS provider for outbox: %w", err)
				}
				dest = p
				natsSideEffects = p
			} else if s.AnalyticsEventBus != nil {
				dest = analytics.NewLocalProvider(s.AnalyticsEventBus)
			} else {
				dest = analytics.NewLocalProvider(analytics.NewChannelBus(1000, analytics.LoadConfig().WALDir))
			}
			p := outbox.NewPublisher(s.DB, dest)
			go runOutboxPublisher(p)
			slog.Info("transactional outbox enabled", "site", s.Name)

			// Side effects consume the broker stream and fan back into the site bus.
			// This keeps the DB/outbox as the source of truth while making NATS the
			// transport for analytics and downstream consumers.
			if natsSideEffects != nil && s.AnalyticsEventBus != nil {
				go runNATSOutboxSideEffects(context.Background(), s.Name, natsSideEffects, s.AnalyticsEventBus)
			}
		}
	}

	// Start async hook worker (processes after_* hooks in background).
	asyncHookQueue := make(chan orm.AsyncHookRequest, 1000)
	go runAsyncHookWorker(asyncHookQueue, scriptRunner, siteScriptStores, loadedSites, common.DBType, httpAllowlist)
	slog.Info("async hook worker started", "queue_size", 1000)

	api.RegisterRoutesOnGroupWithAnalytics(apiGroup, primaryRegistry, txManager, siteBuses, siteRealtimeProviders, scriptRunner, siteScriptStores, siteSecretStores, httpAllowlist, siteWebhookWorkers, asyncHookQueueSink(asyncHookQueue), siteOutboxes, siteStorages, kernelCommands)
	api.RegisterRoutesOnGroupWithAnalytics(apiLegacyGroup, primaryRegistry, txManager, siteBuses, siteRealtimeProviders, scriptRunner, siteScriptStores, siteSecretStores, httpAllowlist, siteWebhookWorkers, asyncHookQueueSink(asyncHookQueue), siteOutboxes, siteStorages, kernelCommands)
	api.RegisterPublicRoutesOnGroup(publicV1Group, primaryRegistry, txManager, siteStorages)
	api.RegisterPublicRoutesOnGroup(publicLegacyGroup, primaryRegistry, txManager, siteStorages)

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
		ch := api.NewConsoleHandler(systemGuard, siteRouter, common.DBType, common.DBHost, common.DBUser, common.DBPassword, 3306, platformDB, sc.AllowConsoleOnboarding)
		ch.SiteStorages = siteStorages
		ch.ResolveStorage = resolveStorage
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
	srv := knet.NewServer(router, addr, tlsCfg)
	if common.ReadTimeout > 0 {
		srv.ReadTimeout = time.Duration(common.ReadTimeout) * time.Second
	}
	if common.WriteTimeout > 0 {
		srv.WriteTimeout = time.Duration(common.WriteTimeout) * time.Second
	}
	if common.IdleTimeout > 0 {
		srv.IdleTimeout = time.Duration(common.IdleTimeout) * time.Second
	}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		sig := <-sigCh
		slog.Info("received signal, shutting down gracefully", "signal", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			slog.Error("server shutdown error", "error", err)
		}
		// Stop webhook workers.
		for _, w := range siteWebhookWorkers {
			w.Stop()
		}
		for _, s := range loadedSites {
			s.DB.Close()
		}
		if scriptRunner != nil {
			scriptRunner.Close()
		}
		slog.Info("server stopped")
	}()

	return srv.ListenAndServe()
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
				"type":        "change",
				"transport":   "nats",
				"site":        event.Site,
				"resource":    "doctype:" + event.Doctype,
				"doctype":     event.Doctype,
				"doc_name":    event.DocName,
				"operation":   event.Operation,
				"occurred_at": event.Timestamp,
			})
			if err != nil {
				slog.Warn("realtime bridge marshal failed", "site", siteName, "error", err)
				continue
			}
			_ = provider.PublishSubject(ctx, subjectPrefix, payload, contract.NewEventID())
		}
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

func decodeOutboxDelivery(delivery contract.Delivery) (analytics.ChangeEvent, error) {
	var envelope contract.EventEnvelope
	if err := json.Unmarshal(delivery.Data, &envelope); err != nil {
		return analytics.ChangeEvent{}, err
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
	db, err := site.Connect(siteCfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to database: %w", err)
	}
	if err := site.BootstrapSystemTables(db, kdb.Resolve(common.DBType)); err != nil {
		db.Close()
		return nil, fmt.Errorf("bootstrapping system tables: %w", err)
	}
	store := configstore.NewStore(db, kdb.Resolve(common.DBType))
	doctypes, _ := store.LoadAll(info.Name)
	roles, _ := store.LoadRoles(info.Name)
	permissions, _ := store.LoadPermissions(info.Name)
	workflows, _ := store.LoadWorkflows(info.Name)
	views, _ := store.LoadViews(info.Name)

	registry := doctype.NewRegistry()
	registry.LoadFull(doctypes, roles, permissions)
	registry.Views.LoadFromDB(views)
	for _, wf := range workflows {
		registry.Workflows.Register(wf)
	}
	if err := schema.MigrateSiteFromRegistry(db, siteCfg.DBName, registry, kdb.Resolve(common.DBType)); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating site: %w", err)
	}
	return &knet.LoadedSite{
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
	for _, p := range []string{"config/fieldwork/scheduler.yaml", "scheduler.yaml"} {
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
func runAsyncHookWorker(queue chan orm.AsyncHookRequest, runner script.Runner, stores map[string]*script.Store, sites []*knet.LoadedSite, dbType string, httpAllowlist []string) {
	// Build a site runtime lookup.
	sitesByName := make(map[string]*knet.LoadedSite)
	for _, s := range sites {
		sitesByName[s.Name] = s
	}

	for req := range queue {
		loadedSite, ok := sitesByName[req.Site]
		if !ok || loadedSite.DB == nil || loadedSite.Registry == nil {
			continue
		}

		dt := loadedSite.Registry.Get(req.Doctype)
		if dt == nil {
			slog.Warn("async hook worker: doctype not found", "doctype", req.Doctype, "site", req.Site)
			continue
		}

		tm := &orm.TxManager{
			DB:              loadedSite.DB,
			Registry:        loadedSite.Registry,
			Dialect:         kdb.Resolve(dbType),
			ScriptRunner:    runner,
			SiteName:        req.Site,
			CurrentUser:     req.User,
			CurrentUserRole: req.UserRole,
		}
		if store, ok := stores[req.Site]; ok {
			tm.ScriptStore = store
		}
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

		if store, ok := stores[req.Site]; ok {
			_ = store.LogExecution(req.Site, req.Rec, req.Doctype, doc.Name, req.Event, req.User, durationMs, status, errMsg)
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
func runOutboxPublisher(p *outbox.Publisher) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if _, err := p.PublishDue(ctx, 100); err != nil {
			slog.Warn("outbox publisher: publish due failed", "error", err)
		}
		cancel()
	}
}
