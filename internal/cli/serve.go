package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/asenawritescode/kora/internal/analytics"
	purchasingengine "github.com/asenawritescode/kora/internal/application/purchasing/engineadapter"
	subscriptionjobs "github.com/asenawritescode/kora/internal/application/subscriptions/engineadapter"
	"github.com/asenawritescode/kora/internal/auth"
	"github.com/asenawritescode/kora/internal/engine"
	"github.com/asenawritescode/kora/internal/envcompat"
	"github.com/asenawritescode/kora/internal/ingress"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/natsprovider"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/script"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/storage"
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

func splitHTTPOrigins(value string) []string {
	seen := make(map[string]struct{})
	origins := make([]string, 0)
	for _, origin := range strings.Split(value, ",") {
		origin = strings.TrimSpace(origin)
		if origin == "" {
			continue
		}
		if _, exists := seen[origin]; exists {
			continue
		}
		seen[origin] = struct{}{}
		origins = append(origins, origin)
	}
	return origins
}

func enginePlacementConfig(localDeploymentID, managementDeploymentID, legacyCellID, deploymentURLs, legacyCellURLs string) (string, map[string]string, error) {
	localDeploymentID, managementDeploymentID, legacyCellID = strings.TrimSpace(localDeploymentID), strings.TrimSpace(managementDeploymentID), strings.TrimSpace(legacyCellID)
	normalizedLocal, err := site.NormalizeRuntimeCellID(localDeploymentID)
	if err != nil {
		return "", nil, fmt.Errorf("invalid KORA_ENGINE_LOCAL_DEPLOYMENT_ID: %w", err)
	}
	normalizedManagement, err := site.NormalizeRuntimeCellID(managementDeploymentID)
	if err != nil {
		return "", nil, fmt.Errorf("invalid KORA_ENGINE_DEPLOYMENT_ID: %w", err)
	}
	normalizedCell, err := site.NormalizeRuntimeCellID(legacyCellID)
	if err != nil {
		return "", nil, fmt.Errorf("invalid KORA_ENGINE_CELL_ID: %w", err)
	}
	if normalizedLocal != "" && normalizedCell != "" && normalizedLocal != normalizedCell {
		return "", nil, fmt.Errorf("KORA_ENGINE_LOCAL_DEPLOYMENT_ID %q conflicts with legacy KORA_ENGINE_CELL_ID %q", normalizedLocal, normalizedCell)
	}
	localID := normalizedLocal
	if localID == "" {
		localID = normalizedCell
	}
	if localID == "" {
		localID = normalizedManagement
	}

	deploymentURLs, legacyCellURLs = strings.TrimSpace(deploymentURLs), strings.TrimSpace(legacyCellURLs)
	if deploymentURLs != "" && legacyCellURLs != "" {
		deploymentMap, parseErr := ingress.ParseDeploymentURLs(deploymentURLs)
		if parseErr != nil {
			return "", nil, fmt.Errorf("parse KORA_ENGINE_DEPLOYMENT_URLS: %w", parseErr)
		}
		legacyMap, parseErr := ingress.ParseCellURLs(legacyCellURLs)
		if parseErr != nil {
			return "", nil, fmt.Errorf("parse KORA_ENGINE_CELL_URLS: %w", parseErr)
		}
		if !sameEngineEndpointMap(deploymentMap, legacyMap) {
			return "", nil, fmt.Errorf("KORA_ENGINE_DEPLOYMENT_URLS conflicts with legacy KORA_ENGINE_CELL_URLS")
		}
		if localID == "" {
			return "", nil, fmt.Errorf("deployment ingress endpoints require a local deployment or cell ID")
		}
		return localID, deploymentMap, nil
	}
	if deploymentURLs != "" {
		if localID == "" {
			return "", nil, fmt.Errorf("KORA_ENGINE_DEPLOYMENT_URLS requires KORA_ENGINE_DEPLOYMENT_ID or KORA_ENGINE_CELL_ID")
		}
		parsed, parseErr := ingress.ParseDeploymentURLs(deploymentURLs)
		if parseErr != nil {
			return "", nil, fmt.Errorf("parse KORA_ENGINE_DEPLOYMENT_URLS: %w", parseErr)
		}
		return localID, parsed, nil
	}
	if legacyCellURLs != "" {
		if localID == "" {
			return "", nil, fmt.Errorf("KORA_ENGINE_CELL_URLS requires KORA_ENGINE_CELL_ID or KORA_ENGINE_DEPLOYMENT_ID")
		}
		parsed, parseErr := ingress.ParseCellURLs(legacyCellURLs)
		if parseErr != nil {
			return "", nil, fmt.Errorf("parse KORA_ENGINE_CELL_URLS: %w", parseErr)
		}
		return localID, parsed, nil
	}
	return localID, nil, nil
}

func sameEngineEndpointMap(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func envPositiveInt(name string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}

func envPositiveUint64(name string, fallback uint64) (uint64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || value == 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}

func sessionCacheBudgetFromEnv() (uint64, error) {
	value, err := envPositiveUint64("KORA_SESSION_CACHE_MAX_BYTES", auth.DefaultSessionCacheBudgetBytes)
	if err != nil {
		return 0, err
	}
	if value > auth.MaxSessionCacheBudgetBytes {
		return 0, fmt.Errorf("KORA_SESSION_CACHE_MAX_BYTES cannot exceed %d", auth.MaxSessionCacheBudgetBytes)
	}
	return value, nil
}

func analyticsQueueBudgetFromEnv() (uint64, error) {
	name := "KORA_ANALYTICS_PROCESS_MAX_QUEUED_BYTES"
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		// Preserve the historical shared setting for existing deployments. New
		// deployments can tune the process budget independently of each tenant's
		// analytics wake-up queue via PROCESS_MAX_QUEUED_BYTES.
		name = "KORA_ANALYTICS_MAX_QUEUED_BYTES"
		raw = strings.TrimSpace(os.Getenv(name))
	}
	value := analytics.DefaultAnalyticsQueueAdmissionBytes
	if raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || parsed == 0 {
			return 0, fmt.Errorf("%s must be a positive integer", name)
		}
		value = parsed
	}
	if value > analytics.MaxAnalyticsQueueAdmissionBytes {
		return 0, fmt.Errorf("%s cannot exceed %d", name, analytics.MaxAnalyticsQueueAdmissionBytes)
	}
	return value, nil
}

func analyticsSubscriberLimitFromEnv() (int, error) {
	value, err := envPositiveInt("KORA_ANALYTICS_MAX_SUBSCRIBERS", analytics.DefaultAnalyticsProcessMaxListeners)
	if err != nil {
		return 0, err
	}
	if value > analytics.MaxAnalyticsProcessListeners {
		return 0, fmt.Errorf("KORA_ANALYTICS_MAX_SUBSCRIBERS cannot exceed %d", analytics.MaxAnalyticsProcessListeners)
	}
	return value, nil
}

func uploadAdmissionFromEnv() (int, uint64, error) {
	maxActive, err := envPositiveInt("KORA_UPLOAD_MAX_ACTIVE", engine.DefaultUploadMaxActive)
	if err != nil {
		return 0, 0, err
	}
	if maxActive > engine.MaxUploadMaxActive {
		return 0, 0, fmt.Errorf("KORA_UPLOAD_MAX_ACTIVE cannot exceed %d", engine.MaxUploadMaxActive)
	}
	maxBytes, err := envPositiveUint64("KORA_UPLOAD_ACTIVE_BUDGET_BYTES", engine.DefaultUploadActiveBytes)
	if err != nil {
		return 0, 0, err
	}
	if maxBytes > engine.MaxUploadActiveBytes {
		return 0, 0, fmt.Errorf("KORA_UPLOAD_ACTIVE_BUDGET_BYTES cannot exceed %d", engine.MaxUploadActiveBytes)
	}
	return maxActive, maxBytes, nil
}

func firstEnvBool(def bool, names ...string) bool {
	for _, name := range names {
		if v := os.Getenv(name); v != "" {
			return v != "false" && v != "0"
		}
	}
	return def
}

func outboxEnabledFromEnv(value string, brokerEnabled bool) bool {
	if brokerEnabled {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "false", "0", "off", "disabled":
		return false
	default:
		// Analytics workers are active by default, so kernel writes must enter
		// the transactional outbox even when no external broker is configured.
		return true
	}
}

// storageConfigFromEnv keeps environment parsing at the CLI boundary. Engine
// applies tenant overrides and owns backend construction and bucket setup.
func storageConfigFromEnv() storage.Config {
	return storage.Config{
		Backend:         firstEnv("KORA_STORAGE_BACKEND"),
		LocalPath:       os.Getenv("KORA_STORAGE_LOCAL_PATH"),
		S3Endpoint:      firstEnv("KORA_STORAGE_S3_ENDPOINT"),
		S3Region:        firstEnv("KORA_STORAGE_S3_REGION"),
		S3Bucket:        firstEnv("KORA_STORAGE_S3_BUCKET"),
		S3AccessKey:     firstEnv("KORA_STORAGE_S3_ACCESS_KEY"),
		S3SecretKey:     firstEnv("KORA_STORAGE_S3_SECRET_KEY"),
		S3UseSSL:        firstEnvBool(true, "KORA_STORAGE_S3_USE_SSL"),
		S3PublicBaseURL: firstEnv("KORA_STORAGE_S3_PUBLIC_URL"),
	}
}

var (
	httpPortFlag int
)

func init() {
	serveCmd.Flags().IntVar(&httpPortFlag, "port", 0, "HTTP port (overrides common config)")
}

func runServe() error {
	processCtx := context.Background()
	sc := site.LoadStartupConfig()
	if err := sc.Validate(); err != nil {
		return err
	}
	if sc.DBDSN == "" {
		return fmt.Errorf("DB_DSN is required; Engine serves sites from the canonical platform directory")
	}
	common := site.CommonConfigFromEnv()
	configureLogging(common.LogLevel, common.LogFormat)
	storageResolver := engine.NewStorageResolver(storageConfigFromEnv())
	localDeploymentID, deploymentURLs, err := enginePlacementConfig(
		os.Getenv("KORA_ENGINE_LOCAL_DEPLOYMENT_ID"), os.Getenv("KORA_ENGINE_DEPLOYMENT_ID"), os.Getenv("KORA_ENGINE_CELL_ID"),
		os.Getenv("KORA_ENGINE_DEPLOYMENT_URLS"), os.Getenv("KORA_ENGINE_CELL_URLS"),
	)
	if err != nil {
		return err
	}
	telemetryConfig := engine.TelemetryConfig{
		ServiceName: os.Getenv("OTEL_SERVICE_NAME"), ServiceVersion: Version,
		TracesEnabled: !strings.EqualFold(os.Getenv("OTEL_TRACES_EXPORTER"), "none") &&
			(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""),
		MetricsEnabled: os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") != "" &&
			!strings.EqualFold(os.Getenv("OTEL_METRICS_EXPORTER"), "none"),
	}
	if common.DBUser == "" || common.DBPassword == "" {
		slog.Warn("platform db_user or db_password not set — site creation from console UI will fail. Set KORA_DB_USER / KORA_DB_PASSWORD env vars.")
	}
	retirementLimit := uint16(32)
	if raw := strings.TrimSpace(os.Getenv("KORA_MAX_RETIRING_GENERATIONS")); raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 16)
		if err != nil || parsed == 0 {
			return fmt.Errorf("KORA_MAX_RETIRING_GENERATIONS must be an integer between 1 and 65535")
		}
		retirementLimit = uint16(parsed)
	}
	maxResidentTenants, err := envPositiveInt("KORA_MAX_RESIDENT_TENANTS", 256)
	if err != nil {
		return err
	}
	activationWaiters, err := envPositiveInt("KORA_TENANT_ACTIVATION_WAITERS", 32)
	if err != nil {
		return err
	}
	if activationWaiters > int(^uint16(0)) {
		return fmt.Errorf("KORA_TENANT_ACTIVATION_WAITERS cannot exceed %d", ^uint16(0))
	}
	activationWaitMS, err := envPositiveInt("KORA_TENANT_ACTIVATION_WAIT_MS", 1500)
	if err != nil {
		return err
	}
	asyncHookQueueSize, err := envPositiveInt("KORA_ASYNC_HOOK_QUEUE_SIZE", engine.DefaultAsyncHookQueueSize)
	if err != nil {
		return err
	}
	asyncHookMaxQueuedBytes, err := envPositiveUint64("KORA_ASYNC_HOOK_MAX_QUEUED_BYTES", engine.DefaultAsyncHookMaxQueuedBytes)
	if err != nil {
		return err
	}
	asyncHookMaxEventBytes, err := envPositiveUint64("KORA_ASYNC_HOOK_MAX_EVENT_BYTES", engine.DefaultAsyncHookMaxEventBytes)
	if err != nil {
		return err
	}
	if asyncHookMaxEventBytes > asyncHookMaxQueuedBytes {
		return fmt.Errorf("KORA_ASYNC_HOOK_MAX_EVENT_BYTES cannot exceed KORA_ASYNC_HOOK_MAX_QUEUED_BYTES")
	}
	realtimeMaxStreams, err := envPositiveInt("KORA_REALTIME_MAX_STREAMS", engine.DefaultRealtimeMaxStreams)
	if err != nil {
		return err
	}
	if realtimeMaxStreams > engine.MaxRealtimeMaxStreams {
		return fmt.Errorf("KORA_REALTIME_MAX_STREAMS cannot exceed %d", engine.MaxRealtimeMaxStreams)
	}
	sessionCacheBudgetBytes, err := sessionCacheBudgetFromEnv()
	if err != nil {
		return err
	}
	uploadMaxActive, uploadActiveBudgetBytes, err := uploadAdmissionFromEnv()
	if err != nil {
		return err
	}
	analyticsQueueBudgetBytes, err := analyticsQueueBudgetFromEnv()
	if err != nil {
		return err
	}
	analyticsMaxSubscribers, err := analyticsSubscriberLimitFromEnv()
	if err != nil {
		return err
	}
	scriptBudget := script.DefaultBudgetConfig()
	if scriptBudget.PerTenant, err = envPositiveInt("KORA_SCRIPT_MAX_CONCURRENCY", scriptBudget.PerTenant); err != nil {
		return err
	}
	if scriptBudget.MaxTenants, err = envPositiveInt("KORA_SCRIPT_MAX_ACTIVE_TENANTS", scriptBudget.MaxTenants); err != nil {
		return err
	}
	if raw := strings.TrimSpace(os.Getenv("KORA_SCRIPT_MAX_WAITERS")); raw != "" {
		value, parseErr := strconv.Atoi(raw)
		if parseErr != nil || value < 0 {
			return fmt.Errorf("KORA_SCRIPT_MAX_WAITERS must be a non-negative integer")
		}
		scriptBudget.MaxWaiters = value
	}
	waitMS, err := envPositiveInt("KORA_SCRIPT_WAIT_MS", int(scriptBudget.Wait.Milliseconds()))
	if err != nil {
		return err
	}
	timeoutMS, err := envPositiveInt("KORA_SCRIPT_TIMEOUT_MS", int(scriptBudget.DefaultTimeout.Milliseconds()))
	if err != nil {
		return err
	}
	scriptBudget.Wait = time.Duration(waitMS) * time.Millisecond
	scriptBudget.DefaultTimeout = time.Duration(timeoutMS) * time.Millisecond
	var kernelCommands *kernel.CommandRegistry
	if dir := os.Getenv("KORA_COMMANDS_DIR"); dir != "" {
		commands, err := kernel.LoadCommandDir(dir)
		if err != nil {
			return fmt.Errorf("loading command definitions from %s: %w", dir, err)
		}
		kernelCommands = commands
		slog.Info("command definitions loaded", "dir", dir, "count", len(commands.List()))
	}
	var httpAllowlist []string
	for _, domain := range strings.Split(os.Getenv("KORA_SCRIPTS_HTTP_ALLOWLIST"), ",") {
		if domain = strings.TrimSpace(domain); domain != "" {
			httpAllowlist = append(httpAllowlist, domain)
		}
	}
	scriptEnabled := os.Getenv("KORA_SCRIPTS_ENABLED")
	var natsConfig *natsprovider.Config
	if natsEnabled() {
		config := natsprovider.FromEnv()
		natsConfig = &config
	}
	cloudRelayConfig := analytics.LoadCloudRelayConfig()
	port := common.HTTPPort
	if httpPortFlag > 0 {
		port = httpPortFlag
	}
	listenerTLS := knet.TLSConfig{Mode: common.TLSMode, Email: common.TLSEmail}
	var managementPackageResolver engine.ManagementPackageArtifactResolver
	artifactURL, artifactToken := strings.TrimSpace(os.Getenv("KORA_ENGINE_MANAGEMENT_ARTIFACT_URL")), envcompat.IntegrationToken()
	if artifactURL != "" { // the shared token is always set; the URL turns package downloads on
		managementPackageResolver, err = engine.NewHTTPManagementPackageArtifactResolver(artifactURL, artifactToken, nil)
		if err != nil {
			return fmt.Errorf("configure Engine management package artifact resolver: %w", err)
		}
	}
	engineConfig := engine.Config{
		Telemetry: telemetryConfig, DBDriver: sc.DBType, DBDSN: sc.DBDSN, DBDialect: common.DBType,
		RawCellID: localDeploymentID, Common: common, BinaryVersion: Version,
		TenantConcurrency: 8, RetirementLimit: retirementLimit, MaxResidentTenants: uint32(maxResidentTenants),
		TenantActivationWaiters: uint16(activationWaiters), TenantActivationWait: time.Duration(activationWaitMS) * time.Millisecond, ResolveStorage: storageResolver,
		CellURLs: deploymentURLs, DirectoryConsumerID: os.Getenv("KORA_SITE_DIRECTORY_CONSUMER_ID"),
		NATSConfig: natsConfig, ScriptsEnabled: scriptEnabled == "" || scriptEnabled == "true",
		KernelCommands: kernelCommands, HTTPAllowlist: httpAllowlist, CloudRelay: cloudRelayConfig,
		CORSAllowedOrigins: splitHTTPOrigins(os.Getenv("KORA_CORS_ALLOWED_ORIGINS")),
		Workflow:           engine.SelfHostedWorkflowConfig(),
		Analytics:          analytics.LoadConfig(),
		OutboxEnabled:      outboxEnabledFromEnv(os.Getenv("KORA_OUTBOX"), natsEnabled()),
		PlatformDBUser:     common.DBUser, PlatformDBPassword: common.DBPassword,
		AllowConsoleOnboarding:          sc.AllowConsoleOnboarding,
		ReloadToken:                     envcompat.IntegrationToken(),
		ManagementToken:                 envcompat.IntegrationToken(),
		ManagementCallerID:              os.Getenv("KORA_ENGINE_MANAGEMENT_CALLER_ID"),
		PublicAppBaseURL:                strings.TrimSpace(os.Getenv("KORA_PUBLIC_APP_BASE_URL")),
		CloudIdentityURL:                strings.TrimSpace(os.Getenv("KORA_ENGINE_CLOUD_IDENTITY_URL")),
		CloudIdentityToken:              envcompat.IntegrationToken(),
		DeploymentID:                    os.Getenv("KORA_ENGINE_DEPLOYMENT_ID"),
		ManagementDatabaseAllocationRef: os.Getenv("KORA_ENGINE_MANAGEMENT_DATABASE_ALLOCATION_REF"),
		ManagementPackageResolver:       managementPackageResolver,
		DirectoryLagMetricsEnabled:      telemetryConfig.MetricsEnabled,
		AsyncHookQueueSize:              asyncHookQueueSize, AsyncHookMaxQueuedBytes: asyncHookMaxQueuedBytes,
		AsyncHookMaxEventBytes:    asyncHookMaxEventBytes,
		ScriptBudget:              scriptBudget,
		RealtimeMaxStreams:        realtimeMaxStreams,
		SessionCacheBudgetBytes:   sessionCacheBudgetBytes,
		UploadMaxActive:           uploadMaxActive,
		UploadActiveBudgetBytes:   uploadActiveBudgetBytes,
		AnalyticsQueueBudgetBytes: analyticsQueueBudgetBytes,
		AnalyticsMaxSubscribers:   analyticsMaxSubscribers,
		SchedulerHandlerFactory:   subscriptionjobs.ScheduledJobHandlersWithModuleBilling,
		Listener: engine.HTTPServerConfig{
			ListenAddress: fmt.Sprintf(":%d", port), TLS: listenerTLS,
			ReadTimeout:  time.Duration(common.ReadTimeout) * time.Second,
			WriteTimeout: time.Duration(common.WriteTimeout) * time.Second,
			IdleTimeout:  time.Duration(common.IdleTimeout) * time.Second,
		},
		OnReady: func(metrics engine.Metrics) {
			slog.Info("engine startup ready", "duration_ms", metrics.StartupDuration.Milliseconds(),
				"sites_discovered", metrics.DiscoveredTenants, "sites_loaded", metrics.LoadedTenants,
				"db_open_connections", metrics.OpenConnections, "db_in_use_connections", metrics.InUseConnections,
				"db_idle_connections", metrics.IdleConnections, "db_pool_wait_count", metrics.PoolWaitCount,
				"db_pool_wait_duration_ms", metrics.PoolWaitDuration.Milliseconds(),
				"heap_alloc_bytes", metrics.HeapAllocBytes, "heap_sys_bytes", metrics.HeapSysBytes)
		},
	}
	// The executable is the application composition root. Register the
	// purchasing module here; Engine's tenant runtime remains domain-neutral.
	purchasingengine.Configure(&engineConfig)
	engineProcess, err := engine.New(processCtx, engineConfig)
	if err != nil {
		return fmt.Errorf("initialize Engine: %w", err)
	}
	return engineProcess.Run(processCtx)
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
