package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/asenawritescode/kora/contract"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/site"
)

// TenantRuntimeAssemblyConfig contains the typed dependencies needed to
// publish the startup tenant set and its directory-backed activation policy.
// The ProcessResources owner must already own Runtimes until this constructor
// installs the SiteRouter; after that point it owns the router and generations.
type TenantRuntimeAssemblyConfig struct {
	Runtimes               []*knet.LoadedSite
	DirectoryDescriptors   []contract.SiteDescriptor
	Directory              TenantRuntimeDirectory
	DirectoryRevision      uint64
	DirectoryConsumerID    string
	DirectoryWorkAdmission interface {
		Acquire(context.Context) (func(), error)
	}
	TenantActivationWorkAdmission func(context.Context, *site.Config) (func(), error)
	CellID                        string
	RetirementLimit               uint16
	MaxResidentTenants            uint32
	MaxActivationWaiters          uint16
	ActivationWait                time.Duration
	Common                        *site.CommonConfig
	BinaryVersion                 string
	Ingress                       TenantIngressDirectory
	Services                      TenantServiceConfig
	Resources                     *ProcessResources
}

// TenantRuntimeAssembly is the coherent runtime and directory control plane
// consumed by HTTP routes and the process-owned directory feed.
type TenantRuntimeAssembly struct {
	Tenants            *knet.SiteRouter
	Coordinator        *TenantRuntimeCoordinator
	DirectoryConsumer  *site.DirectoryConsumer
	DurableWorkScanner *DurableWorkScanner
	ReloadController   *TenantReloadController
}

// AssembleTenantRuntime constructs the startup tenant manager, installs the
// same service bundle used for future activation, and wires directory reload
// through the single runtime coordinator. On any error, Resources retains the
// partially assembled state for the caller's normal startup rollback path.
func AssembleTenantRuntime(config TenantRuntimeAssemblyConfig) (*TenantRuntimeAssembly, error) {
	if config.Resources == nil || config.Directory == nil || config.Common == nil {
		return nil, errors.New("tenant runtime assembly requires process resources, directory, and common config")
	}
	if config.DirectoryConsumerID == "" {
		return nil, errors.New("tenant runtime assembly requires a stable directory consumer identity")
	}
	if config.RetirementLimit == 0 {
		return nil, errors.New("tenant runtime assembly requires a positive retirement limit")
	}
	if config.Services.ProcessContext == nil || config.Services.ResolveStorage == nil {
		return nil, errors.New("tenant runtime assembly requires process context and storage resolver")
	}
	if config.Services.Analytics != nil && config.Services.Analytics.WALDir != "" {
		if err := migrateAnalyticsWALNamespaces(config.Services.ProcessContext, config.Services.Analytics.WALDir, config.Directory, config.CellID, config.DirectoryWorkAdmission); err != nil {
			return nil, fmt.Errorf("migrate analytics WAL to stable tenant namespaces: %w", err)
		}
	}
	for _, runtime := range config.Runtimes {
		if runtime == nil {
			return nil, errors.New("tenant runtime assembly contains a nil startup runtime")
		}
		runtime.DirectoryRevision = config.DirectoryRevision
	}
	tenants, err := knet.NewSiteRouterWithRetirementLimit(config.Runtimes, config.RetirementLimit, config.MaxResidentTenants)
	if err != nil {
		return nil, fmt.Errorf("initialize tenant runtime manager: %w", err)
	}
	config.Resources.Tenants = tenants
	config.Resources.UnregisteredTenants = nil

	services := config.Services
	services.Common = config.Common
	installer, err := NewTenantServiceInstaller(services)
	if err != nil {
		return nil, fmt.Errorf("configure tenant service installer: %w", err)
	}
	coordinator, err := NewTenantRuntimeCoordinator(TenantRuntimeCoordinatorConfig{
		Tenants: tenants, Directory: config.Directory, Common: config.Common, BinaryVersion: config.BinaryVersion,
		Installer: installer, Ingress: config.Ingress,
		ActivationWorkAdmission: func(ctx context.Context, info site.DBSiteInfo) (func(), error) {
			if config.TenantActivationWorkAdmission == nil {
				return func() {}, nil
			}
			tenantConfig := site.ReconstructSiteConfigFromDBInfo(info, config.Common)
			return config.TenantActivationWorkAdmission(ctx, tenantConfig)
		},
		RetainRuntime:        config.Resources.RetainUnregisteredTenant,
		MaxResidentTenants:   config.MaxResidentTenants,
		MaxActivationWaiters: config.MaxActivationWaiters, ActivationWait: config.ActivationWait,
	})
	if err != nil {
		return nil, fmt.Errorf("configure tenant runtime coordinator: %w", err)
	}
	config.Resources.TenantCoordinator = coordinator
	tenants.SetLazyDirectory(config.DirectoryDescriptors, coordinator.ActivateFromDirectory)

	for _, runtime := range config.Runtimes {
		info, err := config.Directory.GetByID(runtime.SiteID)
		if err != nil {
			return nil, fmt.Errorf("load tenant directory entry for %s during service activation: %w", runtime.SiteID, err)
		}
		if err := coordinator.Install(info, runtime); err != nil {
			return nil, fmt.Errorf("install tenant services for %s: %w", runtime.SiteID, err)
		}
	}
	consumer, err := NewTenantDirectoryConsumer(TenantDirectoryConfig{
		Directory: config.Directory, Tenants: tenants, CellID: config.CellID,
		ConsumerID: config.DirectoryConsumerID, InitialCursor: config.DirectoryRevision,
		Coordinator: coordinator, WorkAdmission: config.DirectoryWorkAdmission,
	})
	if err != nil {
		return nil, fmt.Errorf("configure tenant directory consumer: %w", err)
	}
	scanner, err := NewDurableWorkScanner(DurableWorkScannerConfig{
		ParentContext: config.Services.ProcessContext, PlatformDB: config.Resources.PlatformDB,
		PlatformDialect: config.Resources.PlatformDialect, Directory: config.Directory,
		Tenants: tenants, Coordinator: coordinator, WorkAdmission: config.DirectoryWorkAdmission,
		TenantWorkAdmission: func(config *site.Config) (interface {
			Acquire(context.Context) (func(), error)
		}, error) {
			return site.ConnectionAdmission(config)
		},
		Common: config.Common, CellID: config.CellID,
		DirectoryConsumerID: config.DirectoryConsumerID,
		Analytics:           config.Services.Analytics,
	})
	if err != nil {
		return nil, fmt.Errorf("configure durable tenant work scanner: %w", err)
	}
	config.Resources.DurableWorkScanner = scanner
	if err := config.Resources.RegisterWorkerStart(scanner.Start); err != nil {
		return nil, fmt.Errorf("register durable tenant work scanner: %w", err)
	}
	return &TenantRuntimeAssembly{
		Tenants: tenants, Coordinator: coordinator, DirectoryConsumer: consumer, DurableWorkScanner: scanner,
		ReloadController: &TenantReloadController{
			Tenants: tenants, Directory: config.Directory, PlatformDB: config.Resources.PlatformDB,
			WorkAdmission: config.DirectoryWorkAdmission, Coordinator: coordinator,
		},
	}, nil
}
