package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/application/process"
	applicationworkflow "github.com/asenawritescode/kora/internal/application/workflow"
	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/email"
	"github.com/asenawritescode/kora/internal/eventstream"
	"github.com/asenawritescode/kora/internal/expression"
	"github.com/asenawritescode/kora/internal/kernel"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/requests"
	"github.com/asenawritescode/kora/internal/workflow"
	workflowsql "github.com/asenawritescode/kora/internal/workflow/sqlstore"
)

// WorkflowTenantConfig supplies verified policy and domain adapters to the
// tenant-owned durable orchestration runtime. Catalog construction, SQL stores,
// claim handling, and worker lifecycle remain owned by Engine.
type WorkflowTenantConfig struct {
	ModuleSnapshot   func(contract.TenantID) (contract.ModuleSnapshot, error)
	Registrations    []workflow.ActionRegistration
	LocalBuilders    map[workflow.ActionKey]process.WorkflowOperationBuilder
	Schemas          workflow.PayloadSchemaRegistry
	CapabilityLeases workflow.CapabilityLeaseManager
	Waits            workflow.WaitFieldRegistry
	// ServiceGrants are exact trusted policy grants for non-action workflow
	// operations, such as one registered human-request policy or a condition
	// subject read. Action grants are derived from ActionRegistration instead.
	ServiceGrants []policy.Grant
	// Identity is retained for source compatibility with early injected
	// workflow assemblies. Engine wraps it, checks its service/tenant/site
	// binding on each use, and replaces its grants with the trusted set above.
	// New assemblies should leave it nil so Engine mints the identity itself.
	Identity     func(contract.TenantID) (workflow.ExecutionIdentityProvider, error)
	Conditions   func(*knet.LoadedSite) workflow.ConditionScopeProvider
	Definitions  workflow.DefinitionAuthorizer
	Instances    workflow.InstanceAuthorizer
	Evaluator    expression.Evaluator
	Concurrency  uint16
	Batch        uint16
	Lease        time.Duration
	PollInterval time.Duration
	Mailer       *email.Sender
	EventBus     eventstream.Bus
}

// SelfHostedWorkflowConfig enables the domain-neutral durable workflow runtime
// for human waits, conditions, and timers. Business capabilities are supplied
// by separately registered domain integrations; the generic Engine grants no
// domain permissions and registers no domain actions by default.
func SelfHostedWorkflowConfig() *WorkflowTenantConfig {
	return &WorkflowTenantConfig{
		Schemas:   selfHostedWorkflowSchemas{},
		Waits:     selfHostedWorkflowWaitFields{},
		Evaluator: expression.NewTreeEvaluator(),
	}
}

type selfHostedWorkflowSchemas struct{}

func (selfHostedWorkflowSchemas) Registered(contract.SchemaRef) bool { return false }
func (selfHostedWorkflowSchemas) Validate(contract.SchemaRef, json.RawMessage) error {
	return fmt.Errorf("workflow payload schema is not registered")
}

type selfHostedWorkflowWaitFields struct{}

func (selfHostedWorkflowWaitFields) RegisteredCorrelationField(string, string) bool { return false }

type tenantWorkflowSchemas struct {
	base     workflow.PayloadSchemaRegistry
	registry *doctype.Registry
	mailer   bool
	bus      bool
}

func (s tenantWorkflowSchemas) Registered(ref contract.SchemaRef) bool {
	return builtinWorkflowSchema(ref) || s.base != nil && s.base.Registered(ref)
}

func (s tenantWorkflowSchemas) Validate(ref contract.SchemaRef, data json.RawMessage) error {
	if err := validateBuiltinWorkflowSchema(ref, data); err != errUnrecognizedWorkflowSchema {
		if err == nil && ref == notificationWorkflowInputSchema {
			var config struct {
				Channel string `json:"channel"`
			}
			_ = json.Unmarshal(data, &config)
			if (config.Channel == "email" || config.Channel == "both") && !s.mailer {
				return fmt.Errorf("email delivery is not configured for this workspace")
			}
			if (config.Channel == "in_app" || config.Channel == "both") && !s.bus {
				return fmt.Errorf("in-app notifications are not configured for this workspace")
			}
		}
		return err
	}
	if s.base == nil {
		return fmt.Errorf("workflow schema registry is unavailable")
	}
	return s.base.Validate(ref, data)
}

func (s tenantWorkflowSchemas) RecordTableExists(table string) bool {
	dt := s.registry.Get(table)
	return dt != nil && dt.Name == table && !dt.IsChildTable
}

func (s tenantWorkflowSchemas) RecordFieldExists(table, field string) bool {
	dt := s.registry.Get(table)
	return dt != nil && dt.Name == table && !dt.IsChildTable && dt.GetField(field) != nil
}

func (s tenantWorkflowSchemas) RecordFieldType(table, field string) (string, bool) {
	dt := s.registry.Get(table)
	if dt == nil || dt.Name != table || dt.IsChildTable {
		return "", false
	}
	definition := dt.GetField(field)
	if definition == nil {
		return "", false
	}
	if isWorkflowEmailField(definition) {
		return "Email", true
	}
	return definition.Fieldtype, true
}

func (s tenantWorkflowSchemas) RecordFieldOptions(table, field string) (string, bool) {
	if s.registry == nil {
		return "", false
	}
	dt := s.registry.Get(table)
	if dt == nil {
		return "", false
	}
	definition := dt.GetField(field)
	if definition == nil {
		return "", false
	}
	return definition.Options, true
}

func configureTenantWorkflow(ctx context.Context, cfg *WorkflowTenantConfig, loaded *knet.LoadedSite, dialect db.Dialect, requestsService *requests.Service, admission *dbaccess.Admission, domainKernel *kernel.Kernel) (*workflow.ExecutionWorker, *workflow.Service, *workflow.InstanceService, error) {
	if loaded == nil || loaded.DB == nil || dialect == nil || admission == nil {
		return nil, nil, nil, fmt.Errorf("tenant workflow requires runtime, database, dialect and active-work admission")
	}
	if domainKernel == nil {
		return nil, nil, nil, fmt.Errorf("tenant workflow requires the tenant-scoped domain kernel")
	}
	definitionStore, err := workflowsql.NewDefinitionStore(loaded.DB, dialect)
	if err != nil {
		return nil, nil, nil, err
	}
	domainKernel.WorkflowStatusPolicies = definitionStore
	instanceStore, err := workflowsql.NewInstanceStore(loaded.DB, dialect)
	if err != nil {
		return nil, nil, nil, err
	}
	if loaded.RuntimeServices != nil {
		loaded.RuntimeServices.WorkflowOpenWorkInspector = instanceStore
	}
	executionStore, err := workflowsql.NewExecutionStore(loaded.DB, dialect)
	if err != nil {
		return nil, nil, nil, err
	}
	tenant, err := contract.ParseTenantID(loaded.SiteID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("workflow stable tenant ID: %w", err)
	}
	ensureCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = withTenantWorkAdmission(ensureCtx, admission, "ensure durable workflow schema", func() error { return definitionStore.Ensure(ensureCtx) })
	cancel()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("ensure durable workflow schema: %w", err)
	}
	if cfg == nil {
		return nil, nil, nil, nil
	}
	if cfg.Schemas == nil || cfg.Waits == nil || cfg.Evaluator == nil {
		return nil, nil, nil, fmt.Errorf("workflow runtime schema and evaluator dependencies are required")
	}
	workflowSchemas := tenantWorkflowSchemas{base: cfg.Schemas, registry: loaded.Registry, mailer: cfg.Mailer != nil, bus: cfg.EventBus != nil}
	registrations := append([]workflow.ActionRegistration(nil), cfg.Registrations...)
	registrations = append(registrations, statusWorkflowRegistration())
	registrations = append(registrations, notificationWorkflowRegistration(cfg.Mailer, loaded.DB, loaded.Registry, dialect, cfg.EventBus))
	localBuilders := make(map[workflow.ActionKey]process.WorkflowOperationBuilder, len(cfg.LocalBuilders)+1)
	for key, builder := range cfg.LocalBuilders {
		localBuilders[key] = builder
	}
	localBuilders[workflow.ActionKey{Name: "record.update_status", Version: 1}] = statusWorkflowOperation
	if cfg.Mailer != nil {
		registrations = append(registrations, emailWorkflowRegistration(cfg.Mailer, loaded.DB, loaded.Registry, dialect))
	}
	serviceGrants := append([]policy.Grant(nil), cfg.ServiceGrants...)
	for _, grant := range serviceGrants {
		if !grant.Valid() {
			return nil, nil, nil, fmt.Errorf("workflow service grant %q:%q is invalid", grant.Resource, grant.Operation)
		}
	}
	allGrants := append([]policy.Grant(nil), serviceGrants...)
	actionGrants := make([]policy.Grant, 0, len(registrations))
	for _, registration := range registrations {
		if !registration.RequiredGrant.Valid() {
			return nil, nil, nil, fmt.Errorf("workflow action %s@%d has no valid required grant", registration.Key.Name, registration.Key.Version)
		}
		actionGrants = append(actionGrants, registration.RequiredGrant)
		allGrants = append(allGrants, registration.RequiredGrant)
	}
	var identity workflow.ExecutionIdentityProvider
	if cfg.Identity == nil {
		identity, err = newEngineWorkflowIdentity(tenant, loaded.Name, allGrants)
	} else {
		legacyIdentity, identityErr := cfg.Identity(tenant)
		if identityErr != nil {
			return nil, nil, nil, fmt.Errorf("assemble compatibility workflow identity: %w", identityErr)
		}
		identity, err = newScopedWorkflowIdentity(legacyIdentity, tenant, loaded.Name, allGrants)
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("assemble tenant workflow service identity: %w", err)
	}
	if identity == nil {
		return nil, nil, nil, fmt.Errorf("tenant workflow service identity is unavailable")
	}
	grantCtx, cancelGrants := context.WithTimeout(ctx, 5*time.Second)
	err = identity.RequireActionGrants(grantCtx, tenant, actionGrants)
	cancelGrants()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("verify workflow service action grants: %w", err)
	}
	for _, registration := range registrations {
		lease := registration.CapabilityLease
		if registration.Class == workflow.ActionLocal {
			lease = registration.LocalCapabilityLease
		}
		if registration.PinnedDispatcher == nil && lease == nil {
			continue
		}
		if cfg.CapabilityLeases == nil || lease == nil {
			return nil, nil, nil, fmt.Errorf("pinned workflow capability %s@%d has no durable lease manager", registration.Key.Name, registration.Key.Version)
		}
		leaseCtx, cancelLease := context.WithTimeout(ctx, 5*time.Second)
		err := withTenantWorkAdmission(leaseCtx, admission, "ensure pinned workflow capability lease", func() error {
			return cfg.CapabilityLeases.EnsureCapabilityLease(leaseCtx, *lease)
		})
		cancelLease()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("ensure workflow capability lease for %s@%d: %w", registration.Key.Name, registration.Key.Version, err)
		}
	}
	localRegistrations := make([]workflow.ActionRegistration, 0, len(registrations))
	for _, registration := range registrations {
		if registration.Class == workflow.ActionLocal {
			if registration.Local != nil || registration.External != nil {
				return nil, nil, nil, fmt.Errorf("local workflow adapters must be assembled through registered domain-operation builders")
			}
			localRegistrations = append(localRegistrations, registration)
		} else if registration.Class != workflow.ActionExternal || (registration.External == nil && registration.PinnedDispatcher == nil) || (registration.PinnedDispatcher != nil && registration.External != nil) || registration.Local != nil {
			return nil, nil, nil, fmt.Errorf("external workflow registration has an invalid adapter")
		}
	}
	transaction, err := process.NewTenantTransactionOpener(tenant, loaded.DB, admission)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("construct workflow tenant transaction opener: %w", err)
	}
	var localExecutor workflow.LocalActionExecutor
	if len(localRegistrations) > 0 {
		var localLeaseAdmission process.WorkflowCapabilityLeaseAdmission
		if admission, ok := cfg.CapabilityLeases.(process.WorkflowCapabilityLeaseAdmission); ok {
			localLeaseAdmission = admission
		}
		localExecutor, err = process.NewWorkflowLocalActionExecutor(domainKernel, loaded.Registry, transaction, executionStore, workflowSchemas, localRegistrations, localBuilders, localLeaseAdmission)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("construct workflow local domain executor: %w", err)
		}
	} else if len(localBuilders) > 0 {
		return nil, nil, nil, fmt.Errorf("workflow local builder supplied without a registered local action")
	}
	catalog := workflow.NewCatalog()
	for _, registration := range registrations {
		if registration.Class == workflow.ActionLocal {
			registration.Local = localExecutor
		}
		if err := catalog.Register(registration); err != nil {
			return nil, nil, nil, fmt.Errorf("register workflow action %s@%d: %w", registration.Key.Name, registration.Key.Version, err)
		}
	}
	if err := catalog.Freeze(); err != nil {
		return nil, nil, nil, fmt.Errorf("freeze tenant workflow action catalog: %w", err)
	}
	definitionAuthorizer := cfg.Definitions
	instanceAuthorizer := cfg.Instances
	if definitionAuthorizer == nil || instanceAuthorizer == nil {
		authorizationStore, storeErr := workflowsql.NewAuthorizationStore(loaded.DB, dialect, loaded.Registry)
		if storeErr != nil {
			return nil, nil, nil, fmt.Errorf("construct tenant workflow authorization store: %w", storeErr)
		}
		defaultAuthorizer, authErr := applicationworkflow.NewAuthorizer(authorizationStore, loaded.Registry)
		if authErr != nil {
			return nil, nil, nil, fmt.Errorf("construct tenant workflow policy: %w", authErr)
		}
		if definitionAuthorizer == nil {
			definitionAuthorizer = defaultAuthorizer
		}
		if instanceAuthorizer == nil {
			instanceAuthorizer = defaultAuthorizer
		}
	}
	definitionService, err := workflow.NewService(definitionStore, definitionAuthorizer, catalog, workflowSchemas, cfg.Waits, nil)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("construct workflow definition service: %w", err)
	}
	definitionService.SetModuleSnapshotLookup(func(tenant contract.TenantID) (contract.ModuleSnapshot, error) {
		return configstore.NewModuleStore(loaded.DB, dialect).Load(tenant)
	})
	instanceService, err := workflow.NewInstanceService(definitionStore, instanceStore, instanceAuthorizer, catalog, workflowSchemas, cfg.Waits, nil, nil)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("construct workflow instance service: %w", err)
	}
	var conditionScope workflow.ConditionScopeProvider
	if cfg.Conditions != nil {
		conditionScope = cfg.Conditions(loaded)
	} else {
		conditionScope, err = newTenantWorkflowConditionScope(loaded, dialect)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("construct tenant workflow condition scope: %w", err)
		}
	}
	if conditionScope == nil {
		return nil, nil, nil, fmt.Errorf("tenant workflow authorized condition scope is unavailable")
	}
	human := workflow.HumanWaitRuntime{Requests: requestsService, Store: executionStore}
	claimHandler, err := workflow.NewDefinitionClaimHandler(executionStore, instanceStore, definitionStore, catalog, workflowSchemas, identity, conditionScope, cfg.Evaluator, cfg.CapabilityLeases, human)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("construct tenant workflow claim handler: %w", err)
	}
	concurrency, batch, lease, poll := cfg.Concurrency, cfg.Batch, cfg.Lease, cfg.PollInterval
	if concurrency == 0 {
		concurrency = 2
	}
	if batch == 0 {
		batch = concurrency
	}
	if lease == 0 {
		lease = 30 * time.Second
	}
	if poll == 0 {
		poll = 500 * time.Millisecond
	}
	worker, err := workflow.NewExecutionWorker(executionStore, claimHandler, workflow.ExecutionWorkerConfig{Tenant: tenant, Owner: contract.NewID(), Concurrency: concurrency, Batch: batch, Lease: lease, PollInterval: poll, WorkAdmission: admission}, func(err error) {
		slog.Warn("durable workflow execution pass failed", "tenant_id", tenant, "error", err)
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("construct tenant workflow execution worker: %w", err)
	}
	return worker, definitionService, instanceService, nil
}

func newEngineWorkflowIdentity(tenant contract.TenantID, siteName string, grants []policy.Grant) (workflow.ExecutionIdentityProvider, error) {
	principal, err := contract.ParsePrincipalID("kora-workflow-runtime")
	if err != nil {
		return nil, fmt.Errorf("workflow service principal: %w", err)
	}
	if siteName == "" {
		return nil, fmt.Errorf("workflow service route is required")
	}
	return workflow.NewTenantServiceIdentityProvider(tenant, contract.ActorContext{
		PrincipalID: string(principal), PrincipalType: contract.PrincipalService,
		Site: siteName, AuthenticatedAt: time.Now().UTC(),
	}, grants)
}
