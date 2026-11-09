package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/asenawritescode/kora/internal/api"
	"github.com/asenawritescode/kora/internal/auth"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/email"
	"github.com/asenawritescode/kora/internal/extension/tokencache"
	"github.com/asenawritescode/kora/internal/kernel"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/script"
	"github.com/gin-gonic/gin"
)

// ApplicationRouteConfig is the concrete dependency set used to compose
// Engine-owned tenant and public APIs. These references are assembled once at
// startup and route handlers resolve tenant-owned state through SiteRouter.
type ApplicationRouteConfig struct {
	Router              *gin.Engine
	Tenants             *knet.SiteRouter
	Database            *sql.DB
	Registry            *doctype.Registry
	Transactions        *orm.TxManager
	Mailer              *email.Sender
	ScriptRunner        script.Runner
	HTTPAllowlist       []string
	AsyncHookSink       orm.AsyncHookSink
	KernelCommands      *kernel.CommandRegistry
	SessionLifetime     time.Duration
	CSRFSecure          bool
	AdminRole           string
	AppName             string
	PrimaryColor        string
	APIDefaultLimit     int
	APIMaxLimit         int
	BinaryVersion       string
	PublicAppBaseURL    string
	CloudIdentityURL    string
	CloudIdentityToken  string
	AnalyticsCache      *api.AnalyticsQueryCache
	RealtimeConnections *api.RealtimeConnectionOwner

	// ExtensionTokenCacheTTL caches extension token verification per process;
	// zero disables it. It bounds cross-replica revocation lag.
	ExtensionTokenCacheTTL time.Duration
}

// RegisterApplicationRoutes installs the canonical, compatibility, request,
// public, auth and workspace route sets. It returns the process-owned session
// manager so the caller can register it for rollback and shutdown.
func RegisterApplicationRoutes(config ApplicationRouteConfig) (*auth.SessionManager, error) {
	if config.Router == nil {
		return nil, errors.New("Engine HTTP router is required")
	}
	if config.Tenants == nil || config.Registry == nil || config.Transactions == nil {
		return nil, errors.New("tenant router, registry and transaction manager are required")
	}

	auth.SessionLifetime = config.SessionLifetime
	doctype.SetAdminRole(config.AdminRole)
	api.AppBranding = api.Branding{AppName: config.AppName, PrimaryColor: config.PrimaryColor}
	api.SetAPILimits(config.APIDefaultLimit, config.APIMaxLimit)
	api.BinaryVersion = config.BinaryVersion
	auth.SetCSRFSecure(config.CSRFSecure)

	sessionManager := auth.NewOwnedSessionManagerPaused(config.Database)
	if config.Transactions != nil {
		sessionManager.SetDialect(config.Transactions.Dialect)
	}
	if err := auth.RegisterAuthRoutesWithPublicAppBaseURL(config.Router, sessionManager, config.Database, config.Mailer, config.PublicAppBaseURL); err != nil {
		_ = sessionManager.Close()
		return nil, fmt.Errorf("register authentication routes: %w", err)
	}
	if err := auth.RegisterCloudHandoffRoute(config.Router, sessionManager, config.Database, auth.CloudHandoffConfig{RedeemURL: config.CloudIdentityURL, ServiceToken: config.CloudIdentityToken}); err != nil {
		_ = sessionManager.Close()
		return nil, fmt.Errorf("register Cloud handoff route: %w", err)
	}
	siteGuard := auth.NewSiteGuardWithSessionManager(sessionManager)
	if config.ExtensionTokenCacheTTL > 0 {
		tokenCache := tokencache.New(tokencache.Config{TTL: config.ExtensionTokenCacheTTL})
		siteGuard.ExtensionTokens = func(database *sql.DB, dialect kdb.QueryDialect) auth.ExtensionTokenVerifier {
			return tokenCache.Wrap(auth.OpenSQLExtensionTokens(database, dialect))
		}
		siteGuard.ExtensionTokensChanged = tokenCache.InvalidateSite
	}

	apiGroup := config.Router.Group("/api/v1")
	apiGroup.Use(siteGuard.Middleware(false))
	apiGroup.Use(knet.CompressMiddleware())
	legacyAPIGroup := config.Router.Group("/api")
	legacyAPIGroup.Use(siteGuard.Middleware(false))
	legacyAPIGroup.Use(knet.CompressMiddleware())
	requestGroup := config.Router.Group("/api/v2")
	requestGroup.Use(siteGuard.Middleware(false))
	requestGroup.Use(knet.CompressMiddleware())
	requestGroup.Use(api.WorkAdmissionMiddleware(nil))
	publicV1Group := config.Router.Group("/api/v1")
	publicV1Group.Use(knet.CompressMiddleware())
	publicLegacyGroup := config.Router.Group("/api")
	publicLegacyGroup.Use(knet.CompressMiddleware())

	api.RegisterRoutesOnGroup(apiGroup, config.Registry, config.Transactions, apiRouteOptions(config))
	api.RegisterRoutesOnGroup(legacyAPIGroup, config.Registry, config.Transactions, apiRouteOptions(config))
	api.RegisterRequestRoutes(requestGroup, nil)
	api.RegisterWorkflowRoutes(requestGroup, nil)
	catalogHandler := api.NewHandler(config.Registry, config.Transactions)
	requestGroup.GET("/modules/catalog", catalogHandler.HandleApplicationCatalog)
	requestGroup.POST("/modules/operations/:operation/execute", catalogHandler.HandleExecutePublishedOperation)
	requestGroup.POST("/module-executions/lookup", catalogHandler.HandleLookupPublishedOperation)
	requestGroup.GET("/module-executions/:execution", catalogHandler.HandleGetPublishedOperationReceipt)
	requestGroup.GET("/offline/capabilities", catalogHandler.HandleOfflineCapabilitiesV2)
	requestGroup.POST("/offline/intents", catalogHandler.HandleOfflineIntentsV2)
	requestGroup.POST("/offline/bootstrap", catalogHandler.HandleOfflineBootstrapV2)
	requestGroup.POST("/offline/pull", catalogHandler.HandleOfflinePullV2)
	api.RegisterPublicRoutesOnGroupWithRuntimeServices(publicV1Group, config.Registry, config.Transactions, nil, nil)
	api.RegisterPublicRoutesOnGroupWithRuntimeServices(publicLegacyGroup, config.Registry, config.Transactions, nil, nil)

	// The Engine is API-only. Path-based tenant access (/s/:site/api/...) is the only non-API route
	// family, and the standalone UI is served by its own deployment.
	knet.RegisterPathSiteRoutes(config.Router, config.Tenants)
	return sessionManager, nil
}

// RegisterConsoleRoutes installs the platform console API after its handler
// has been assembled by the startup composition root.
func RegisterConsoleRoutes(router *gin.Engine, handler *api.ConsoleHandler) error {
	if router == nil {
		return errors.New("Engine HTTP router is required")
	}
	if handler == nil {
		return errors.New("console handler is required")
	}
	router.POST("/api/console/login", handler.HandleLogin)
	router.POST("/api/console/change-password", handler.HandleChangePassword)
	router.GET("/api/console/sites", handler.RequireConsoleAuth, handler.HandleListSites)
	router.PUT("/api/console/sites/:name", handler.RequireConsoleAuth, handler.RequirePlatformWork, handler.HandleUpdateSite)
	router.DELETE("/api/console/sites/:name", handler.RequireConsoleAuth, handler.RequirePlatformWork, handler.HandleDeleteSite)
	return nil
}

// apiRouteOptions maps the Engine composition config to the API route options.
func apiRouteOptions(config ApplicationRouteConfig) api.RouteOptions {
	return api.RouteOptions{
		ScriptRunner:        config.ScriptRunner,
		HTTPAllowlist:       config.HTTPAllowlist,
		AsyncHookSink:       config.AsyncHookSink,
		KernelCommands:      config.KernelCommands,
		AnalyticsCache:      config.AnalyticsCache,
		RealtimeConnections: config.RealtimeConnections,
	}
}
