package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"time"

	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/auth"
	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/internal/application/process"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/natsprovider"
	"github.com/asenawritescode/kora/script"
	"github.com/asenawritescode/kora/site"
	"github.com/asenawritescode/kora/storage"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// Config is the fully validated, transport-neutral process configuration.
// Environment parsing belongs to the executable; resource acquisition and
// runtime composition belong to Engine.
type Config struct {
	Telemetry               TelemetryConfig
	DBDriver                string
	DBDSN                   string
	DBDialect               string
	RawCellID               string
	Common                  *site.CommonConfig
	BinaryVersion           string
	TenantConcurrency       uint16
	MaxResidentTenants      uint32
	TenantActivationWaiters uint16
	TenantActivationWait    time.Duration
	RetirementLimit         uint16
	ResolveStorage          func(*site.SiteConfig) (storage.Backend, error)
	CellURLs                map[string]string
	DirectoryConsumerID     string
	NATSConfig              *natsprovider.Config
	ScriptsEnabled          bool
	KernelCommands          *kernel.CommandRegistry
	Workflow                *WorkflowTenantConfig
	// RequestAuthorizer and WorkflowApprovals are supplied by registered
	// application-domain integrations; generic Engine composition has no
	// built-in business policy or transition mapping.
	RequestAuthorizer               RequestAuthorizerFactory
	WorkflowApprovals               map[string]process.WorkflowApproval
	SchedulerHandlerFactory         ScheduledJobHandlerFactory
	HTTPAllowlist                   []string
	CORSAllowedOrigins              []string
	CloudRelay                      *analytics.CloudRelayConfig
	Analytics                       *analytics.Config
	OutboxEnabled                   bool
	SystemGuard                     *auth.SystemGuard
	PlatformDBUser                  string
	PlatformDBPassword              string
	AllowConsoleOnboarding          bool
	ManagementToken                 string
	ReloadToken                     string
	ManagementCallerID              string
	PublicAppBaseURL                string
	CloudIdentityURL                string
	CloudIdentityToken              string
	DeploymentID                    string
	ManagementDatabaseAllocationRef string
	ManagementPackageResolver       ManagementPackageArtifactResolver
	ManagementCheckpointVerifier    CheckpointVerifier
	Listener                        HTTPServerConfig
	OnReady                         func(EngineMetrics)
	DirectoryLagMetricsEnabled      bool
	AsyncHookQueueSize              int
	AsyncHookMaxQueuedBytes         uint64
	AsyncHookMaxEventBytes          uint64
	ScriptBudget                    script.BudgetConfig
	RealtimeMaxStreams              int
	SessionCacheBudgetBytes         uint64
	UploadMaxActive                 int
	UploadActiveBudgetBytes         uint64
	AnalyticsQueueBudgetBytes       uint64
	AnalyticsMaxSubscribers         int
}

// EngineMetrics is a low-cardinality startup snapshot provided to the host
// application after the HTTP listener begins accepting requests.
type EngineMetrics struct {
	DiscoveredTenants int
	LoadedTenants     int
	OpenConnections   int
	InUseConnections  int
	IdleConnections   int
	PoolWaitCount     int
	PoolWaitDuration  time.Duration
	HeapAllocBytes    uint64
	HeapSysBytes      uint64
	StartupDuration   time.Duration
}

type engineAssemblyDependencies struct {
	assembleStartup func(context.Context, ProcessStartupConfig, *ProcessResources) (*ProcessStartupAssembly, error)
	sharedRuntime   func(context.Context, SharedRuntimeConfig, *ProcessResources) (*SharedRuntime, error)
	assembleTenants func(TenantRuntimeAssemblyConfig) (*TenantRuntimeAssembly, error)
	assembleHTTP    func(HTTPProcessAssemblyConfig) (*HTTPProcessAssembly, error)
}

// New assembles the complete Engine runtime and immediately assumes ownership
// of every acquired resource. On failure it unwinds the partial assembly using
// the same bounded shutdown path used after serving begins.
func New(ctx context.Context, config Config) (_ *Engine, err error) {
	return newEngine(ctx, config, engineAssemblyDependencies{
		assembleStartup: AssembleProcessStartup,
		sharedRuntime:   InitializeSharedRuntime,
		assembleTenants: AssembleTenantRuntime,
		assembleHTTP:    AssembleHTTPProcess,
	})
}

func newEngine(ctx context.Context, config Config, deps engineAssemblyDependencies) (_ *Engine, err error) {
	if ctx == nil {
		return nil, errors.New("Engine context is required")
	}
	if config.Common == nil || config.ResolveStorage == nil {
		return nil, errors.New("Engine common config and storage resolver are required")
	}
	if err := auth.ValidatePublicAppBaseURL(config.PublicAppBaseURL); err != nil {
		return nil, fmt.Errorf("configure public application base URL: %w", err)
	}
	if deps.assembleStartup == nil || deps.sharedRuntime == nil || deps.assembleTenants == nil || deps.assembleHTTP == nil {
		return nil, errors.New("Engine assembly dependencies are incomplete")
	}
	if config.RetirementLimit == 0 {
		config.RetirementLimit = 32
	}
	if config.MaxResidentTenants == 0 {
		config.MaxResidentTenants = DefaultMaxResidentTenants
	}
	if config.TenantActivationWait <= 0 {
		config.TenantActivationWait = 1500 * time.Millisecond
	}
	processCtx, cancel := context.WithCancel(ctx)
	resources := &ProcessResources{CancelProcess: cancel}
	analyticsQueueBudgetBytes := config.AnalyticsQueueBudgetBytes
	if analyticsQueueBudgetBytes == 0 {
		analyticsQueueBudgetBytes = analytics.DefaultAnalyticsQueueAdmissionBytes
	}
	analyticsMaxSubscribers := config.AnalyticsMaxSubscribers
	if analyticsMaxSubscribers == 0 {
		analyticsMaxSubscribers = analytics.DefaultAnalyticsProcessMaxListeners
	}
	analyticsQueueAdmission, err := analytics.NewAnalyticsQueueAdmissionWithLimits(analyticsQueueBudgetBytes, analyticsMaxSubscribers)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("configure process analytics subscriber queue budget: %w", err)
	}
	resources.AnalyticsQueueAdmission = analyticsQueueAdmission
	sessionCacheBudgetBytes := config.SessionCacheBudgetBytes
	if sessionCacheBudgetBytes == 0 {
		sessionCacheBudgetBytes = auth.DefaultSessionCacheBudgetBytes
	}
	sessionCacheBudget, err := auth.NewSessionCacheBudget(sessionCacheBudgetBytes)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("configure shared session cache budget: %w", err)
	}
	resources.SessionCacheBudget = sessionCacheBudget
	uploadMaxActive := config.UploadMaxActive
	if uploadMaxActive == 0 {
		uploadMaxActive = DefaultUploadMaxActive
	}
	uploadActiveBytes := config.UploadActiveBudgetBytes
	if uploadActiveBytes == 0 {
		uploadActiveBytes = DefaultUploadActiveBytes
	}
	uploadAdmission, err := NewUploadAdmission(uploadMaxActive, uploadActiveBytes)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("configure process upload admission: %w", err)
	}
	resources.UploadAdmission = uploadAdmission
	realtimeMaxStreams := config.RealtimeMaxStreams
	if realtimeMaxStreams == 0 {
		realtimeMaxStreams = DefaultRealtimeMaxStreams
	}
	realtimeStreams, err := NewRealtimeStreamAdmission(realtimeMaxStreams)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("configure realtime stream admission: %w", err)
	}
	resources.RealtimeStreams = realtimeStreams
	owned := true
	defer func() {
		if !owned {
			return
		}
		rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer rollbackCancel()
		if rollbackErr := resources.Shutdown(rollbackCtx); rollbackErr != nil {
			err = errors.Join(err, fmt.Errorf("roll back Engine initialization: %w", rollbackErr))
		}
	}()

	startup, err := deps.assembleStartup(processCtx, ProcessStartupConfig{
		Telemetry: config.Telemetry, DBDriver: config.DBDriver, DBDSN: config.DBDSN,
		DBDialect: config.DBDialect, RawCellID: config.RawCellID, Common: config.Common,
		BinaryVersion: config.BinaryVersion, TenantConcurrency: config.TenantConcurrency,
		MaxResidentTenants: config.MaxResidentTenants,
		ResolveStorage:     config.ResolveStorage, CellURLs: config.CellURLs,
		DirectoryConsumerID:        config.DirectoryConsumerID,
		DirectoryLagMetricsEnabled: config.DirectoryLagMetricsEnabled,
	}, resources)
	if err != nil {
		return nil, err
	}
	if startup == nil || startup.Span == nil {
		return nil, errors.New("Engine process startup returned an incomplete assembly")
	}
	if len(config.Listener.TLS.Domains) == 0 {
		config.Listener.TLS.Domains = append([]string(nil), startup.Tenants.Domains...)
	}
	spanEnded := false
	defer func() {
		if owned && !spanEnded {
			startup.Span.End()
		}
	}()
	shared, err := deps.sharedRuntime(processCtx, SharedRuntimeConfig{
		ScriptsEnabled: config.ScriptsEnabled, NATSConfig: config.NATSConfig,
		BootstrapSiteDirectory: startup.Platform.Directory != nil,
	}, resources)
	if err != nil {
		return nil, err
	}
	mailer := newProcessMailer(config.Common)
	if shared.ScriptRunner != nil {
		budget := config.ScriptBudget
		if budget == (script.BudgetConfig{}) {
			budget = script.DefaultBudgetConfig()
		}
		budgeted, budgetErr := script.NewBudgetedRunner(shared.ScriptRunner, budget)
		if budgetErr != nil {
			return nil, fmt.Errorf("configure tenant script admission: %w", budgetErr)
		}
		shared.ScriptRunner = budgeted
		resources.ScriptRunner = budgeted
	}
	var relay *analytics.CloudRelayConfig
	if config.CloudRelay != nil {
		copy := *config.CloudRelay
		relay = &copy
	}
	analyticsConfig := config.Analytics
	if analyticsConfig == nil {
		analyticsConfig = &analytics.Config{}
	}
	directoryWorkAdmission, err := dbaccess.AdmissionFor(config.DBDriver, config.DBDSN)
	if err != nil {
		return nil, fmt.Errorf("configure site-directory work admission: %w", err)
	}
	var tenantIngress TenantIngressDirectory
	if startup.Ingress != nil {
		tenantIngress = startup.Ingress
	}
	tenantAssembly, err := deps.assembleTenants(TenantRuntimeAssemblyConfig{
		Runtimes: startup.Tenants.Runtimes, Directory: startup.Platform.Directory,
		DirectoryDescriptors: startup.Platform.Descriptors,
		DirectoryRevision:    startup.Platform.StartCursor, DirectoryConsumerID: startup.DirectoryConsumerID,
		CellID: startup.Platform.CellID, DirectoryWorkAdmission: directoryWorkAdmission, RetirementLimit: config.RetirementLimit,
		TenantActivationWorkAdmission: func(ctx context.Context, tenantConfig *site.SiteConfig) (func(), error) {
			admission, admissionErr := site.ConnectionAdmission(tenantConfig)
			if admissionErr != nil {
				return nil, admissionErr
			}
			return admission.Acquire(ctx)
		},
		MaxResidentTenants:   config.MaxResidentTenants,
		MaxActivationWaiters: config.TenantActivationWaiters, ActivationWait: config.TenantActivationWait,
		Common: config.Common, BinaryVersion: config.BinaryVersion, Ingress: tenantIngress,
		Resources: resources,
		Services: TenantServiceConfig{
			ProcessContext: processCtx, Analytics: analyticsConfig, CloudRelay: relay, AnalyticsQueueAdmission: resources.AnalyticsQueueAdmission,
			SchedulerHandlerFactory: config.SchedulerHandlerFactory, Mailer: mailer,
			NATS: shared.NATSProvider, ScriptRunner: shared.ScriptRunner, HTTPAllowlist: config.HTTPAllowlist, RealtimeAdmission: resources.RealtimeStreams,
			SessionCacheBudget:  resources.SessionCacheBudget,
			UploadAdmission:     resources.UploadAdmission,
			OutboxEnabled:       config.OutboxEnabled || shared.NATSProvider != nil,
			Workflow:            config.Workflow,
			RequestAuthorizer:   config.RequestAuthorizer,
			WorkflowApprovals:   config.WorkflowApprovals,
			ResolveStorage:      config.ResolveStorage,
			RegisterWorkerStart: resources.RegisterWorkerStart,
		},
	})
	if err != nil {
		return nil, err
	}
	guard := config.SystemGuard
	if guard == nil {
		guard, err = auth.LoadSystemGuardPaused("system_credentials.yaml")
		if err != nil {
			guard = auth.LoadSystemGuardFromEnvPaused()
		}
	}
	httpAssembly, err := deps.assembleHTTP(HTTPProcessAssemblyConfig{
		ProcessContext: processCtx, Resources: resources,
		ReloadToken: config.ReloadToken,
		Application: ApplicationConfig{
			Common: config.Common, Tenants: tenantAssembly.Tenants,
			Database: startup.Platform.DB, Mailer: mailer, ScriptRunner: shared.ScriptRunner,
			SessionCacheBudget: resources.SessionCacheBudget,
			HTTPAllowlist:      config.HTTPAllowlist, CORSAllowedOrigins: config.CORSAllowedOrigins, KernelCommands: config.KernelCommands,
			BinaryVersion:    config.BinaryVersion,
			PublicAppBaseURL: config.PublicAppBaseURL,
			CloudIdentityURL: config.CloudIdentityURL, CloudIdentityToken: config.CloudIdentityToken,
		},
		NATS: shared.NATSProvider, DirectoryConsumer: tenantAssembly.DirectoryConsumer,
		ReloadController: tenantAssembly.ReloadController,
		Console: &ConsoleAssemblyConfig{
			SystemGuard: guard, PlatformWorkAdmission: directoryWorkAdmission,
			DBType: config.Common.DBType, DBHost: config.Common.DBHost,
			DBUser: config.PlatformDBUser, DBPassword: config.PlatformDBPassword, DBPort: config.Common.DBPort,
			PlatformDB: startup.Platform.DB, AllowOnboarding: config.AllowConsoleOnboarding,
			ResolveStorage: config.ResolveStorage,
		},
		PlatformRoutes: PlatformRouteConfig{
			Directory: startup.Platform.Directory, ManagementToken: config.ManagementToken,
			ManagementCallerID:              config.ManagementCallerID,
			PublicAppBaseURL:                config.PublicAppBaseURL,
			ManagementPackageResolver:       config.ManagementPackageResolver,
			ManagementCheckpointVerifier:    config.ManagementCheckpointVerifier,
			DeploymentID:                    config.DeploymentID,
			ManagementDatabaseAllocationRef: config.ManagementDatabaseAllocationRef,
			PlatformDB:                      startup.Platform.DB, PlatformWorkAdmission: directoryWorkAdmission,
			TenantConfigRevision: func(ctx context.Context, tenantID string) (uint64, error) {
				if err := tenantAssembly.Coordinator.ActivateFromDirectory(ctx, tenantID); err != nil {
					return 0, fmt.Errorf("activate tenant runtime for config revision read: %w", err)
				}
				handle, err := tenantAssembly.Tenants.AcquireByID(ctx, tenantID)
				if err != nil {
					return 0, fmt.Errorf("pin tenant runtime for config revision read: %w", err)
				}
				defer handle.Release()
				runtime := handle.Snapshot()
				if runtime == nil || runtime.DB == nil {
					return 0, fmt.Errorf("tenant runtime has no database")
				}
				return configstore.CurrentConfigRevision(ctx, runtime.DB, db.Resolve(runtime.DBType))
			},
			BinaryVersion: config.BinaryVersion,
		},
		PlatformDBDriver: config.DBDriver, PlatformDBDSN: config.DBDSN,
		StartScheduler:          false,
		AsyncHookQueueSize:      config.AsyncHookQueueSize,
		AsyncHookMaxQueuedBytes: config.AsyncHookMaxQueuedBytes,
		AsyncHookMaxEventBytes:  config.AsyncHookMaxEventBytes,
		Listener:                config.Listener,
		WrapHandler: func(handler http.Handler) http.Handler {
			if startup.Ingress != nil {
				return startup.Ingress.Wrap(handler)
			}
			return handler
		},
	})
	if err != nil {
		return nil, err
	}
	if config.Telemetry.MetricsEnabled {
		var budgetedScripts *script.BudgetedRunner
		budgetedScripts, _ = shared.ScriptRunner.(*script.BudgetedRunner)
		resourceMetrics, metricsErr := RegisterResourceMetrics(tenantAssembly.Tenants, httpAssembly.AsyncHookWorker, budgetedScripts, ResourceMetricSources{PlatformDB: startup.Platform.DB, RealtimeStreams: resources.RealtimeStreams, SessionCache: resources.SessionCacheBudget, Uploads: resources.UploadAdmission, AnalyticsQueueAdmission: resources.AnalyticsQueueAdmission, NATSRealtime: shared.NATSProvider, TenantActivation: tenantAssembly.Coordinator, DurableWorkScan: tenantAssembly.DurableWorkScanner, DeletionWorker: resources.DeletionWorker, AnalyticsQueryCache: httpAssembly.Application.AnalyticsCache})
		if metricsErr != nil {
			return nil, fmt.Errorf("register Engine resource metrics: %w", metricsErr)
		}
		resources.ResourceMetrics = resourceMetrics
	}
	ready := func() {
		metrics := collectEngineMetrics(startup, resources)
		startup.Span.SetAttributes(
			attribute.Int("kora.site.discovered", metrics.DiscoveredTenants),
			attribute.Int("kora.site.loaded", metrics.LoadedTenants),
			attribute.Int64("kora.engine.startup.duration_ms", metrics.StartupDuration.Milliseconds()),
			attribute.Int("kora.db.open_connections", metrics.OpenConnections),
			attribute.Int("kora.db.in_use_connections", metrics.InUseConnections),
			attribute.Int("kora.db.idle_connections", metrics.IdleConnections),
			attribute.Int("kora.db.pool_wait_count", metrics.PoolWaitCount),
			attribute.Int64("kora.db.pool_wait_duration_ms", metrics.PoolWaitDuration.Milliseconds()),
			attribute.Int64("kora.runtime.heap_alloc_bytes", int64(metrics.HeapAllocBytes)),
			attribute.Int64("kora.runtime.heap_sys_bytes", int64(metrics.HeapSysBytes)),
		)
		if config.OnReady != nil {
			config.OnReady(metrics)
		}
		if resources.DeletionWorker != nil {
			resources.DeletionWorker.Start()
		}
	}
	owned = false
	engine := &Engine{server: httpAssembly.Server, onReady: ready, resources: resources}
	engine.onStopped = func(runErr error) {
		if runErr != nil {
			startup.Span.RecordError(runErr)
			startup.Span.SetStatus(codes.Error, "Engine stopped with an error")
		}
		startup.Span.End()
	}
	return engine, nil
}

func collectEngineMetrics(startup *ProcessStartupAssembly, resources *ProcessResources) EngineMetrics {
	var metrics EngineMetrics
	if startup == nil {
		return metrics
	}
	metrics.DiscoveredTenants = startup.DiscoveredTenants
	metrics.LoadedTenants = len(startup.Tenants.Runtimes)
	metrics.StartupDuration = time.Since(startup.Started)
	for _, runtime := range startup.Tenants.Runtimes {
		if runtime == nil || runtime.DB == nil {
			continue
		}
		stats := runtime.DB.Stats()
		metrics.OpenConnections += stats.OpenConnections
		metrics.InUseConnections += stats.InUse
		metrics.IdleConnections += stats.Idle
		metrics.PoolWaitCount += int(stats.WaitCount)
		metrics.PoolWaitDuration += stats.WaitDuration
	}
	if resources != nil && resources.PlatformDB != nil {
		stats := resources.PlatformDB.Stats()
		metrics.OpenConnections += stats.OpenConnections
		metrics.InUseConnections += stats.InUse
		metrics.IdleConnections += stats.Idle
		metrics.PoolWaitCount += int(stats.WaitCount)
		metrics.PoolWaitDuration += stats.WaitDuration
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	metrics.HeapAllocBytes = memory.HeapAlloc
	metrics.HeapSysBytes = memory.HeapSys
	return metrics
}
