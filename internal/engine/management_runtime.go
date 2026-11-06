package engine

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/asenawritescode/kora/api"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/management"
	"github.com/asenawritescode/kora/management/sqlstore"
	knet "github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/site"
)

type managementWorkAdmission interface {
	Acquire(context.Context) (func(), error)
}

type admittedManagementStore struct {
	store     management.Store
	admission managementWorkAdmission
}

func (s admittedManagementStore) with(ctx context.Context, run func() error) error {
	if s.admission == nil {
		return fmt.Errorf("management database admission is unavailable")
	}
	release, err := s.admission.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	return run()
}

func (s admittedManagementStore) Accept(ctx context.Context, input management.AcceptCommand) (value management.Operation, err error) {
	err = s.with(ctx, func() error { value, err = s.store.Accept(ctx, input); return err })
	return
}
func (s admittedManagementStore) Get(ctx context.Context, caller contract.PrincipalID, id contract.OperationID) (value management.Operation, err error) {
	err = s.with(ctx, func() error { value, err = s.store.Get(ctx, caller, id); return err })
	return
}
func (s admittedManagementStore) ClaimDue(ctx context.Context, owner string, limit uint16, lease time.Duration) (values []management.Claim, err error) {
	err = s.with(ctx, func() error { values, err = s.store.ClaimDue(ctx, owner, limit, lease); return err })
	return
}
func (s admittedManagementStore) ClaimDueForDeployment(ctx context.Context, owner string, limit uint16, lease time.Duration, deployment contract.DeploymentID) (values []management.Claim, err error) {
	store, ok := s.store.(management.DeploymentClaimStore)
	if !ok {
		return nil, fmt.Errorf("management store does not support deployment-scoped claims")
	}
	err = s.with(ctx, func() error {
		values, err = store.ClaimDueForDeployment(ctx, owner, limit, lease, deployment)
		return err
	})
	return
}
func (s admittedManagementStore) Renew(ctx context.Context, claim management.Claim, lease time.Duration) (value management.Claim, err error) {
	err = s.with(ctx, func() error { value, err = s.store.Renew(ctx, claim, lease); return err })
	return
}
func (s admittedManagementStore) Release(ctx context.Context, claim management.Claim) error {
	return s.with(ctx, func() error { return s.store.Release(ctx, claim) })
}
func (s admittedManagementStore) Finish(ctx context.Context, claim management.Claim, outcome management.FinishResult) error {
	return s.with(ctx, func() error { return s.store.Finish(ctx, claim, outcome) })
}
func (s admittedManagementStore) RetryStateOperation(ctx context.Context, caller contract.PrincipalID, id contract.OperationID, key contract.IdempotencyKey, reason string) (value management.Operation, err error) {
	store, ok := s.store.(management.StateRecoveryStore)
	if !ok {
		return management.Operation{}, contract.NewError(contract.CodeDependencyUnavailable, "tenant-state operation recovery is unavailable")
	}
	err = s.with(ctx, func() error { value, err = store.RetryStateOperation(ctx, caller, id, key, reason); return err })
	return
}

func (s admittedManagementStore) RetryProvisionOperation(ctx context.Context, caller contract.PrincipalID, id contract.OperationID, key contract.IdempotencyKey, reason string) (value management.Operation, err error) {
	store, ok := s.store.(management.ProvisionRecoveryStore)
	if !ok {
		return management.Operation{}, contract.NewError(contract.CodeDependencyUnavailable, "tenant provisioning operation recovery is unavailable")
	}
	err = s.with(ctx, func() error { value, err = store.RetryProvisionOperation(ctx, caller, id, key, reason); return err })
	return
}

func (s admittedManagementStore) RetryPackageOperation(ctx context.Context, caller contract.PrincipalID, id contract.OperationID, key contract.IdempotencyKey, reason string) (value management.Operation, err error) {
	store, ok := s.store.(management.PackageRecoveryStore)
	if !ok {
		return management.Operation{}, contract.NewError(contract.CodeDependencyUnavailable, "package application recovery is unavailable")
	}
	err = s.with(ctx, func() error { value, err = store.RetryPackageOperation(ctx, caller, id, key, reason); return err })
	return
}

func (s admittedManagementStore) RetryDeleteOperation(ctx context.Context, caller contract.PrincipalID, id contract.OperationID, key contract.IdempotencyKey, reason string) (value management.Operation, err error) {
	store, ok := s.store.(management.DeleteRecoveryStore)
	if !ok {
		return management.Operation{}, contract.NewError(contract.CodeDependencyUnavailable, "tenant deletion recovery is unavailable")
	}
	err = s.with(ctx, func() error { value, err = store.RetryDeleteOperation(ctx, caller, id, key, reason); return err })
	return
}

func (s admittedManagementStore) RetryInvitationOperation(ctx context.Context, caller contract.PrincipalID, id contract.OperationID, key contract.IdempotencyKey, outcome contract.InvitationDeliveryOutcome, reason string) (value management.Operation, err error) {
	store, ok := s.store.(management.InvitationRecoveryStore)
	if !ok {
		return management.Operation{}, contract.NewError(contract.CodeDependencyUnavailable, "tenant invitation recovery is unavailable")
	}
	err = s.with(ctx, func() error {
		value, err = store.RetryInvitationOperation(ctx, caller, id, key, outcome, reason)
		return err
	})
	return
}

type admittedManagementStateDirectory struct {
	directory managementStateDirectory
	admission managementWorkAdmission
}

func (d admittedManagementStateDirectory) SetStatusAtRevision(ctx context.Context, tenant, status string, revision uint64) (site.SiteDescriptor, error) {
	if d.admission == nil {
		return site.SiteDescriptor{}, fmt.Errorf("management database admission is unavailable")
	}
	release, err := d.admission.Acquire(ctx)
	if err != nil {
		return site.SiteDescriptor{}, err
	}
	defer release()
	return d.directory.SetStatusAtRevision(ctx, tenant, status, revision)
}

type managementOperationIDs struct{}

func (managementOperationIDs) NewID() string { return contract.NewID() }

type managementOperationRuntime struct {
	routes api.ManagementOperationRoutes
	worker *management.Worker
	config management.WorkerConfig
}

func assembleManagementOperationRuntime(ctx context.Context, token, caller string, database *sql.DB, dialect db.Dialect, admission managementWorkAdmission, directory managementStateDirectory, coordinator managementStateCoordinator, tenants *knet.SiteRouter, packageResolver ManagementPackageArtifactResolver, verifier CheckpointVerifier, platformDBType string, common *site.CommonConfig, deploymentID, databaseAllocationRef string) (*managementOperationRuntime, error) {
	if caller == "" {
		return nil, nil // inspection-only compatibility mode remains available.
	}
	if token == "" || database == nil || dialect == nil || admission == nil {
		return nil, fmt.Errorf("management mutations require a bearer token, platform database, dialect, and bounded work admission")
	}
	principal, err := contract.ParsePrincipalID(caller)
	if err != nil {
		return nil, fmt.Errorf("invalid configured management caller identity")
	}
	store, err := sqlstore.New(database, dialect)
	if err != nil {
		return nil, fmt.Errorf("configure management operation store: %w", err)
	}
	release, err := admission.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("admit management operation schema setup: %w", err)
	}
	err = store.EnsureSchema(ctx)
	release()
	if err != nil {
		return nil, fmt.Errorf("initialize management operation schema: %w", err)
	}
	operationStore := admittedManagementStore{store: store, admission: admission}
	service, err := management.NewService(operationStore, managementOperationIDs{}, nil)
	if err != nil {
		return nil, fmt.Errorf("configure management operation service: %w", err)
	}
	executorDirectory := admittedManagementStateDirectory{directory: directory, admission: admission}
	executor, err := newManagementStateExecutor(executorDirectory, coordinator)
	if err != nil {
		return nil, fmt.Errorf("configure management state executor: %w", err)
	}
	dispatcher := managementOperationDispatcher{state: executor}
	moduleLifecycleExecutor, err := newManagementModuleLifecycleExecutor(tenants)
	if err != nil {
		return nil, fmt.Errorf("configure module lifecycle executor: %w", err)
	}
	dispatcher.moduleChange = moduleLifecycleExecutor
	memberRevokeExecutor, err := newManagementMemberRevokeExecutor(tenantMemberAcquirer(tenants))
	if err != nil {
		return nil, fmt.Errorf("configure member revoke executor: %w", err)
	}
	dispatcher.memberRevoke = memberRevokeExecutor
	memberExecutor, err := newManagementMemberExecutor(tenantMemberAcquirer(tenants))
	if err != nil {
		return nil, fmt.Errorf("configure tenant member executor: %w", err)
	}
	dispatcher.memberGrant = memberExecutor
	capabilities := map[api.ManagementCapability]bool{
		api.ManagementStateCapability:       true,
		api.ManagementStateRecovery:         true,
		api.ManagementOperationRead:         true,
		api.ManagementModuleLifecycle:       true,
		api.ManagementMemberRevoke:          true,
		api.ManagementMemberPlanCapability:  true,
		api.ManagementMemberGrantCapability: true,
	}
	if packageResolver != nil {
		packageDirectory, ok := directory.(managementPackageDirectory)
		if !ok {
			return nil, fmt.Errorf("management package application requires the Engine SQL tenant directory")
		}
		packageCoordinator, ok := coordinator.(managementPackageCoordinator)
		if !ok {
			return nil, fmt.Errorf("management package application requires the tenant runtime coordinator")
		}
		packageExecutor, err := newManagementPackageExecutor(packageDirectory, admission, packageCoordinator, tenants, packageResolver)
		if err != nil {
			return nil, fmt.Errorf("configure management package executor: %w", err)
		}
		dispatcher.packageApply = packageExecutor
		capabilities[api.ManagementPackageCapability] = true
		capabilities[api.ManagementPackageRecovery] = true
	}
	if databaseAllocationRef != "" {
		provisionDirectory, ok := directory.(managementProvisionDirectory)
		if !ok {
			return nil, fmt.Errorf("management tenant provisioning requires the Engine SQL directory")
		}
		provisionExecutor, err := newManagementProvisionExecutor(
			databaseAllocationRef, deploymentID, database, platformDBType, common,
			admission, provisionDirectory, coordinator, site.CreateSiteContext,
		)
		if err != nil {
			return nil, fmt.Errorf("configure management provision executor: %w", err)
		}
		dispatcher.provision = provisionExecutor
		capabilities[api.ManagementProvisionCapability] = true
		capabilities[api.ManagementProvisionRecovery] = true
	}
	if verifier != nil {
		allocationDirectory, ok := directory.(managementTenantAllocationDirectory)
		if !ok {
			return nil, fmt.Errorf("management tenant deletion requires the Engine allocation directory")
		}
		if database == nil || platformDBType == "" || common == nil {
			return nil, fmt.Errorf("management tenant deletion requires the platform database and database configuration")
		}
		deleteExecutor, err := newManagementDeleteExecutor(allocationDirectory, verifier, coordinator, admission, func(ctx context.Context, info site.DBSiteInfo) error {
			return site.DeleteRegisteredSiteWithAllocation(ctx, database, platformDBType, info.SiteID, info.Name, site.DataPlaneReference(info), common)
		})
		if err != nil {
			return nil, fmt.Errorf("configure management delete executor: %w", err)
		}
		dispatcher.delete = deleteExecutor
	}
	if dispatcher.delete != nil {
		capabilities[api.ManagementDeleteCapability] = true
		capabilities[api.ManagementDeleteRecovery] = true
	}
	var operationExecutor management.OperationExecutor = dispatcher
	worker, err := management.NewWorker(operationStore, operationExecutor, slog.Default())
	if err != nil {
		return nil, fmt.Errorf("configure management operation worker: %w", err)
	}
	owner, err := os.Hostname()
	if err != nil || owner == "" {
		owner = "engine"
	}
	owner += ":" + strconv.Itoa(os.Getpid())
	return &managementOperationRuntime{
		routes: api.ManagementOperationRoutes{
			Service: service, Caller: principal, WorkerReady: true,
			Capabilities:           capabilities,
			ModuleLifecyclePlanner: moduleLifecycleExecutor,
			ModuleInspector:        moduleLifecycleExecutor,
			MemberPlanner:          memberExecutor,
		},
		worker: worker,
		config: management.WorkerConfig{Owner: owner, Deployment: contract.DeploymentID(deploymentID), Concurrency: 2, Lease: 30 * time.Second, Poll: 250 * time.Millisecond},
	}, nil
}

func (r *managementOperationRuntime) start(ctx context.Context, resources *ProcessResources) error {
	if r == nil || r.worker == nil || resources == nil {
		return fmt.Errorf("management operation runtime is incomplete")
	}
	done := make(chan struct{})
	if err := resources.RegisterWorkerStart(func() error {
		resources.ManagementWorkerDone = done
		go func() {
			defer close(done)
			if err := r.worker.Run(ctx, r.config); err != nil {
				slog.Error("management operation worker stopped", "error", err)
			}
		}()
		return nil
	}); err != nil {
		return fmt.Errorf("register management operation worker: %w", err)
	}
	return nil
}

var _ management.Store = admittedManagementStore{}
