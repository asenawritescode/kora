package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/management"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/recon"
	schedsql "github.com/asenawritescode/kora/internal/scheduler/sqlstore"
	"github.com/asenawritescode/kora/internal/tenantruntime"
)

type managementModuleLifecycleExecutor struct {
	tenants    *knet.SiteRouter
	mu         sync.RWMutex
	inspectors map[contract.ModuleID][]contract.OpenWorkInspector
}

func (e *managementModuleLifecycleExecutor) RegisterOpenWorkInspector(module contract.ModuleID, inspector contract.OpenWorkInspector) error {
	if e == nil || e.tenants == nil || module == "" || inspector == nil {
		return fmt.Errorf("module open-work inspector registration is invalid")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.inspectors == nil {
		e.inspectors = make(map[contract.ModuleID][]contract.OpenWorkInspector)
	}
	e.inspectors[module] = append(e.inspectors[module], inspector)
	return nil
}

func (e *managementModuleLifecycleExecutor) moduleInspectors(module contract.ModuleID, services tenantruntime.Services) []contract.OpenWorkInspector {
	e.mu.RLock()
	defer e.mu.RUnlock()
	inspectors := []contract.OpenWorkInspector{tenantScheduledWorkInspector{tenants: e.tenants}, tenantWorkflowWorkInspector{tenants: e.tenants}, tenantOutboxWorkInspector{tenants: e.tenants}}
	if services.BusinessReconciliation != nil && services.BusinessReconciliation.Store != nil {
		inspectors = append(inspectors, tenantBusinessReconciliationInspector{store: services.BusinessReconciliation.Store})
	}
	inspectors = append(inspectors, e.inspectors[module]...)
	for _, registered := range services.ModuleOpenWorkInspectors {
		if registered.ModuleID == module && registered.Inspector != nil {
			inspectors = append(inspectors, registered.Inspector)
		}
	}
	return inspectors
}

type tenantScheduledWorkInspector struct{ tenants *knet.SiteRouter }

func (tenantScheduledWorkInspector) OpenWorkDomain() string { return "scheduled_execution" }

func (i tenantScheduledWorkInspector) InspectOpenWork(ctx context.Context, tenant contract.TenantID, module contract.ModuleID, limit uint16) ([]contract.ModuleBlocker, error) {
	if i.tenants == nil {
		return nil, fmt.Errorf("tenant scheduler inspector is unavailable")
	}
	handle, err := i.tenants.AcquireByID(ctx, string(tenant))
	if err != nil {
		return nil, err
	}
	defer handle.Release()
	runtime := handle.Snapshot()
	if runtime == nil || runtime.DB == nil || runtime.Activation.Tenant != tenant {
		return nil, fmt.Errorf("tenant scheduler runtime identity is inconsistent")
	}
	return inspectScheduledModuleWork(ctx, runtime, tenant, module, limit)
}

func inspectScheduledModuleWork(ctx context.Context, runtime *tenantruntime.Snapshot, tenant contract.TenantID, module contract.ModuleID, limit uint16) ([]contract.ModuleBlocker, error) {
	if runtime == nil || runtime.DB == nil || runtime.Activation.Tenant != tenant {
		return nil, fmt.Errorf("tenant scheduler runtime identity is inconsistent")
	}
	dialect := db.Resolve(runtime.DBType)
	if dialect == nil {
		return nil, fmt.Errorf("tenant scheduler dialect is unsupported")
	}
	store, err := schedsql.New(runtime.DB, dialect)
	if err != nil {
		return nil, err
	}
	if err := withTenantWorkAdmission(ctx, runtime.Services.WorkAdmission, "ensure scheduler schema for module inspection", func() error {
		return store.Ensure(ctx)
	}); err != nil {
		return nil, fmt.Errorf("prepare scheduled-work inspection store: %w", err)
	}
	blockers, err := store.InspectOpenWork(ctx, tenant, module, limit)
	if err != nil {
		return nil, err
	}
	return blockers, nil
}

type tenantWorkflowWorkInspector struct{ tenants *knet.SiteRouter }

type tenantOutboxWorkInspector struct{ tenants *knet.SiteRouter }

type tenantBusinessReconciliationInspector struct{ store *recon.BusinessRunStore }

func (tenantBusinessReconciliationInspector) OpenWorkDomain() string { return "domain_obligation" }

func (i tenantBusinessReconciliationInspector) InspectOpenWork(ctx context.Context, tenant contract.TenantID, module contract.ModuleID, limit uint16) ([]contract.ModuleBlocker, error) {
	if i.store == nil {
		return nil, fmt.Errorf("business reconciliation lifecycle inspector is unavailable")
	}
	return i.store.InspectOpenWork(ctx, tenant, module, limit)
}

func (i tenantOutboxWorkInspector) InspectOpenWork(ctx context.Context, tenant contract.TenantID, module contract.ModuleID, limit uint16) ([]contract.ModuleBlocker, error) {
	if i.tenants == nil {
		return nil, fmt.Errorf("tenant outbox inspector router is unavailable")
	}
	handle, err := i.tenants.AcquireByID(ctx, string(tenant))
	if err != nil {
		return nil, err
	}
	defer handle.Release()
	runtime := handle.Snapshot()
	if runtime == nil || runtime.DB == nil || runtime.Activation.Tenant != tenant {
		return nil, fmt.Errorf("tenant outbox runtime identity is inconsistent")
	}
	dialect := db.Resolve(runtime.DBType)
	if dialect == nil {
		return nil, fmt.Errorf("tenant outbox dialect is unsupported")
	}
	return outbox.InspectOpenWork(ctx, runtime.DB, dialect, tenant, module, limit)
}

func (tenantWorkflowWorkInspector) OpenWorkDomains() []string {
	return []string{"workflow_instance", "external_outcome"}
}

func (i tenantWorkflowWorkInspector) InspectOpenWork(ctx context.Context, tenant contract.TenantID, module contract.ModuleID, limit uint16) ([]contract.ModuleBlocker, error) {
	if i.tenants == nil {
		return nil, fmt.Errorf("tenant inspector router is unavailable")
	}
	handle, err := i.tenants.AcquireByID(ctx, string(tenant))
	if err != nil {
		return nil, err
	}
	defer handle.Release()
	runtime := handle.Snapshot()
	if runtime == nil || runtime.Activation.Tenant != tenant {
		return nil, fmt.Errorf("tenant inspector runtime identity is inconsistent")
	}
	if runtime.Services.WorkflowOpenWorkInspector == nil {
		return nil, fmt.Errorf("workflow open-work inspector is unavailable")
	}
	return runtime.Services.WorkflowOpenWorkInspector.InspectOpenWork(ctx, tenant, module, limit)
}

func (e *managementModuleLifecycleExecutor) Plan(ctx context.Context, tenant contract.TenantID, request contract.PlanModuleChange) (contract.ModuleChangePlan, error) {
	if e == nil || e.tenants == nil || ctx == nil || contract.ValidatePlanModuleChange(request) != nil {
		return contract.ModuleChangePlan{}, fmt.Errorf("module lifecycle plan request is invalid")
	}
	handle, err := e.tenants.AcquireByID(ctx, string(tenant))
	if err != nil {
		return contract.ModuleChangePlan{}, err
	}
	defer handle.Release()
	runtime := handle.Snapshot()
	if runtime == nil || runtime.DB == nil || runtime.Activation.Tenant != tenant {
		return contract.ModuleChangePlan{}, fmt.Errorf("tenant runtime identity is inconsistent")
	}
	dialect := db.Resolve(runtime.DBType)
	if dialect == nil {
		return contract.ModuleChangePlan{}, fmt.Errorf("tenant database dialect is unsupported")
	}
	snapshot, err := configstore.NewModuleStore(runtime.DB, dialect).Load(tenant)
	if err != nil {
		return contract.ModuleChangePlan{}, err
	}
	if err := validateModuleProviderCutovers(ctx, runtime, snapshot, request.ProviderSelections); err != nil {
		return contract.ModuleChangePlan{}, err
	}
	return configstore.PlanModuleLifecycleChange(ctx, snapshot, request, e.moduleInspectors(request.ModuleID, runtime.Services)...)
}

func validateModuleProviderCutovers(ctx context.Context, runtime *tenantruntime.Snapshot, snapshot contract.ModuleSnapshot, changes []contract.ProviderSelection) error {
	if len(changes) == 0 {
		return nil
	}
	// Runtime snapshots expose immutable services; keep provider resolution in
	// the existing candidate-binding path used by package activation.
	if runtime == nil || runtime.Services.ResolveCandidateModuleBindings == nil {
		return fmt.Errorf("tenant runtime cannot preflight provider cutover")
	}
	byScope := map[contract.BusinessScope][]contract.ProviderSelection{}
	for _, change := range changes {
		byScope[change.Key.Scope] = append(byScope[change.Key.Scope], change)
	}
	scopes := make([]contract.BusinessScope, 0, len(byScope))
	for scope := range byScope {
		scopes = append(scopes, scope)
	}
	sort.Slice(scopes, func(i, j int) bool {
		if scopes[i].LegalEntityID != scopes[j].LegalEntityID {
			return scopes[i].LegalEntityID < scopes[j].LegalEntityID
		}
		return scopes[i].Kind < scopes[j].Kind
	})
	for _, scope := range scopes {
		byKey := map[contract.BindingKey]contract.ProviderSelection{}
		for _, selection := range snapshot.ProviderSelections {
			if selection.Key.Scope == scope {
				byKey[selection.Key] = selection
			}
		}
		for _, selection := range byScope[scope] {
			byKey[selection.Key] = selection
		}
		selections := make([]contract.ProviderSelection, 0, len(byKey))
		for _, selection := range byKey {
			selections = append(selections, selection)
		}
		plan, err := runtime.Services.ResolveCandidateModuleBindings(scope, snapshot.Packages, snapshot.Modules, nil, selections, snapshot.Revision+1)
		if err != nil || len(plan.Blockers) > 0 {
			return fmt.Errorf("provider cutover failed candidate binding validation")
		}
		resolved, err := plan.ProviderSelections()
		if err != nil {
			return fmt.Errorf("provider cutover selections are invalid")
		}
		resolvedByKey := map[contract.BindingKey]string{}
		for _, selection := range resolved {
			resolvedByKey[selection.Key] = selection.ProviderID
		}
		for _, selection := range byScope[scope] {
			if resolvedByKey[selection.Key] != selection.ProviderID {
				return fmt.Errorf("selected cutover provider is unavailable or incompatible")
			}
		}
	}
	return nil
}

func newManagementModuleLifecycleExecutor(tenants *knet.SiteRouter) (*managementModuleLifecycleExecutor, error) {
	if tenants == nil {
		return nil, fmt.Errorf("module lifecycle executor requires tenant runtime router")
	}
	return &managementModuleLifecycleExecutor{tenants: tenants}, nil
}

func (e *managementModuleLifecycleExecutor) Execute(ctx context.Context, claim management.Claim) (management.FinishResult, error) {
	failed := func(message string) (management.FinishResult, error) {
		return management.FinishResult{State: contract.OperationFailed, Error: contract.NewError(contract.CodeValidationFailed, message)}, nil
	}
	if e == nil || ctx == nil || claim.Owner == "" || claim.Fence == 0 || claim.Operation.Kind != management.CommandApplyModuleChange {
		return failed("module lifecycle operation is invalid")
	}
	var command contract.ApplyModuleChange
	if json.Unmarshal(claim.Operation.Command, &command) != nil || contract.ValidateApplyModuleChange(command) != nil {
		return failed("stored module lifecycle command is invalid")
	}
	handle, err := e.tenants.AcquireByID(ctx, string(claim.Operation.Tenant))
	if err != nil {
		return management.FinishResult{State: contract.OperationRecoveryRequired, Error: contract.NewError(contract.CodeDependencyUnavailable, "tenant runtime is unavailable for module lifecycle operation")}, nil
	}
	defer handle.Release()
	runtime := handle.Snapshot()
	if runtime == nil || runtime.DB == nil || runtime.Activation.Tenant != claim.Operation.Tenant {
		return failed("tenant runtime identity is inconsistent")
	}
	dialect := db.Resolve(runtime.DBType)
	if dialect == nil {
		return failed("tenant database dialect is unsupported")
	}
	store := configstore.NewModuleStore(runtime.DB, dialect)
	if err := store.EnsureSchema(); err != nil {
		return management.FinishResult{State: contract.OperationRecoveryRequired, Error: contract.NewError(contract.CodeDependencyUnavailable, "module snapshot store is unavailable")}, nil
	}
	tx, err := runtime.DB.BeginTx(ctx, nil)
	if err != nil {
		return management.FinishResult{State: contract.OperationRecoveryRequired, Error: contract.NewError(contract.CodeDependencyUnavailable, "module lifecycle transaction could not start")}, nil
	}
	defer tx.Rollback()
	snapshot, err := store.LoadTx(ctx, tx, claim.Operation.Tenant)
	if err != nil {
		return management.FinishResult{State: contract.OperationRecoveryRequired, Error: contract.NewError(contract.CodeDependencyUnavailable, "module snapshot could not be loaded")}, nil
	}
	if err := validateModuleProviderCutovers(ctx, runtime, snapshot, command.ProviderSelections); err != nil {
		return failed("provider cutover failed candidate binding validation")
	}
	for _, installation := range snapshot.Installations {
		if installation.ModuleID == command.ModuleID && installation.LastLifecycleOperationID == claim.Operation.ID && snapshot.Revision == command.ExpectedRevision+1 {
			if err := tx.Commit(); err != nil {
				return management.FinishResult{State: contract.OperationRecoveryRequired, Error: contract.NewError(contract.CodeDependencyUnavailable, "module lifecycle recovery confirmation is uncertain")}, nil
			}
			return management.FinishResult{State: contract.OperationSucceeded, ObservedRevision: contract.Revision(fmt.Sprint(snapshot.Revision))}, nil
		}
	}
	request := contract.PlanModuleChange{ModuleID: command.ModuleID, TargetState: command.TargetState, TargetVersion: command.TargetVersion, ProviderSelections: command.ProviderSelections, ExpectedRevision: command.ExpectedRevision}
	inspectors := e.moduleInspectors(command.ModuleID, runtime.Services)
	plan, err := configstore.PlanModuleLifecycleChange(ctx, snapshot, request, inspectors...)
	if err != nil || plan.PlanHash != command.PlanHash || len(plan.DependencyBlockers) != 0 || len(plan.OpenWorkBlockers) != 0 {
		return failed("module lifecycle plan is stale or blocked")
	}
	revision, err := store.ApplyModuleLifecycleChangeTx(ctx, tx, claim.Operation.Tenant, claim.Operation.ID, command.ExpectedRevision, plan, inspectors...)
	if err != nil {
		return failed("module lifecycle state changed; create a new plan")
	}
	if err := tx.Commit(); err != nil {
		return management.FinishResult{State: contract.OperationRecoveryRequired, Error: contract.NewError(contract.CodeDependencyUnavailable, "module lifecycle commit outcome is uncertain")}, nil
	}
	return management.FinishResult{State: contract.OperationSucceeded, ObservedRevision: contract.Revision(fmt.Sprint(revision))}, nil
}

var _ management.OperationExecutor = (*managementModuleLifecycleExecutor)(nil)
