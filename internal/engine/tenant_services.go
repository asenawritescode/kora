package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/api"
	"github.com/asenawritescode/kora/auth"
	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/email"
	"github.com/asenawritescode/kora/internal/application/process"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/natsprovider"
	knet "github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/plugin"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/recon"
	"github.com/asenawritescode/kora/requests"
	requestsql "github.com/asenawritescode/kora/requests/sqlstore"
	"github.com/asenawritescode/kora/scheduler"
	schedulersql "github.com/asenawritescode/kora/scheduler/sqlstore"
	"github.com/asenawritescode/kora/script"
	"github.com/asenawritescode/kora/secret"
	"github.com/asenawritescode/kora/site"
	"github.com/asenawritescode/kora/storage"
	"github.com/asenawritescode/kora/webhook"
	"github.com/asenawritescode/kora/workflow"
)

// TenantServiceConfig is the concrete dependency set for constructing services
// owned by one tenant generation. Deployment-specific storage construction is
// supplied by the process composition root; worker and service ownership stays
// in Engine.
type TenantServiceConfig struct {
	ProcessContext          context.Context
	Common                  *site.CommonConfig
	Analytics               *analytics.Config
	CloudRelay              *analytics.CloudRelayConfig
	AnalyticsQueueAdmission *analytics.AnalyticsQueueAdmission
	NATS                    *natsprovider.Provider
	ScriptRunner            script.Runner
	HTTPAllowlist           []string
	RealtimeAdmission       interface {
		Acquire(context.Context) (func(), error)
	}
	SessionCacheBudget *auth.SessionCacheBudget
	UploadAdmission    interface {
		Acquire(context.Context, uint64) (func(), error)
	}
	OutboxEnabled                     bool
	Workflow                          *WorkflowTenantConfig
	RequestAuthorizer                 RequestAuthorizerFactory
	WorkflowApprovals                 map[string]process.WorkflowApproval
	KernelCommands                    *kernel.CommandRegistry
	ModuleSchemas                     workflow.PayloadSchemaRegistry
	ModuleProviders                   func(site.DBSiteInfo, *sql.DB) ([]plugin.ProviderRegistration, []plugin.ProviderSelection, error)
	SchedulerHandlerFactory           ScheduledJobHandlerFactory
	SchedulerExecutionResolverFactory ScheduledJobExecutionResolverFactory
	Mailer                            *email.Sender
	ResolveStorage                    func(*site.SiteConfig) (storage.Backend, error)
	RegisterWorkerStart               func(func() error) error
}

// RequestAuthorizerFactory lets an owning domain add governed request policies
// to the generic direct-task policy at the application composition boundary.
// The Engine owns the request service and SQL lifecycle; the domain owns policy.
type RequestAuthorizerFactory func(*sql.DB, kdb.Dialect, *doctype.Registry, requests.Authorizer) (requests.Authorizer, error)

// TenantJobRuntime provides tenant-bound infrastructure to application-owned
// scheduled-job handlers without teaching Engine about domain job types.
type TenantJobRuntime struct {
	Database       *sql.DB
	Registry       *doctype.Registry
	Dialect        kdb.Dialect
	Kernel         *kernel.Kernel
	ModuleBindings *plugin.BindingPlan
	TenantID       string
	SiteName       string
	ConfigRevision uint64
}

// ScheduledJobHandlerFactory creates application handlers for one tenant
// generation. The application decides which configured types apply there.
type ScheduledJobHandlerFactory func(TenantJobRuntime) map[scheduler.JobType]scheduler.JobHandler

// ScheduledJobExecutionResolverFactory binds legal-entity scheduled work to
// trusted service grants owned by the application domain.
type ScheduledJobExecutionResolverFactory func(TenantJobRuntime) scheduler.ExecutionResolver

// NewTenantServiceInstaller validates process dependencies once and returns a
// tenant-generation installer used for startup activation and hot replacement.
func NewTenantServiceInstaller(config TenantServiceConfig) (func(site.DBSiteInfo, *knet.LoadedSite) error, error) {
	if config.Common == nil {
		return nil, errors.New("tenant service common configuration is required")
	}
	if config.ResolveStorage == nil {
		return nil, errors.New("tenant storage resolver is required")
	}
	if config.Analytics == nil {
		config.Analytics = analytics.LoadConfig()
	}
	if config.ProcessContext == nil {
		config.ProcessContext = context.Background()
	}
	return func(info site.DBSiteInfo, loaded *knet.LoadedSite) error {
		return installTenantServices(config, info, loaded)
	}, nil
}

func installTenantServices(config TenantServiceConfig, info site.DBSiteInfo, loaded *knet.LoadedSite) error {
	if loaded == nil {
		return errors.New("tenant runtime is required")
	}
	if loaded.DB == nil {
		return errors.New("tenant database is required")
	}
	if loaded.SessionManager != nil {
		loaded.SessionManager.SetCacheBudget(config.SessionCacheBudget)
		if err := registerTenantWorkerStart(config, func() error {
			loaded.SessionManager.StartCleanup()
			return nil
		}); err != nil {
			return fmt.Errorf("register session cleanup for tenant %s: %w", loaded.Name, err)
		}
	}
	services := loaded.RuntimeServiceSnapshot()
	services.Mailer = config.Mailer
	services.CommandSchemas = config.ModuleSchemas
	// Publish the in-progress bundle to the still-unpublished runtime so its
	// single Close path can roll back any worker started by this installer.
	loaded.RuntimeServices = &services
	cfg := site.ReconstructSiteConfigFromDBInfo(info, config.Common)
	dialect := kdb.Resolve(cfg.DBType)
	domainKernel := kernel.New(dialect, outbox.NewSQLWriter(dialect))
	domainKernel.CommandSchemas = config.ModuleSchemas
	backend, err := resolveTenantStorage(services.Storage, config.ResolveStorage, cfg)
	if err != nil {
		return fmt.Errorf("resolve storage: %w", err)
	}
	services.Storage = backend
	tenantID, err := contract.ParseTenantID(loaded.SiteID)
	if err != nil {
		return fmt.Errorf("parse tenant identity for module bindings: %w", err)
	}
	moduleStore := configstore.NewModuleStore(loaded.DB, dialect)
	if moduleErr := moduleStore.EnsureSchema(); moduleErr != nil {
		return fmt.Errorf("initialize module ownership store for tenant %s: %w", loaded.Name, moduleErr)
	}
	moduleSnapshot, moduleErr := moduleStore.Load(contract.TenantID(loaded.SiteID))
	if moduleErr != nil {
		return fmt.Errorf("load module ownership snapshot for tenant %s: %w", loaded.Name, moduleErr)
	}
	if moduleErr := contract.ValidateInstalledPackages(moduleSnapshot.Packages); moduleErr != nil {
		return fmt.Errorf("validate installed package composition for tenant %s: %w", loaded.Name, moduleErr)
	}
	baseModuleSchemas := config.ModuleSchemas
	config.ModuleSchemas, moduleErr = newModulePayloadSchemas(moduleSnapshot.Modules, baseModuleSchemas)
	if moduleErr != nil {
		return fmt.Errorf("compile module payload schemas for tenant %s: %w", loaded.Name, moduleErr)
	}
	services.CommandSchemas = config.ModuleSchemas
	domainKernel.CommandSchemas = config.ModuleSchemas
	if moduleErr := validateModuleExtensionSchemas(moduleSnapshot.Modules, config.ModuleSchemas); moduleErr != nil {
		return fmt.Errorf("validate module extension schemas for tenant %s: %w", loaded.Name, moduleErr)
	}
	var registeredExtensionSchema func(contract.SchemaRef) bool
	if config.ModuleSchemas != nil {
		registeredExtensionSchema = config.ModuleSchemas.Registered
	}
	extensionPlan, moduleErr := contract.OrderModuleExtensions(moduleSnapshot.Modules, registeredExtensionSchema)
	if moduleErr != nil {
		return fmt.Errorf("resolve module extensions for tenant %s: %w", loaded.Name, moduleErr)
	}
	var providers []plugin.ProviderRegistration
	var configuredSelections []plugin.ProviderSelection
	if config.ModuleProviders != nil {
		providers, configuredSelections, moduleErr = config.ModuleProviders(info, loaded.DB)
		if moduleErr != nil {
			return fmt.Errorf("load module providers for tenant %s: %w", loaded.Name, moduleErr)
		}
	}
	configuredProviders := append([]plugin.ProviderRegistration(nil), providers...)
	internalProviders, moduleErr := registeredKernelCommandProviders(tenantID, loaded.DB, loaded.Registry, moduleSnapshot.Modules, config.KernelCommands, domainKernel, config.ModuleSchemas, providers)
	if moduleErr != nil {
		return fmt.Errorf("register internal module command providers for tenant %s: %w", loaded.Name, moduleErr)
	}
	providers = append(providers, internalProviders...)
	publishedQueryProviders, moduleErr := registeredSQLPublishedQueryProviders(config.ProcessContext, tenantID, loaded.DB, dialect, moduleSnapshot.Modules, config.ModuleSchemas, providers)
	if moduleErr != nil {
		return fmt.Errorf("register durable published query providers for tenant %s: %w", loaded.Name, moduleErr)
	}
	providers = append(providers, publishedQueryProviders...)
	selections, moduleErr := mergeProviderSelections(tenantID, moduleSnapshot.ProviderSelections, contractProviderSelections(configuredSelections))
	if moduleErr != nil {
		return fmt.Errorf("resolve persisted provider selections for tenant %s: %w", loaded.Name, moduleErr)
	}
	resolveModuleBindings := func(scope contract.BusinessScope) (plugin.BindingPlan, error) {
		if scope.TenantID != contract.TenantID(loaded.SiteID) {
			return plugin.BindingPlan{}, contract.NewError(contract.CodePermissionDenied, "module binding scope belongs to another tenant")
		}
		if err := validateModuleExtensionSchemas(moduleSnapshot.Modules, config.ModuleSchemas); err != nil {
			return plugin.BindingPlan{}, err
		}
		plan, err := plugin.ResolveInstalledModules(scope, moduleSnapshot.Packages, moduleSnapshot.Modules, providers, pluginProviderSelections(selections), moduleSnapshot.Revision)
		if err != nil {
			return plan, err
		}
		plan.LeaseAdmission = moduleStore
		plan.LeaseReader = moduleStore
		return plan, nil
	}
	resolveCandidateModuleBindings := func(scope contract.BusinessScope, packages []contract.InstalledPackage, modules []contract.ModuleDescriptor, extra []contract.ProviderSelection, revision uint64) (plugin.BindingPlan, error) {
		if scope.TenantID != contract.TenantID(loaded.SiteID) {
			return plugin.BindingPlan{}, contract.NewError(contract.CodePermissionDenied, "module binding scope belongs to another tenant")
		}
		candidateSchemas, err := newModulePayloadSchemas(modules, baseModuleSchemas)
		if err != nil {
			return plugin.BindingPlan{}, err
		}
		if err := validateModuleExtensionSchemas(modules, candidateSchemas); err != nil {
			return plugin.BindingPlan{}, err
		}
		candidateKernel := kernel.New(dialect, outbox.NewSQLWriter(dialect))
		candidateKernel.CommandSchemas = candidateSchemas
		candidateProviders := append([]plugin.ProviderRegistration(nil), configuredProviders...)
		commands, err := registeredKernelCommandProviders(tenantID, loaded.DB, loaded.Registry, modules, config.KernelCommands, candidateKernel, candidateSchemas, candidateProviders)
		if err != nil {
			return plugin.BindingPlan{}, err
		}
		candidateProviders = append(candidateProviders, commands...)
		queries, err := registeredSQLPublishedQueryProviders(config.ProcessContext, tenantID, loaded.DB, dialect, modules, candidateSchemas, candidateProviders)
		if err != nil {
			return plugin.BindingPlan{}, err
		}
		candidateProviders = append(candidateProviders, queries...)
		baseSelections := filterProviderSelections(selections, scope)
		candidateExtra := filterProviderSelections(extra, scope)
		candidateSelections, err := mergeProviderSelections(tenantID, baseSelections, candidateExtra)
		if err != nil {
			return plugin.BindingPlan{}, err
		}
		plan, err := plugin.ResolveInstalledModules(scope, packages, modules, candidateProviders, pluginProviderSelections(candidateSelections), revision)
		if err != nil {
			return plan, err
		}
		plan.LeaseAdmission = moduleStore
		plan.LeaseReader = moduleStore
		return plan, nil
	}
	tenantScope := contract.BusinessScope{TenantID: contract.TenantID(loaded.SiteID), Kind: contract.ScopeKindTenant}
	bindingPlan, moduleErr := resolveModuleBindings(tenantScope)
	if moduleErr != nil {
		return fmt.Errorf("resolve module providers for tenant %s: %w", loaded.Name, moduleErr)
	}
	if len(bindingPlan.Blockers) > 0 {
		return fmt.Errorf("tenant %s has %d required module provider blockers", loaded.Name, len(bindingPlan.Blockers))
	}
	resolvedSelections, moduleErr := bindingPlan.ProviderSelections()
	if moduleErr != nil {
		return fmt.Errorf("validate resolved provider bindings for tenant %s: %w", loaded.Name, moduleErr)
	}
	selections, moduleErr = mergeProviderSelections(tenantID, selections, resolvedSelections)
	if moduleErr != nil {
		return fmt.Errorf("persist resolved provider selections for tenant %s: %w", loaded.Name, moduleErr)
	}
	persistedSelections := selections
	if !sameProviderSelections(moduleSnapshot.ProviderSelections, persistedSelections) {
		moduleSnapshot, moduleErr = moduleStore.CommitProviderSelections(config.ProcessContext, tenantID, moduleSnapshot.Revision, persistedSelections)
		if moduleErr != nil {
			return fmt.Errorf("persist provider bindings for tenant %s: %w", loaded.Name, moduleErr)
		}
		bindingPlan, moduleErr = resolveModuleBindings(tenantScope)
		if moduleErr != nil {
			return fmt.Errorf("resolve versioned module bindings for tenant %s: %w", loaded.Name, moduleErr)
		}
		if len(bindingPlan.Blockers) > 0 {
			return fmt.Errorf("tenant %s has %d required module provider blockers", loaded.Name, len(bindingPlan.Blockers))
		}
	}
	services.ModuleBindings = &bindingPlan
	services.ResolveModuleBindings = resolveModuleBindings
	services.ResolveCandidateModuleBindings = resolveCandidateModuleBindings
	services.LoadModuleSnapshot = func() (contract.ModuleSnapshot, error) {
		return moduleStore.Load(tenantID)
	}
	services.ExecutePublishedOperation = func(ctx context.Context, execution policy.Execution, scope contract.BusinessScope, owner contract.ModuleID, published contract.PublishedOperation, inputSchema contract.SchemaRef, meta contract.CommandMeta, input []byte) (contract.OperationReceipt, error) {
		return executePublishedOperation(ctx, moduleStore, resolveModuleBindings, domainKernel, config.KernelCommands, loaded.Registry, execution, scope, owner, published, inputSchema, meta, input)
	}
	services.LookupPublishedOperation = func(ctx context.Context, execution policy.Execution, scope contract.BusinessScope, owner contract.ModuleID, published contract.PublishedOperation, key contract.IdempotencyKey) (contract.OperationReceipt, bool, error) {
		return lookupPublishedOperation(ctx, moduleStore, domainKernel, config.KernelCommands, resolveModuleBindings, execution, scope, owner, published, key)
	}
	services.LookupPublishedOperationByID = func(ctx context.Context, execution policy.Execution, scope contract.BusinessScope, id contract.OperationID) (contract.OperationReceipt, bool, error) {
		return moduleStore.LoadPublishedOperationReceipt(ctx, scope.TenantID, id, execution.Actor(), scope)
	}
	services.PublishedProjections = &analytics.PublishedProjectionStore{DB: loaded.DB, Dialect: dialect, Site: loaded.SiteID}
	workAdmission, err := site.ConnectionAdmission(cfg)
	if err != nil {
		return fmt.Errorf("resolve tenant work admission: %w", err)
	}
	services.WorkAdmission = workAdmission
	businessRuns := &recon.BusinessRunStore{DB: loaded.DB, Dialect: dialect}
	setupCtx, cancelSetup := context.WithTimeout(config.ProcessContext, 30*time.Second)
	err = withTenantWorkAdmission(setupCtx, workAdmission, "ensure business reconciliation schema", func() error {
		return businessRuns.EnsureSchema(setupCtx)
	})
	cancelSetup()
	if err != nil {
		return fmt.Errorf("prepare business reconciliation store for tenant %s: %w", loaded.Name, err)
	}
	services.BusinessReconciliation = &recon.BusinessReconciliationService{
		Runs:  businessRuns,
		Store: businessRuns,
		ReadPage: func(ctx context.Context, request recon.BusinessQueryRequest) (contract.QueryResult, error) {
			plan, err := resolveModuleBindings(request.Scope)
			if err != nil {
				return contract.QueryResult{}, err
			}
			return plan.ExecutePublishedQuery(ctx, request.Scope, request.OwnerModuleID, request.Name, request.Input, request.Cursor, request.Limit)
		},
	}
	if config.ScriptRunner != nil {
		services.ScriptStore = &script.Store{DB: loaded.DB, Dialect: dialect}
		services.AsyncHookInvocations = services.ScriptStore
		services.SecretStore = secret.NewStore(loaded.DB, dialect)
		setupCtx, cancelSetup := context.WithTimeout(config.ProcessContext, 30*time.Second)
		err := withTenantWorkAdmission(setupCtx, workAdmission, "ensure durable lifecycle hook schema", func() error {
			return services.ScriptStore.EnsureAsyncHookInvocationSchema(setupCtx)
		})
		cancelSetup()
		if err != nil {
			return fmt.Errorf("prepare durable lifecycle hook store for tenant %s: %w", loaded.Name, err)
		}
	}
	requestService, requestStore, err := composeRequests(loaded.DB, dialect, loaded.Registry, config.RequestAuthorizer)
	if err != nil {
		return fmt.Errorf("compose request service: %w", err)
	}
	services.Requests = requestService
	transactionOpener, err := process.NewTenantTransactionOpener(tenantID, loaded.DB, workAdmission)
	if err != nil {
		return fmt.Errorf("construct tenant approval transaction opener: %w", err)
	}
	domainTx := &orm.TxManager{
		DB: loaded.DB, Registry: loaded.Registry, Dialect: dialect, Outbox: domainKernel.Outbox,
		SiteName: loaded.Name, AsyncHookTenantID: tenantID,
		AsyncHookConfigRevision: loaded.ConfigRevision,
		ModuleDescriptors:       append([]contract.ModuleDescriptor(nil), moduleSnapshot.Modules...),
		ModuleExtensions:        extensionPlan, ModuleExtensionSchemas: config.ModuleSchemas,
	}
	if config.ScriptRunner != nil {
		domainTx.ScriptRunner = config.ScriptRunner
		domainTx.ScriptStore = services.ScriptStore
		domainTx.AsyncHookInvocations = services.AsyncHookInvocations
		domainTx.ScriptProviderFactory = api.ScriptProviderFactory(loaded.Registry, loaded.Name, services.SecretStore, config.HTTPAllowlist)
	}
	domainKernel.TxManager = domainTx
	if len(config.WorkflowApprovals) > 0 {
		approvalApplier, coordinatorErr := process.NewCoordinator(requestService, domainKernel, loaded.Registry,
			transactionOpener, config.WorkflowApprovals)
		if coordinatorErr != nil {
			return fmt.Errorf("construct governed approval coordinator: %w", coordinatorErr)
		}
		services.ApprovalApplier = approvalApplier
	}
	if config.ScriptRunner != nil {
		if err := registerTenantWorkerStart(config, func() error {
			if !loaded.StartWorker(config.ProcessContext, func(ctx context.Context) {
				RunDurableLifecycleHookWorker(ctx, config.ScriptRunner, services.ScriptStore, loaded.DB, loaded.Registry,
					services.Outbox, loaded.SiteID, loaded.Name, loaded.DBType, services.SecretStore, config.HTTPAllowlist, workAdmission)
			}) {
				return fmt.Errorf("tenant runtime is stopping")
			}
			return nil
		}); err != nil {
			return fmt.Errorf("register durable lifecycle hook worker for tenant %s: %w", loaded.Name, err)
		}
	}
	if loaded.SessionManager != nil {
		loaded.SessionManager.SetCleanupAdmission(workAdmission)
	}
	services.RealtimeAdmission = config.RealtimeAdmission
	services.UploadAdmission = config.UploadAdmission
	bootstrapCtx, cancelBootstrap := context.WithTimeout(config.ProcessContext, 30*time.Second)
	err = withTenantWorkAdmission(bootstrapCtx, workAdmission, "bootstrap analytics schema", func() error {
		return analytics.BootstrapTablesContext(bootstrapCtx, loaded.DB, dialect)
	})
	cancelBootstrap()
	if err != nil {
		return fmt.Errorf("bootstrap tenant analytics tables: %w", err)
	}
	baseBus := analytics.NewChannelBusWithLimits(config.Analytics.ChannelSize, config.Analytics.MaxEventBytes, config.Analytics.MaxQueuedBytes, AnalyticsTenantWALDir(config.Analytics.WALDir, loaded.SiteID))
	if config.AnalyticsQueueAdmission != nil {
		queueAware, ok := baseBus.(interface {
			SetQueueAdmission(*analytics.AnalyticsQueueAdmission) error
		})
		if !ok {
			_ = baseBus.Close()
			return fmt.Errorf("analytics event bus does not support process queue admission")
		}
		if err := queueAware.SetQueueAdmission(config.AnalyticsQueueAdmission); err != nil {
			_ = baseBus.Close()
			return fmt.Errorf("configure process analytics wake-up queue admission: %w", err)
		}
	}
	multiBus, err := analytics.NewDeferredMultiBus(baseBus)
	if err != nil {
		_ = baseBus.Close()
		return fmt.Errorf("create tenant event fanout: %w", err)
	}
	if config.AnalyticsQueueAdmission != nil {
		if err := multiBus.SetQueueAdmission(config.AnalyticsQueueAdmission); err != nil {
			_ = multiBus.Close()
			return fmt.Errorf("configure process analytics subscriber queue admission: %w", err)
		}
	}
	loaded.AnalyticsEventBus = multiBus
	if err := registerTenantWorkerStart(config, func() error { return multiBus.Start() }); err != nil {
		return fmt.Errorf("register tenant event fanout for %s: %w", loaded.Name, err)
	}
	analyticsWorker := analytics.NewWorker(multiBus, loaded.DB, dialect, loaded.Registry, loaded.Name, config.Analytics)
	analyticsWorker.SetWorkAdmission(workAdmission)
	loaded.AnalyticsWorker = analyticsWorker
	if err := registerTenantWorkerStart(config, func() error {
		loaded.AnalyticsWorker.StartAsyncContext(loaded.WorkerContext(config.ProcessContext))
		return nil
	}); err != nil {
		return fmt.Errorf("register analytics worker for tenant %s: %w", loaded.Name, err)
	}
	services.EventBus = multiBus
	workflowConfig := config.Workflow
	var workflowRuntimeConfig *WorkflowTenantConfig
	if workflowConfig != nil {
		workflowConfigCopy := *workflowConfig
		workflowConfigCopy.Mailer = config.Mailer
		workflowConfigCopy.EventBus = services.EventBus
		workflowConfigCopy.CapabilityLeases = moduleStore
		for i := range workflowConfigCopy.Registrations {
			if lease := workflowConfigCopy.Registrations[i].CapabilityLease; lease != nil && lease.CredentialRef != "" {
				workflowConfigCopy.Registrations[i].CredentialResolver = secret.CredentialResolver{Store: services.SecretStore, Site: loaded.Name}
			}
		}
		workflowRuntimeConfig = &workflowConfigCopy
	}
	workflowWorker, definitionService, instanceService, err := configureTenantWorkflow(config.ProcessContext, workflowRuntimeConfig, loaded, dialect, requestService, workAdmission, domainKernel)
	if err != nil {
		return fmt.Errorf("compose durable workflow runtime for tenant %s: %w", loaded.Name, err)
	}
	services.WorkflowDefinitions = definitionService
	services.WorkflowInstances = instanceService
	services.WorkflowStatusPolicies = domainKernel.WorkflowStatusPolicies
	services.WorkflowApprovalPoliciesByTable = make(map[string][]string)
	for policyID, approval := range config.WorkflowApprovals {
		if approval.SubjectKind == "" {
			continue
		}
		services.WorkflowApprovalPoliciesByTable[approval.SubjectKind] = append(services.WorkflowApprovalPoliciesByTable[approval.SubjectKind], policyID)
	}
	for table := range services.WorkflowApprovalPoliciesByTable {
		sort.Strings(services.WorkflowApprovalPoliciesByTable[table])
	}
	if definitionService != nil && instanceService != nil {
		if err := registerWorkflowTriggerConsumer(config, loaded, &services, instanceService, dialect); err != nil {
			return fmt.Errorf("register record-triggered workflow consumer for tenant %s: %w", loaded.Name, err)
		}
	}
	if err := registerRequestNotifications(config, loaded, &services, requestStore, dialect); err != nil {
		return fmt.Errorf("configure request notifications for tenant %s: %w", loaded.Name, err)
	}
	worker := webhook.NewWorker(loaded.DB, multiBus, loaded.Name, dialect)
	worker.WorkAdmission = workAdmission
	services.WebhookWorker = worker
	if err := registerWebhookDelivery(config, loaded, worker, dialect, workAdmission); err != nil {
		return fmt.Errorf("configure webhook delivery for tenant %s: %w", loaded.Name, err)
	}
	if config.CloudRelay != nil {
		relay := analytics.NewCloudRelay(multiBus, loaded.Name, *config.CloudRelay)
		services.AnalyticsRelay = relay
		if err := registerTenantWorkerStart(config, func() error { relay.Start(); return nil }); err != nil {
			return fmt.Errorf("register analytics relay for tenant %s: %w", loaded.Name, err)
		}
	}
	runner := analytics.NewRebuildRunner(loaded.DB, dialect, loaded.Name, loaded.Registry, services.WorkAdmission)
	services.AnalyticsRebuild = runner
	if err := registerTenantWorkerStart(config, func() error {
		if !loaded.StartWorker(config.ProcessContext, runner.Run) {
			return fmt.Errorf("tenant runtime is stopping")
		}
		return nil
	}); err != nil {
		return fmt.Errorf("register analytics rebuild runner for tenant %s: %w", loaded.Name, err)
	}
	if config.NATS != nil {
		services.Realtime = config.NATS
		if err := registerTenantWorkerStart(config, func() error {
			if !loaded.StartWorker(config.ProcessContext, func(ctx context.Context) {
				RunRealtimeBridge(ctx, loaded.Name, services.EventBus, config.NATS)
			}) {
				return fmt.Errorf("tenant runtime is stopping")
			}
			return nil
		}); err != nil {
			return fmt.Errorf("register realtime bridge for tenant %s: %w", loaded.Name, err)
		}
		if err := registerTenantWorkerStart(config, func() error {
			if !loaded.StartWorker(config.ProcessContext, func(ctx context.Context) {
				RunAnalyticsRebuildConsumer(ctx, loaded.Name, config.NATS, runner)
			}) {
				return fmt.Errorf("tenant runtime is stopping")
			}
			return nil
		}); err != nil {
			return fmt.Errorf("register analytics rebuild notification consumer for tenant %s: %w", loaded.Name, err)
		}
	}
	if config.OutboxEnabled {
		services.Outbox = outbox.NewSQLWriter(dialect)
		services.OutboxBacklog = &outbox.BacklogMetrics{}
		var destination contract.EventPublisher
		if config.NATS != nil {
			destination = config.NATS
		} else {
			destination = analytics.NewLocalProvider(services.EventBus)
		}
		publisher := outbox.NewPublisher(loaded.DB, destination, dialect)
		if err := registerTenantWorkerStart(config, func() error {
			if !loaded.StartWorker(config.ProcessContext, func(ctx context.Context) {
				RunOutboxPublisher(ctx, publisher, services.WorkAdmission, services.OutboxBacklog)
			}) {
				return fmt.Errorf("tenant runtime is stopping")
			}
			return nil
		}); err != nil {
			return fmt.Errorf("register outbox publisher for tenant %s: %w", loaded.Name, err)
		}
		if config.NATS != nil && services.EventBus != nil {
			if err := registerTenantWorkerStart(config, func() error {
				if !loaded.StartWorker(config.ProcessContext, func(ctx context.Context) {
					RunNATSOutboxSideEffects(ctx, loaded.Name, config.NATS, services.EventBus)
				}) {
					return fmt.Errorf("tenant runtime is stopping")
				}
				return nil
			}); err != nil {
				return fmt.Errorf("register outbox side effects for tenant %s: %w", loaded.Name, err)
			}
		}
	}
	activeConfigCtx, cancelActiveConfig := context.WithTimeout(config.ProcessContext, 30*time.Second)
	var activeConfig *doctype.ConfigSnapshot
	err = withTenantWorkAdmission(activeConfigCtx, workAdmission, "load active scheduled job definitions", func() error {
		var loadErr error
		activeConfig, _, loadErr = configstore.NewStore(loaded.DB, dialect).LoadActiveSnapshotContext(activeConfigCtx, loaded.Name)
		return loadErr
	})
	cancelActiveConfig()
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("load active scheduled job definitions for tenant %s: %w", loaded.Name, err)
	}
	jobs := make([]*scheduler.JobConfig, 0)
	if activeConfig != nil && len(activeConfig.ScheduledJobs) > 0 {
		jobs = make([]*scheduler.JobConfig, 0, len(activeConfig.ScheduledJobs))
		for _, definition := range activeConfig.ScheduledJobs {
			var jobConfig map[string]any
			if len(definition.Config) > 0 {
				if err := json.Unmarshal(definition.Config, &jobConfig); err != nil {
					return fmt.Errorf("decode scheduled job %q config: %w", definition.Name, err)
				}
			}
			scope := definition.Scope
			if scope == nil {
				defaultScope := contract.BusinessScope{TenantID: contract.TenantID(loaded.SiteID), Kind: contract.ScopeKindTenant}
				scope = &defaultScope
			}
			jobs = append(jobs, &scheduler.JobConfig{Name: definition.Name, OwnerModuleID: definition.OwnerModuleID, Type: scheduler.JobType(definition.Type), Schedule: definition.Schedule, Config: jobConfig, Scope: scope, RequiredResource: definition.RequiredResource, RequiredOperation: definition.RequiredOperation})
		}
	}
	tenantScheduler, err := newTenantScheduler(loaded, dialect, jobs, domainKernel, services.ModuleBindings, config.SchedulerHandlerFactory, config.SchedulerExecutionResolverFactory, config.Mailer, services.Outbox, config.ScriptRunner, services.ScriptStore, services.WorkAdmission, worker)
	if err != nil {
		return fmt.Errorf("construct scheduler for tenant %s: %w", loaded.Name, err)
	}
	if tenantScheduler != nil {
		services.Scheduler = tenantScheduler
		if err := registerTenantWorkerStart(config, func() error {
			if !loaded.StartWorker(config.ProcessContext, func(ctx context.Context) {
				tenantScheduler.StartContext(ctx)
				<-ctx.Done()
				stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := tenantScheduler.StopContext(stopCtx); err != nil {
					slog.Error("tenant scheduler did not stop cleanly", "tenant", loaded.Name, "error", err)
				}
			}) {
				return fmt.Errorf("tenant runtime is stopping")
			}
			return nil
		}); err != nil {
			return fmt.Errorf("register scheduler for tenant %s: %w", loaded.Name, err)
		}
	}
	loaded.RuntimeServices = &services
	if workflowWorker != nil {
		if err := registerTenantWorkerStart(config, func() error {
			if !loaded.StartWorker(config.ProcessContext, func(ctx context.Context) {
				if err := workflowWorker.Run(ctx); err != nil {
					slog.Warn("durable workflow worker stopped with error", "tenant_id", loaded.SiteID, "error", err)
				}
			}) {
				return fmt.Errorf("tenant runtime is stopping")
			}
			return nil
		}); err != nil {
			return fmt.Errorf("register workflow execution worker for tenant %s: %w", loaded.Name, err)
		}
	}
	if err := registerTenantWorkerStart(config, func() error {
		return StartHumanWaitRecoveryWorker(loaded, config.ProcessContext)
	}); err != nil {
		return fmt.Errorf("register workflow request recovery: %w", err)
	}
	return nil
}

func filterProviderSelections(selections []contract.ProviderSelection, scope contract.BusinessScope) []contract.ProviderSelection {
	filtered := make([]contract.ProviderSelection, 0, len(selections))
	for _, selection := range selections {
		if selection.Key.Scope == scope {
			filtered = append(filtered, selection)
		}
	}
	return filtered
}

func contractProviderSelections(selections []plugin.ProviderSelection) []contract.ProviderSelection {
	result := make([]contract.ProviderSelection, 0, len(selections))
	for _, selection := range selections {
		result = append(result, contract.ProviderSelection{
			Key:        contract.BindingKey{Scope: selection.Key.Scope, ConsumerModuleID: selection.Key.ConsumerModuleID, CapabilityName: selection.Key.CapabilityName},
			ProviderID: selection.ProviderID,
		})
	}
	return result
}

func pluginProviderSelections(selections []contract.ProviderSelection) []plugin.ProviderSelection {
	result := make([]plugin.ProviderSelection, 0, len(selections))
	for _, selection := range selections {
		result = append(result, plugin.ProviderSelection{
			Key:        plugin.BindingKey{Scope: selection.Key.Scope, ConsumerModuleID: selection.Key.ConsumerModuleID, CapabilityName: selection.Key.CapabilityName},
			ProviderID: selection.ProviderID,
		})
	}
	return result
}

func mergeProviderSelections(tenant contract.TenantID, existing, incoming []contract.ProviderSelection) ([]contract.ProviderSelection, error) {
	byKey := make(map[contract.BindingKey]string, len(existing)+len(incoming))
	for _, selection := range existing {
		if selection.Key.Scope.TenantID != tenant {
			return nil, fmt.Errorf("persisted provider selection crosses tenant")
		}
		if err := selection.Validate(); err != nil {
			return nil, err
		}
		byKey[selection.Key] = selection.ProviderID
	}
	seenIncoming := make(map[contract.BindingKey]bool, len(incoming))
	for _, selection := range incoming {
		if selection.Key.Scope.TenantID != tenant {
			return nil, fmt.Errorf("configured provider selection crosses tenant")
		}
		if err := selection.Validate(); err != nil {
			return nil, err
		}
		if seenIncoming[selection.Key] {
			return nil, fmt.Errorf("configured provider selection is duplicated")
		}
		seenIncoming[selection.Key] = true
		if provider, exists := byKey[selection.Key]; exists && provider != selection.ProviderID {
			return nil, fmt.Errorf("configured provider selection conflicts with the pinned provider")
		}
		byKey[selection.Key] = selection.ProviderID
	}
	merged := make([]contract.ProviderSelection, 0, len(byKey))
	for key, providerID := range byKey {
		merged = append(merged, contract.ProviderSelection{Key: key, ProviderID: providerID})
	}
	sort.Slice(merged, func(i, j int) bool {
		a, b := merged[i].Key, merged[j].Key
		if a.Scope.TenantID != b.Scope.TenantID {
			return a.Scope.TenantID < b.Scope.TenantID
		}
		if a.Scope.Kind != b.Scope.Kind {
			return a.Scope.Kind < b.Scope.Kind
		}
		if a.Scope.LegalEntityID != b.Scope.LegalEntityID {
			return a.Scope.LegalEntityID < b.Scope.LegalEntityID
		}
		if a.ConsumerModuleID != b.ConsumerModuleID {
			return a.ConsumerModuleID < b.ConsumerModuleID
		}
		return a.CapabilityName < b.CapabilityName
	})
	return merged, nil
}

func sameProviderSelections(a, b []contract.ProviderSelection) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// registerWebhookDelivery only starts webhook effects from the durable outbox.
// KORA_OUTBOX=false is the documented opt-out for reports and event-driven
// integrations; the old in-memory listener could lose accepted events when
// its bounded fan-out queue filled or a tenant runtime was stopped.
func validateModuleExtensionSchemas(modules []contract.ModuleDescriptor, schemas workflow.PayloadSchemaRegistry) error {
	hasExtensions := false
	for _, module := range modules {
		hasExtensions = hasExtensions || len(module.ExtensionPoints) > 0 || len(module.Extensions) > 0
	}
	if !hasExtensions {
		return nil
	}
	if schemas == nil {
		return fmt.Errorf("module extension schemas require the tenant schema registry")
	}
	return contract.ValidateModuleExtensions(modules, schemas.Registered)
}

func registerWebhookDelivery(config TenantServiceConfig, loaded *knet.LoadedSite, worker *webhook.Worker, dialect kdb.Dialect, admission *dbaccess.Admission) error {
	if !config.OutboxEnabled {
		return nil
	}
	if loaded == nil || worker == nil {
		return errors.New("durable webhook delivery requires a tenant runtime and worker")
	}
	dispatcher, err := webhook.NewOutboxDispatcher(loaded.DB, dialect, worker)
	if err != nil {
		return fmt.Errorf("construct durable webhook dispatcher: %w", err)
	}
	dispatcher.WorkAdmission = admission
	return registerTenantWorkerStart(config, func() error {
		if !loaded.StartWorker(config.ProcessContext, func(ctx context.Context) {
			RunWebhookOutboxDispatcher(ctx, dispatcher, admission)
		}) {
			return errors.New("tenant runtime is stopping")
		}
		return nil
	})
}

func newTenantScheduler(loaded *knet.LoadedSite, dialect kdb.Dialect, jobs []*scheduler.JobConfig, operationKernel *kernel.Kernel, moduleBindings *plugin.BindingPlan, handlerFactory ScheduledJobHandlerFactory, executionResolverFactory ScheduledJobExecutionResolverFactory, mailer *email.Sender, outboxWriter outbox.Writer, scriptRunner script.Runner, scriptStore *script.Store, workAdmission interface {
	Acquire(context.Context) (func(), error)
}, webhookRetrier scheduler.WebhookRetrier) (*scheduler.Scheduler, error) {
	if loaded == nil || loaded.DB == nil || loaded.Registry == nil {
		return nil, nil
	}
	if loaded.SiteID == "" {
		return nil, errors.New("tenant scheduler requires stable tenant ID")
	}
	store, err := schedulersql.New(loaded.DB, dialect)
	if err != nil {
		return nil, err
	}
	ensureCtx, cancelEnsure := context.WithTimeout(context.Background(), 30*time.Second)
	err = withTenantWorkAdmission(ensureCtx, workAdmission, "ensure durable scheduler schema", func() error {
		return store.Ensure(ensureCtx)
	})
	cancelEnsure()
	if err != nil {
		return nil, fmt.Errorf("prepare durable scheduler store: %w", err)
	}
	if len(jobs) == 0 {
		return nil, nil
	}
	if mailer == nil {
		mailer = email.NewSender(&email.Config{From: "kora@localhost"})
	}
	txManager := &orm.TxManager{
		DB: loaded.DB, Registry: loaded.Registry, Dialect: dialect,
		Outbox: outboxWriter, ScriptRunner: scriptRunner,
		ScriptStore: scriptStore, SiteName: loaded.Name,
	}
	worker := scheduler.New(loaded.DB, loaded.Registry, txManager, mailer)
	worker.WebhookWorker = webhookRetrier
	worker.ScriptRunner = scriptRunner
	worker.ScriptStore = scriptStore
	worker.SiteName = loaded.Name
	worker.WorkAdmission = workAdmission
	worker.TenantID = loaded.SiteID
	worker.WorkerID = contract.NewID()
	if executionResolverFactory != nil {
		worker.ResolveExecution = executionResolverFactory(TenantJobRuntime{
			Database: loaded.DB, Registry: loaded.Registry, Dialect: dialect, Kernel: operationKernel,
			ModuleBindings: moduleBindings,
			TenantID:       loaded.SiteID, SiteName: loaded.Name, ConfigRevision: loaded.ConfigRevision,
		})
	}
	if handlerFactory != nil {
		handlers := handlerFactory(TenantJobRuntime{
			Database: loaded.DB, Registry: loaded.Registry, Dialect: dialect, Kernel: operationKernel,
			ModuleBindings: moduleBindings,
			TenantID:       loaded.SiteID, SiteName: loaded.Name, ConfigRevision: loaded.ConfigRevision,
		})
		for jobType, handler := range handlers {
			if err := worker.RegisterHandler(jobType, handler); err != nil {
				return nil, fmt.Errorf("register scheduled job handler %q: %w", jobType, err)
			}
		}
	}
	activeJobs := make([]*scheduler.JobConfig, 0, len(jobs))
	for _, job := range jobs {
		if job == nil {
			continue
		}
		if !worker.CanExecute(job.Type) {
			slog.Warn("scheduled job has no handler for tenant generation; skipping", "tenant", loaded.Name, "job", job.Name, "type", job.Type)
			continue
		}
		activeJobs = append(activeJobs, job)
	}
	if len(activeJobs) == 0 {
		return nil, nil
	}
	worker.DurableStore = store
	for _, job := range activeJobs {
		worker.RegisterJob(job)
	}
	return worker, nil
}

func withTenantWorkAdmission(ctx context.Context, admission interface {
	Acquire(context.Context) (func(), error)
}, operation string, work func() error) error {
	if ctx == nil || work == nil {
		return fmt.Errorf("%s requires context and work", operation)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if admission == nil {
		return work()
	}
	release, err := admission.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("admit %s: %w", operation, err)
	}
	defer release()
	return work()
}

func registerTenantWorkerStart(config TenantServiceConfig, start func() error) error {
	if config.RegisterWorkerStart != nil {
		return config.RegisterWorkerStart(start)
	}
	return start()
}

func resolveTenantStorage(existing storage.Backend, resolver func(*site.SiteConfig) (storage.Backend, error), config *site.SiteConfig) (storage.Backend, error) {
	if existing != nil {
		return existing, nil
	}
	return resolver(config)
}

type requestWallClock struct{}

func (requestWallClock) Now() time.Time { return time.Now().UTC() }

type requestIDs struct{}

func (requestIDs) NewID() string { return contract.NewID() }

func composeRequests(database *sql.DB, dialect kdb.Dialect, registry *doctype.Registry, factory RequestAuthorizerFactory) (*requests.Service, *requestsql.Store, error) {
	store, err := requestsql.New(database, dialect)
	if err != nil {
		return nil, nil, err
	}
	directAuthorizer, err := requestsql.NewDirectTaskAuthorizer(database, dialect)
	if err != nil {
		return nil, nil, err
	}
	var authorizer requests.Authorizer = directAuthorizer
	if factory != nil {
		authorizer, err = factory(database, dialect, registry, directAuthorizer)
		if err != nil {
			return nil, nil, fmt.Errorf("compose domain request authorization: %w", err)
		}
		if authorizer == nil {
			return nil, nil, fmt.Errorf("request authorizer factory returned nil")
		}
	}
	service, err := requests.NewService(store, authorizer, requests.ParticipantProjector{}, requests.NoResponseSchemas{}, requestWallClock{}, requestIDs{})
	if err != nil {
		return nil, nil, err
	}
	return service, store, nil
}
