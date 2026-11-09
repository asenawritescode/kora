package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/ingress"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/storage"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

type ProcessStartupConfig struct {
	Telemetry                  TelemetryConfig
	DBDriver                   string
	DBDSN                      string
	DBDialect                  string
	RawCellID                  string
	Common                     *site.CommonConfig
	BinaryVersion              string
	TenantConcurrency          uint16
	MaxResidentTenants         uint32
	ResolveStorage             func(*site.Config) (storage.Backend, error)
	CellURLs                   map[string]string
	DirectoryConsumerID        string
	DirectoryLagMetricsEnabled bool
}

type ProcessStartupAssembly struct {
	Context             context.Context
	Span                trace.Span
	Started             time.Time
	DiscoveredTenants   int
	Platform            PlatformSiteDirectory
	Tenants             StartupTenants
	Ingress             *ingress.Gateway
	DirectoryConsumerID string
}

type processStartupDependencies struct {
	initializeTelemetry func(context.Context, TelemetryConfig) (func(context.Context) error, error)
	openPlatform        func(string, string, string, string) (PlatformSiteDirectory, error)
	loadTenants         func(context.Context, StartupTenantConfig) (StartupTenants, error)
}

// AssembleProcessStartup acquires process telemetry, the canonical platform
// directory, and the bounded cold-start tenant set in dependency order. Every
// successful acquisition is immediately registered with ProcessResources so
// the caller can roll back the same owner if a later stage fails.
func AssembleProcessStartup(ctx context.Context, config ProcessStartupConfig, resources *ProcessResources) (*ProcessStartupAssembly, error) {
	return assembleProcessStartup(ctx, config, resources, processStartupDependencies{
		initializeTelemetry: InitializeTelemetry,
		openPlatform:        OpenPlatformSiteDirectory,
		loadTenants:         LoadStartupTenants,
	})
}

func assembleProcessStartup(ctx context.Context, config ProcessStartupConfig, resources *ProcessResources, deps processStartupDependencies) (_ *ProcessStartupAssembly, err error) {
	if ctx == nil || resources == nil || config.Common == nil {
		return nil, errors.New("process startup requires context, resource owner, and common config")
	}
	if config.DBDriver == "" || config.DBDialect == "" || config.DBDSN == "" {
		return nil, errors.New("process startup requires platform database driver, DSN, and dialect")
	}
	if config.ResolveStorage == nil {
		return nil, errors.New("process startup requires tenant storage resolver")
	}
	if deps.initializeTelemetry == nil || deps.openPlatform == nil || deps.loadTenants == nil {
		return nil, errors.New("process startup dependencies are incomplete")
	}
	started := time.Now()
	shutdownTelemetry, err := deps.initializeTelemetry(ctx, config.Telemetry)
	if err != nil {
		return nil, fmt.Errorf("initialize telemetry: %w", err)
	}
	resources.TelemetryShutdown = shutdownTelemetry
	startupCtx, startupSpan := otel.Tracer("kora/engine/startup").Start(ctx, "engine.startup")
	spanTransferred := false
	defer func() {
		if !spanTransferred {
			startupSpan.End()
		}
	}()
	platform, err := deps.openPlatform(config.DBDriver, config.DBDSN, config.DBDialect, config.RawCellID)
	if err != nil {
		return nil, fmt.Errorf("open platform directory: %w", err)
	}
	resources.PlatformDB = platform.DB
	resources.PlatformDialect = db.Resolve(config.DBDialect)
	allDescriptors := append([]contract.SiteDescriptor(nil), platform.Descriptors...)
	discoveredTenantCount := countActiveDescriptors(localDirectoryDescriptors(allDescriptors, platform.CellID))
	// Compatibility callers may still populate Sites; never retain or pass
	// their credential-bearing snapshot into lazy runtime assembly.
	platform.Sites = nil
	tenants, err := deps.loadTenants(startupCtx, StartupTenantConfig{
		// Startup intentionally activates no tenant database. The credential-free
		// descriptors are installed into the runtime manager; credentials are
		// fetched for a single tenant only when a request first routes to it.
		Sites: nil, Common: config.Common, BinaryVersion: config.BinaryVersion,
		Concurrency: config.TenantConcurrency, SharedPlatformDSN: config.DBDSN,
		ResolveStorage: config.ResolveStorage,
	})
	if err != nil {
		return nil, fmt.Errorf("load startup tenants: %w", err)
	}
	platform.Descriptors = localDirectoryDescriptors(allDescriptors, platform.CellID)
	tenants.Domains = descriptorDomains(platform.Descriptors)
	resources.UnregisteredTenants = tenants.Runtimes
	consumerID := strings.TrimSpace(config.DirectoryConsumerID)
	if consumerID == "" {
		hostname, _ := os.Hostname()
		if hostname == "" {
			hostname = "localhost"
		}
		consumerID = "engine:" + hostname
	}
	var cellGateway *ingress.Gateway
	if platform.CellID != "" {
		cellGateway, err = ingress.New(platform.CellID, config.CellURLs)
		if err != nil {
			return nil, fmt.Errorf("configure Engine cell ingress: %w", err)
		}
		if err := cellGateway.ValidateDirectory(allDescriptors); err != nil {
			return nil, fmt.Errorf("validate cell ingress placement: %w", err)
		}
		if err := cellGateway.ReplaceDirectory(allDescriptors); err != nil {
			return nil, fmt.Errorf("publish Engine deployment ingress directory: %w", err)
		}
		for _, descriptor := range platform.Descriptors {
			if descriptor.Status == "active" || descriptor.Status == "degraded" {
				cellGateway.MarkLocalReady(descriptor.SiteID, true)
			}
		}
		slog.Info("cell ingress ready", "cell_id", platform.CellID, "directory_sites", len(allDescriptors), "remote_cell_urls", len(config.CellURLs))
	}
	if config.DirectoryLagMetricsEnabled {
		metricCellID := platform.CellID
		if metricCellID == "" {
			metricCellID = "default"
		}
		metrics, err := NewDirectoryLagMetrics(metricCellID)
		if err != nil {
			return nil, fmt.Errorf("initialize site-directory metrics: %w", err)
		}
		admission, err := dbaccess.AdmissionFor(config.DBDriver, config.DBDSN)
		if err != nil {
			return nil, fmt.Errorf("configure site-directory lag admission: %w", err)
		}
		monitor, err := PrepareDirectoryLagMonitor(ctx, platform.Directory, consumerID, metrics, admission)
		if err != nil {
			return nil, fmt.Errorf("start site-directory lag monitor: %w", err)
		}
		resources.DirectoryLagMonitor = monitor
		if err := resources.RegisterWorkerStart(monitor.Start); err != nil {
			return nil, fmt.Errorf("register site-directory lag monitor: %w", err)
		}
	}
	spanTransferred = true
	return &ProcessStartupAssembly{
		Context: startupCtx, Span: startupSpan, Started: started, DiscoveredTenants: discoveredTenantCount, Platform: platform,
		Tenants: tenants, Ingress: cellGateway, DirectoryConsumerID: consumerID,
	}, nil
}

func localDirectoryDescriptors(descriptors []contract.SiteDescriptor, cellID string) []contract.SiteDescriptor {
	local := make([]contract.SiteDescriptor, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if descriptor.Status != "deleted" && site.BelongsToDeployment(descriptor.RuntimeCellID, cellID) {
			descriptor.Aliases = append([]string(nil), descriptor.Aliases...)
			local = append(local, descriptor)
		}
	}
	return local
}

func descriptorDomains(descriptors []contract.SiteDescriptor) []string {
	seen := make(map[string]struct{})
	var domains []string
	for _, descriptor := range descriptors {
		if descriptor.Status != "active" && descriptor.Status != "degraded" {
			continue
		}
		for _, alias := range descriptor.Aliases {
			alias = site.NormalizeSiteAlias(alias)
			if alias == "" {
				continue
			}
			if _, exists := seen[alias]; exists {
				continue
			}
			seen[alias] = struct{}{}
			domains = append(domains, alias)
		}
	}
	return domains
}

func countActiveDescriptors(descriptors []contract.SiteDescriptor) int {
	count := 0
	for _, descriptor := range descriptors {
		if descriptor.Status == "active" || descriptor.Status == "degraded" {
			count++
		}
	}
	return count
}
