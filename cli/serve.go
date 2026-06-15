package cli

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"

	"github.com/asenawritescode/kora/api"
	"github.com/asenawritescode/kora/auth"
	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/console"
	"github.com/asenawritescode/kora/workspace"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/email"
	knet "github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/scheduler"
	"github.com/asenawritescode/kora/schema"
	"github.com/asenawritescode/kora/site"
)

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
	configDir := configDirFlag
	if configDir == "" {
		configDir = os.Getenv("KORA_CONFIG_DIR")
	}
	if configDir == "" {
		configDir = "."
	}

	common, err := site.LoadCommonConfig(filepath.Join(configDir, "common_site_config.yaml"))
	if err != nil {
		slog.Warn("common config not found, using env defaults", "path", configDir)
		common = site.CommonConfigFromEnv()
	}
	configureLogging(common.LogLevel, common.LogFormat)

	// Discover sites.
	hostnames := []string{serveSiteFlag}
	if serveSiteFlag == "" {
		hostnames, err = site.DiscoverSites(filepath.Join(configDir, "sites"))
		if err != nil {
			return fmt.Errorf("discovering sites: %w", err)
		}
	}
	if len(hostnames) == 0 {
		slog.Warn("no sites found — console-only mode. Use /console to create your first site.")
	}

	// Load all sites.
	var loadedSites []*knet.LoadedSite
	var allDomains []string
	var firstDB *sql.DB

	for _, hostname := range hostnames {
		siteCfg, err := site.LoadSiteConfig(filepath.Join(configDir, "sites", hostname, "site_config.yaml"))
		if err != nil {
			slog.Warn("skipping site", "hostname", hostname, "error", err)
			continue
		}
		if siteCfg.DBHost == "" {
			siteCfg.DBHost = common.DBHost
		}

		slog.Info("connecting to database", "site", hostname, "db", siteCfg.DBName)
		db, err := site.Connect(siteCfg)
		if err != nil {
			slog.Warn("skipping site", "hostname", hostname, "error", err)
			continue
		}
		if firstDB == nil {
			firstDB = db
		}

		if err := bootstrapSystemTables(db); err != nil {
			db.Close()
			return fmt.Errorf("bootstrapping %s: %w", hostname, err)
		}

		store := configstore.NewStore(db)
		doctypes, _ := store.LoadAll()
		roles, _ := store.LoadRoles()
		permissions, _ := store.LoadPermissions()
		workflows, _ := store.LoadWorkflows()

		registry := doctype.NewRegistry()
		registry.LoadFull(doctypes, roles, permissions)
		for _, wf := range workflows {
			registry.Workflows.Register(wf)
		}

		if err := schema.MigrateSite(db, siteCfg.DBName, registry); err != nil {
			db.Close()
			return fmt.Errorf("migrating %s: %w", hostname, err)
		}

		domains := siteCfg.Domains()
		loadedSites = append(loadedSites, &knet.LoadedSite{
			Name: hostname, Config: knet.SiteRouterConfig{Hostname: hostname, Domains: domains},
			DB: db, Registry: registry,
		})
		allDomains = append(allDomains, domains...)
		slog.Info("site loaded", "hostname", hostname, "domains", domains, "doctypes", registry.Len())
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
	router.Use(knet.NewRateLimiter(float64(common.RateLimitRPS), common.RateLimitBurst).Middleware())

	auth.SessionLifetime = time.Duration(common.SessionLifetimeHours) * time.Hour
	doctype.SetAdminRole(common.AdminRole)
	api.AppBranding = api.Branding{AppName: common.AppName, PrimaryColor: common.PrimaryColor}
	api.SetAPILimits(common.APIDefaultLimit, common.APIMaxLimit)

	// Fallback registry — used when no sites are loaded. Routes resolve via SiteRouter.
	primaryRegistry := doctype.NewRegistry()
	if len(loadedSites) > 0 {
		primaryRegistry = loadedSites[0].Registry
	}

	// Always register core routes — sites can be hot-added via console.
	sessionMgr := auth.NewSessionManager(firstDB)
	auth.RegisterAuthRoutes(router, sessionMgr, firstDB)
	siteGuard := auth.NewSiteGuard(firstDB)
	auth.SetCSRFSecure(common.CSRFSecure)
	apiGroup := router.Group("/api")
	apiGroup.Use(siteGuard.Middleware(false))
	txManager := &orm.TxManager{DB: firstDB, Registry: primaryRegistry}
	api.RegisterRoutesOnGroup(apiGroup, primaryRegistry, txManager)

	workspaceHandler := workspace.NewHandler(primaryRegistry)
	if spaIndex, _ := workspace.SPAFS().Open("index.html"); spaIndex != nil {
		spaIndex.Close()
		slog.Info("serving React SPA at /workspace")
		workspace.RegisterSPARoutes(router, siteRouter)
	} else {
		slog.Info("SPA not built, using HTMX templates at /workspace")
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
		var consoleSites []console.SiteInfo
		for _, s := range loadedSites {
			consoleSites = append(consoleSites, console.SiteInfo{
				Name: s.Name, DBName: s.Config.Hostname, Domains: s.Config.Domains,
				DocTypes: s.Registry.Len(), Workflows: 0, Status: "active",
			})
		}
		consoleHandler := console.NewHandler(systemGuard, consoleSites, common.Version, siteRouter)
		consoleHandler.RegisterRoutes(router)

		// Console API (React-driven, Bearer token auth).
		ch := api.NewConsoleHandler(systemGuard, siteRouter)
		router.POST("/api/console/login", ch.HandleLogin)
		router.POST("/api/console/change-password", ch.HandleChangePassword)
		router.GET("/api/console/sites", ch.RequireConsoleAuth, ch.HandleListSites)
		router.POST("/api/console/sites", ch.RequireConsoleAuth, ch.HandleCreateSite)
	}

	// Health + ping.
	router.GET("/api/ping", func(c *gin.Context) { c.JSON(200, gin.H{"message": "pong"}) })
	router.GET("/health", func(c *gin.Context) {
		dbStatus := "connected"
		if firstDB != nil {
			if err := firstDB.Ping(); err != nil { dbStatus = "disconnected" }
		} else { dbStatus = "unknown" }
		status := "ok"
		if dbStatus != "connected" { status = "degraded" }
		c.JSON(200, gin.H{"status": status, "db": dbStatus})
	})

	// Scheduler.
	if len(loadedSites) > 0 {
		startScheduler(firstDB, primaryRegistry, txManager)
	}

	// Server.
	port := common.HTTPPort
	if httpPortFlag > 0 { port = httpPortFlag }
	addr := fmt.Sprintf(":%d", port)
	tlsCfg := &knet.TLSConfig{Mode: common.TLSMode, Email: common.TLSEmail}
	if len(allDomains) > 0 { tlsCfg.Domains = allDomains }
	srv := knet.NewServer(router, addr, tlsCfg)
	if common.ReadTimeout > 0 { srv.ReadTimeout = time.Duration(common.ReadTimeout) * time.Second }
	if common.WriteTimeout > 0 { srv.WriteTimeout = time.Duration(common.WriteTimeout) * time.Second }
	if common.IdleTimeout > 0 { srv.IdleTimeout = time.Duration(common.IdleTimeout) * time.Second }

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		sig := <-sigCh
		slog.Info("received signal, shutting down gracefully", "signal", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil { slog.Error("server shutdown error", "error", err) }
		for _, s := range loadedSites { s.DB.Close() }
		slog.Info("server stopped")
	}()

	return srv.ListenAndServe()
}


func startScheduler(db *sql.DB, registry *doctype.Registry, txManager *orm.TxManager) {
	cfg := loadSchedulerConfig()
	if len(cfg) == 0 {
		slog.Info("scheduler: no jobs configured")
		return
	}
	sched := scheduler.New(db, registry, txManager, email.NewSender(&email.Config{From: "kora@localhost"}))
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
