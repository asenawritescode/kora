package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/asenawritescode/kora/api"
	"github.com/asenawritescode/kora/auth"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/email"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/natsprovider"
	knet "github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/site"
	"github.com/asenawritescode/kora/storage"
)

// ConsoleAssemblyConfig contains the platform-only values needed to install
// the optional console API alongside the tenant application routes.
type ConsoleAssemblyConfig struct {
	SystemGuard           *auth.SystemGuard
	PlatformWorkAdmission interface {
		Acquire(context.Context) (func(), error)
	}
	DBType          string
	DBHost          string
	DBUser          string
	DBPassword      string
	DBPort          int
	PlatformDB      *sql.DB
	AllowOnboarding bool
	ResolveStorage  func(*site.SiteConfig) (storage.Backend, error)
}

// HTTPProcessAssemblyConfig is the typed input to Engine's HTTP and process
// worker assembly. Resources takes ownership incrementally; callers must use
// it for rollback whenever this function returns an error.
type HTTPProcessAssemblyConfig struct {
	ProcessContext          context.Context
	Resources               *ProcessResources
	Application             ApplicationConfig
	NATS                    *natsprovider.Provider
	DirectoryConsumer       *site.DirectoryConsumer
	ReloadController        *TenantReloadController
	ReloadToken             string
	Console                 *ConsoleAssemblyConfig
	PlatformRoutes          PlatformRouteConfig
	PlatformDBDriver        string
	PlatformDBDSN           string
	StartScheduler          bool
	Listener                HTTPServerConfig
	WrapHandler             func(http.Handler) http.Handler
	AsyncHookQueueSize      int
	AsyncHookMaxQueuedBytes uint64
	AsyncHookMaxEventBytes  uint64
}

type HTTPProcessAssembly struct {
	Application     *Application
	Server          *knet.Server
	AsyncHookWorker *AsyncHookWorker
}

// AssembleHTTPProcess builds the request application, installs platform
// routes/workers, creates the listener and transfers every acquired worker to
// ProcessResources before continuing to the next fallible startup stage.
func AssembleHTTPProcess(config HTTPProcessAssemblyConfig) (*HTTPProcessAssembly, error) {
	if config.ProcessContext == nil || config.Resources == nil {
		return nil, errors.New("HTTP process assembly requires process context and resource owner")
	}
	if config.Application.Common == nil || config.Application.Tenants == nil {
		return nil, errors.New("HTTP process assembly requires common config and tenant router")
	}
	if config.Console != nil && config.Console.SystemGuard != nil {
		config.Resources.SystemGuard = config.Console.SystemGuard
		if err := config.Resources.RegisterWorkerStart(func() error {
			config.Console.SystemGuard.StartCleanup()
			return nil
		}); err != nil {
			return nil, fmt.Errorf("register system session cleanup worker: %w", err)
		}
	}
	queueSize := config.AsyncHookQueueSize
	if queueSize == 0 {
		queueSize = DefaultAsyncHookQueueSize
	}
	maxQueuedBytes := config.AsyncHookMaxQueuedBytes
	if maxQueuedBytes == 0 {
		maxQueuedBytes = DefaultAsyncHookMaxQueuedBytes
	}
	maxEventBytes := config.AsyncHookMaxEventBytes
	if maxEventBytes == 0 {
		maxEventBytes = DefaultAsyncHookMaxEventBytes
	}
	asyncHooks, err := NewAsyncHookWorkerWithLimits(queueSize, maxQueuedBytes, maxEventBytes, config.Application.ScriptRunner, config.Application.Tenants, config.Application.Common.DBType, config.Application.HTTPAllowlist)
	if err != nil {
		return nil, fmt.Errorf("start async script hook worker: %w", err)
	}
	if err := config.Resources.RegisterWorkerStart(asyncHooks.Start); err != nil {
		return nil, fmt.Errorf("register async script hook worker: %w", err)
	}
	config.Resources.StopAsyncHookWorker = asyncHooks.StopAccepting
	config.Resources.CancelAsyncHookWorker = asyncHooks.Cancel
	config.Resources.AsyncHookWorkerDone = asyncHooks.Done()
	config.Application.AsyncHookSink = asyncHooks.Sink()
	if config.Application.Mailer == nil {
		config.Application.Mailer = newProcessMailer(config.Application.Common)
	}
	application, err := BuildApplication(config.Application)
	if err != nil {
		return nil, fmt.Errorf("build Engine application: %w", err)
	}
	config.Resources.SessionManager = application.SessionManager
	if application.RealtimeConnections != nil {
		config.Resources.CloseRealtimeStreams = application.RealtimeConnections.Close
	}
	if application.SessionManager != nil && config.PlatformDBDriver != "" && config.PlatformDBDSN != "" {
		admission, err := dbaccess.AdmissionFor(config.PlatformDBDriver, config.PlatformDBDSN)
		if err != nil {
			return nil, fmt.Errorf("resolve platform session-cleanup admission: %w", err)
		}
		application.SessionManager.SetCleanupAdmission(admission)
	}
	if err := config.Resources.RegisterWorkerStart(func() error {
		application.SessionManager.StartCleanup()
		return nil
	}); err != nil {
		return nil, fmt.Errorf("register process session cleanup worker: %w", err)
	}
	router := application.Router
	if config.ReloadController == nil {
		return nil, errors.New("tenant reload controller is required")
	}
	if err := RegisterTenantReloadRoute(router, config.ReloadToken, config.ReloadController); err != nil {
		return nil, fmt.Errorf("register tenant reload route: %w", err)
	}
	if config.DirectoryConsumer != nil {
		consumer := config.DirectoryConsumer
		if consumer.Registry == nil || consumer.ID == "" || consumer.Apply == nil {
			return nil, errors.New("site directory feed requires a configured SQL consumer")
		}
		if err := config.Resources.RegisterWorkerStart(func() error {
			feed, err := StartDirectoryFeed(config.ProcessContext, consumer, config.NATS)
			if err != nil {
				return fmt.Errorf("start site directory feed: %w", err)
			}
			config.Resources.DirectoryFeed = feed
			return nil
		}); err != nil {
			return nil, fmt.Errorf("register site directory feed: %w", err)
		}
	}
	if config.Console != nil && config.Console.SystemGuard != nil {
		console := config.Console
		handler := api.NewConsoleHandler(console.SystemGuard, config.Application.Tenants,
			console.DBType, console.DBHost, console.DBUser, console.DBPassword, console.DBPort,
			console.PlatformDB, console.AllowOnboarding)
		handler.PlatformWorkAdmission = console.PlatformWorkAdmission
		handler.TenantSiteAdmission = tenantSiteProvisioningAdmission(config.PlatformDBDriver, config.PlatformDBDSN)
		handler.ResolveStorage = console.ResolveStorage
		handler.PrepareRuntime = config.ReloadController.Coordinator.Install
		handler.DiscardRuntime = discardTenantCandidate
		handler.ReloadRuntime = func(ctx context.Context, name string) error {
			_, err := config.ReloadController.Reload(ctx, name)
			return err
		}
		handler.Start()
		config.Resources.CloseOnboardingJobs = handler.CloseOnboardingJobs
		if err := RegisterConsoleRoutes(router, handler); err != nil {
			return nil, fmt.Errorf("register Engine console routes: %w", err)
		}
	}
	platformRoutes := config.PlatformRoutes
	platformRoutes.Router = router
	if platformRoutes.ManagementCallerID != "" {
		stateDirectory, ok := platformRoutes.Directory.(managementStateDirectory)
		if !ok {
			return nil, errors.New("management state operations require the Engine SQL tenant directory")
		}
		managementRuntime, err := assembleManagementOperationRuntime(
			config.ProcessContext, platformRoutes.ManagementToken, platformRoutes.ManagementCallerID,
			platformRoutes.PlatformDB, db.Resolve(config.Application.Common.DBType),
			platformRoutes.PlatformWorkAdmission, stateDirectory, config.Resources.TenantCoordinator,
			config.Application.Tenants, platformRoutes.ManagementPackageResolver,
			platformRoutes.ManagementCheckpointVerifier, config.Application.Common.DBType, config.Application.Common,
			platformRoutes.DeploymentID, platformRoutes.ManagementDatabaseAllocationRef,
		)
		if err != nil {
			return nil, err
		}
		platformRoutes.ManagementOperations = &managementRuntime.routes
		if err := managementRuntime.start(config.ProcessContext, config.Resources); err != nil {
			return nil, err
		}
	}
	if err := RegisterPlatformRoutes(platformRoutes); err != nil {
		return nil, fmt.Errorf("register Engine platform routes: %w", err)
	}
	if config.StartScheduler {
		scheduler, err := PrepareScheduler(SchedulerOptions{
			Database: config.Application.Database, Registry: application.Registry,
			Transactions: application.Transactions, Mailer: config.Application.Mailer,
		})
		if err != nil {
			return nil, fmt.Errorf("start process scheduler: %w", err)
		}
		config.Resources.Scheduler = scheduler
		if err := config.Resources.RegisterWorkerStart(func() error {
			scheduler.StartContext(config.ProcessContext)
			return nil
		}); err != nil {
			return nil, fmt.Errorf("register process scheduler: %w", err)
		}
	}
	var handler http.Handler = router
	if config.WrapHandler != nil {
		handler = config.WrapHandler(handler)
	}
	config.Listener.Handler = handler
	server, err := BuildHTTPServer(config.Listener)
	if err != nil {
		return nil, fmt.Errorf("assemble Engine HTTP listener: %w", err)
	}
	platformDB := config.PlatformRoutes.PlatformDB
	if platformDB == nil {
		return nil, errors.New("Engine platform database is required for deletion recovery")
	}
	if err := config.Resources.RegisterWorkerStart(func() error {
		if config.PlatformDBDriver == "" || config.PlatformDBDSN == "" {
			return errors.New("platform database endpoint identity is required for deletion recovery admission")
		}
		admission, err := dbaccess.AdmissionFor(config.PlatformDBDriver, config.PlatformDBDSN)
		if err != nil {
			return fmt.Errorf("resolve platform database work admission: %w", err)
		}
		deletionWorker, err := startPendingDeletionWorker(config.ProcessContext, platformDB, config.Application.Common.DBType, config.Application.Common, site.RetryPendingSiteDeletions, pendingDeletionRetryInterval, admission)
		if err != nil {
			return fmt.Errorf("start pending tenant deletion recovery worker: %w", err)
		}
		config.Resources.DeletionWorker = deletionWorker
		return nil
	}); err != nil {
		return nil, fmt.Errorf("register pending tenant deletion recovery worker: %w", err)
	}
	return &HTTPProcessAssembly{Application: application, Server: server, AsyncHookWorker: asyncHooks}, nil
}

func discardTenantCandidate(runtime *knet.LoadedSite) {
	if runtime == nil {
		return
	}
	runtime.StopWorkers()
	runtime.CloseSessionManager()
	services := runtime.RuntimeServiceSnapshot()
	StopTenantServiceBundle(services)
	if runtime.AnalyticsWorker != nil {
		runtime.AnalyticsWorker.Stop()
	}
}

func newProcessMailer(common *site.CommonConfig) *email.Sender {
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
		Host: common.SMTPHost, Port: common.SMTPPort, Username: common.SMTPUsername,
		Password: common.SMTPPassword, From: from, TLSMode: common.SMTPTLSMode,
	})
}
