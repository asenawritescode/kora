package engine

import (
	"database/sql"
	"errors"
	"time"

	"github.com/asenawritescode/kora/api"
	"github.com/asenawritescode/kora/auth"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/email"
	"github.com/asenawritescode/kora/kernel"
	knet "github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/script"
	"github.com/asenawritescode/kora/site"
	"github.com/gin-gonic/gin"
)

// ApplicationConfig is the concrete Engine-owned dependency set required to
// build the tenant router, request middleware and application route groups.
type ApplicationConfig struct {
	Common              *site.CommonConfig
	Tenants             *knet.SiteRouter
	Database            *sql.DB
	Mailer              *email.Sender
	ScriptRunner        script.Runner
	HTTPAllowlist       []string
	CORSAllowedOrigins  []string
	AsyncHookSink       orm.AsyncHookSink
	KernelCommands      *kernel.CommandRegistry
	BinaryVersion       string
	SessionCacheBudget  *auth.SessionCacheBudget
	PublicAppBaseURL    string
	CloudIdentityURL    string
	CloudIdentityToken  string
	RealtimeConnections *api.RealtimeConnectionOwner
}

// Application is the coherent HTTP composition returned to the process owner.
// The session manager must be registered for startup rollback and shutdown.
type Application struct {
	Router              *gin.Engine
	Registry            *doctype.Registry
	Transactions        *orm.TxManager
	SessionManager      *auth.SessionManager
	AnalyticsCache      *api.AnalyticsQueryCache
	RealtimeConnections *api.RealtimeConnectionOwner
}

// BuildApplication constructs the request router and installs its application
// routes and middleware. Process and platform-only routes are added by Engine
// to the same router before listener assembly.
func BuildApplication(config ApplicationConfig) (*Application, error) {
	if config.Common == nil {
		return nil, errors.New("Engine common configuration is required")
	}
	if config.Tenants == nil {
		return nil, errors.New("Engine tenant router is required")
	}
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.RedirectTrailingSlash = false
	if err := router.SetTrustedProxies(config.Common.TrustedProxies); err != nil {
		return nil, err
	}
	router.Use(gin.Recovery())
	router.Use(knet.RequestIDMiddleware())
	router.Use(knet.SecurityHeadersMiddleware(config.Common.TLSMode != "" && config.Common.TLSMode != "off"))
	router.Use(knet.CORSMiddleware(config.CORSAllowedOrigins))
	router.Use(config.Tenants.Middleware())
	// Bound the complete tenant request, including session authentication's DB
	// lookup. API route groups retain an idempotent compatibility wrapper.
	router.Use(api.WorkAdmissionMiddleware(nil))
	router.Use(knet.NewRateLimiter(float64(config.Common.RateLimitRPS), config.Common.RateLimitBurst).Middleware())

	registry := doctype.NewRegistry()
	if tenants := config.Tenants.AllSites(); len(tenants) > 0 && tenants[0] != nil && tenants[0].Registry != nil {
		registry = tenants[0].Registry
	}
	transactions := &orm.TxManager{DB: config.Database, Registry: registry, Dialect: kdb.Resolve(config.Common.DBType)}
	analyticsCache := api.NewAnalyticsQueryCache()
	realtimeConnections := config.RealtimeConnections
	if realtimeConnections == nil {
		realtimeConnections = api.NewRealtimeConnectionOwner()
	}
	sessionManager, err := RegisterApplicationRoutes(ApplicationRouteConfig{
		Router: router, Tenants: config.Tenants, Database: config.Database, Registry: registry,
		Transactions: transactions, Mailer: config.Mailer, ScriptRunner: config.ScriptRunner,
		HTTPAllowlist: config.HTTPAllowlist, AsyncHookSink: config.AsyncHookSink, KernelCommands: config.KernelCommands,
		SessionLifetime: time.Duration(config.Common.SessionLifetimeHours) * time.Hour,
		CSRFSecure:      config.Common.CSRFSecure, AdminRole: config.Common.AdminRole,
		AppName: config.Common.AppName, PrimaryColor: config.Common.PrimaryColor,
		APIDefaultLimit: config.Common.APIDefaultLimit, APIMaxLimit: config.Common.APIMaxLimit,
		// Tenant middleware pins services into each request snapshot. Production
		// route handlers use that snapshot, not the compatibility registry.
		BinaryVersion: config.BinaryVersion, AnalyticsCache: analyticsCache, RealtimeConnections: realtimeConnections,
		PublicAppBaseURL: config.PublicAppBaseURL,
		CloudIdentityURL: config.CloudIdentityURL, CloudIdentityToken: config.CloudIdentityToken,
	})
	if err != nil {
		return nil, err
	}
	sessionManager.SetCacheBudget(config.SessionCacheBudget)
	return &Application{Router: router, Registry: registry, Transactions: transactions, SessionManager: sessionManager, AnalyticsCache: analyticsCache, RealtimeConnections: realtimeConnections}, nil
}
