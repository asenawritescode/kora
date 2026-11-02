package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/management"
	knet "github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/schema"
	"github.com/asenawritescode/kora/site"
)

type managementPackageDirectory interface {
	GetByIDContext(context.Context, string) (site.DBSiteInfo, error)
	CurrentDirectoryRevisionContext(context.Context) (uint64, error)
}

type managementPackageCoordinator interface {
	managementStateCoordinator
	Replace(context.Context, site.DBSiteInfo, uint64) error
}

type managementPackageExecutor struct {
	directory         managementPackageDirectory
	platformAdmission managementWorkAdmission
	coordinator       managementPackageCoordinator
	tenants           *knet.SiteRouter
	resolver          ManagementPackageArtifactResolver
}

func newManagementPackageExecutor(directory managementPackageDirectory, platformAdmission managementWorkAdmission, coordinator managementPackageCoordinator, tenants *knet.SiteRouter, resolver ManagementPackageArtifactResolver) (*managementPackageExecutor, error) {
	if directory == nil || platformAdmission == nil || coordinator == nil || tenants == nil || resolver == nil {
		return nil, errors.New("management package executor requires directory, admissions, runtime coordinator, tenant router, and trusted artifact resolver")
	}
	return &managementPackageExecutor{
		directory: directory, platformAdmission: platformAdmission,
		coordinator: coordinator, tenants: tenants, resolver: resolver,
	}, nil
}

func (e *managementPackageExecutor) Execute(ctx context.Context, claim management.Claim) (management.FinishResult, error) {
	if e == nil || e.directory == nil || e.coordinator == nil || e.tenants == nil || e.resolver == nil {
		return management.FinishResult{}, errors.New("management package executor is not configured")
	}
	if ctx == nil || claim.Owner == "" || claim.Fence == 0 {
		return management.FinishResult{}, errors.New("management package execution requires context and a fenced claim")
	}
	operation := claim.Operation
	if operation.Kind != management.CommandApplyPackage {
		return failed(contract.CodeValidationFailed, "operation kind is not enabled on this Engine deployment"), nil
	}
	var command contract.ApplyPackage
	if err := json.Unmarshal(operation.Command, &command); err != nil {
		return failed(contract.CodeValidationFailed, "stored package application command is invalid"), nil
	}
	if err := contract.ValidateApplyPackage(command); err != nil || command.Tenant != operation.Tenant {
		return failed(contract.CodeValidationFailed, "stored package application command failed validation or tenant binding"), nil
	}
	expectedRevision, err := strconv.ParseUint(string(command.ExpectedConfigRevision), 10, 64)
	if err != nil || expectedRevision == 0 || expectedRevision > 1<<63-1 {
		return failed(contract.CodeValidationFailed, "stored package configuration revision is invalid"), nil
	}

	incoming, _, artifactManifest, err := verifiedManagementPackage(ctx, e.resolver, command)
	if err != nil {
		var resolution managementPackageResolutionError
		if errors.As(err, &resolution) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return packageRecoveryRequired("trusted package artifact resolution needs recovery"), nil
		}
		return failed(contract.CodeValidationFailed, "package artifact failed immutable digest or Engine validation"), nil
	}
	packageConstraint := contract.InstalledPackage{PackageID: artifactManifest.PackageID, ReleaseID: artifactManifest.ReleaseID, Version: artifactManifest.Version, Digest: string(command.Digest), Requires: artifactManifest.Requires, Conflicts: artifactManifest.Conflicts}

	info, err := e.tenantInfo(ctx, string(operation.Tenant))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return failed(contract.CodeNotFound, "tenant was not found in the Engine directory"), nil
		}
		return packageRecoveryRequired("tenant directory lookup needs recovery"), nil
	}
	if info.SiteID != string(operation.Tenant) || (info.Status != "active" && info.Status != "degraded") {
		return failed(contract.CodeConflict, "package application requires the same active tenant identity"), nil
	}
	if err := e.coordinator.ActivateFromDirectory(ctx, info.SiteID); err != nil {
		return packageRecoveryRequired("tenant runtime activation needs recovery"), nil
	}
	handle, err := e.tenants.AcquireByID(ctx, info.SiteID)
	if err != nil {
		return packageRecoveryRequired("tenant runtime acquisition needs recovery"), nil
	}
	runtime := handle.Snapshot()
	if runtime == nil || runtime.DB == nil || runtime.Registry == nil || runtime.Activation.Tenant != operation.Tenant {
		handle.Release()
		return packageRecoveryRequired("tenant runtime identity or resources are inconsistent"), nil
	}
	dialect := db.Resolve(runtime.DBType)
	if dialect == nil {
		handle.Release()
		return failed(contract.CodeUnsupportedVersion, "tenant database dialect is unsupported for package activation"), nil
	}
	tenantAdmission, err := site.ConnectionAdmission(&site.SiteConfig{
		DBType: info.DBType, DBHost: info.DBHost, DBPort: info.DBPort, DBName: info.DBName,
		DBUser: info.DBUser, DBPassword: info.DBPassword,
	})
	if err != nil {
		handle.Release()
		return packageRecoveryRequired("tenant database admission needs recovery"), nil
	}
	releaseTenantWork, err := tenantAdmission.Acquire(ctx)
	if err != nil {
		handle.Release()
		return packageRecoveryRequired("tenant database admission wait needs recovery"), nil
	}
	store := configstore.NewStore(runtime.DB, dialect)
	moduleStore := configstore.NewModuleStore(runtime.DB, dialect)
	if err := moduleStore.EnsureSchema(); err != nil {
		releaseTenantWork()
		handle.Release()
		return packageRecoveryRequired("module ownership store initialization needs recovery"), nil
	}
	tenantID := contract.TenantID(operation.Tenant)
	moduleSnapshot, moduleLoadErr := moduleStore.Load(tenantID)
	if moduleLoadErr != nil {
		releaseTenantWork()
		handle.Release()
		return packageRecoveryRequired("module ownership snapshot read needs recovery"), nil
	}
	candidatePackages, packageMergeErr := mergeManagementPackageConstraints(moduleSnapshot.Packages, packageConstraint)
	if packageMergeErr != nil {
		releaseTenantWork()
		handle.Release()
		return failed(contract.CodeConflict, "verified package release differs from the installed tenant composition"), nil
	}
	if err := contract.ValidateInstalledPackages(candidatePackages); err != nil {
		releaseTenantWork()
		handle.Release()
		return failed(contract.CodeConflict, "package dependencies or conflicts do not match the installed tenant composition"), nil
	}
	currentRevision, err := configstore.CurrentConfigRevision(ctx, runtime.DB, dialect)
	if err != nil {
		releaseTenantWork()
		handle.Release()
		return packageRecoveryRequired("tenant configuration revision read needs recovery"), nil
	}
	journal, journalErr := configstore.LoadActivationOperation(runtime.DB, dialect, info.Name, string(operation.ID), "management_package")
	if journalErr != nil && !errors.Is(journalErr, sql.ErrNoRows) {
		releaseTenantWork()
		handle.Release()
		return packageRecoveryRequired("package activation journal read needs recovery"), nil
	}
	if errors.Is(journalErr, sql.ErrNoRows) {
		if currentRevision != expectedRevision {
			releaseTenantWork()
			handle.Release()
			return failed(contract.CodeConflict, "tenant configuration changed since package application was requested"), nil
		}
		currentSnapshot, collectErr := store.CollectSnapshot(runtime.Registry, info.Name)
		if collectErr != nil {
			releaseTenantWork()
			handle.Release()
			return packageRecoveryRequired("tenant configuration snapshot read needs recovery"), nil
		}
		target, mergeErr := mergeManagementPackageSnapshot(currentSnapshot, incoming)
		if mergeErr != nil {
			releaseTenantWork()
			handle.Release()
			return failed(contract.CodeConflict, "package configuration conflicts with existing tenant configuration"), nil
		}
		journal, err = configstore.EnsureActivationOperation(runtime.DB, dialect, info.Name, string(operation.ID), "management_package", target)
		if err != nil {
			releaseTenantWork()
			handle.Release()
			return packageRecoveryRequired("package activation journal creation needs recovery"), nil
		}
	}
	target, err := doctype.ParseConfig(journal.TargetSnapshot)
	if err != nil || target == nil {
		releaseTenantWork()
		handle.Release()
		return packageRecoveryRequired("stored package activation snapshot is invalid"), nil
	}
	services := runtime.Services
	// Once config and module state are committed, recovery must reconcile the
	// published runtime even if a provider is now unavailable. Provider
	// availability is an activation gate only before durable activation.
	if journal.Phase != configstore.ActivationCompleted && journal.Phase != configstore.ActivationActivated && len(target.Modules) > 0 {
		if services.ResolveCandidateModuleBindings == nil {
			releaseTenantWork()
			handle.Release()
			return failed(contract.CodeUnsupportedVersion, "tenant runtime cannot preflight module provider bindings"), nil
		}
		plan, planErr := services.ResolveCandidateModuleBindings(
			contract.BusinessScope{TenantID: tenantID, Kind: contract.ScopeKindTenant},
			target.Modules, command.ProviderSelections, moduleSnapshot.Revision+1,
		)
		if planErr != nil || len(plan.Blockers) > 0 {
			releaseTenantWork()
			handle.Release()
			return failed(contract.CodeConflict, "package module provider bindings failed activation preflight"), nil
		}
		if err := contract.ValidateInstalledPackages(candidatePackages); err != nil {
			releaseTenantWork()
			handle.Release()
			return failed(contract.CodeConflict, "candidate package set failed composition preflight"), nil
		}
		resolved, selectionErr := plan.ProviderSelections()
		if selectionErr != nil {
			releaseTenantWork()
			handle.Release()
			return failed(contract.CodeConflict, "package module provider selections are invalid"), nil
		}
		pinnedAndRequested, mergeErr := mergeProviderSelections(tenantID, moduleSnapshot.ProviderSelections, command.ProviderSelections)
		if mergeErr == nil {
			command.ProviderSelections, planErr = mergeProviderSelections(tenantID, pinnedAndRequested, resolved)
		} else {
			planErr = mergeErr
		}
		if planErr != nil {
			releaseTenantWork()
			handle.Release()
			return failed(contract.CodeConflict, "package module provider selections conflict with pinned bindings"), nil
		}
	}
	if err := configstore.RecordManagementActivationFence(ctx, runtime.DB, dialect, info.Name, string(operation.ID), claim.Fence, expectedRevision, command.Digest); err != nil {
		releaseTenantWork()
		handle.Release()
		return packageRecoveryRequired("activation worker fence persistence needs recovery"), nil
	}

	if journal.Phase == configstore.ActivationCompleted || journal.Phase == configstore.ActivationActivated {
		if currentRevision < expectedRevision+1 {
			releaseTenantWork()
			handle.Release()
			return packageRecoveryRequired("previous package activation requires journal recovery"), nil
		}
		observed := strconv.FormatUint(currentRevision, 10)
		releaseTenantWork()
		handle.Release()
		if err := e.reconcileRuntime(ctx, info); err != nil {
			return packageRecoveryRequired("tenant runtime convergence needs recovery"), nil
		}
		if err := e.complete(ctx, info, string(operation.ID), claim.Fence, expectedRevision, command.Digest); err != nil {
			return packageRecoveryRequired("package activation completion record needs recovery"), nil
		}
		return management.FinishResult{State: contract.OperationSucceeded, ObservedRevision: contract.Revision(observed)}, nil
	}
	if currentRevision != expectedRevision {
		releaseTenantWork()
		handle.Release()
		return failed(contract.CodeConflict, "tenant configuration revision no longer matches package request"), nil
	}
	if journal.Phase == configstore.ActivationRequested {
		if err := configstore.UpdateActivationOperation(runtime.DB, dialect, info.Name, string(operation.ID), "management_package", configstore.ActivationSnapshot, "", "replan-from-live-schema", ""); err != nil {
			releaseTenantWork()
			handle.Release()
			return packageRecoveryRequired("package activation snapshot phase needs recovery"), nil
		}
	}
	ddl, err := schema.PlanSnapshotActivation(runtime.DB, info.DBName, dialect, runtime.Registry, target, "", string(operation.ID))
	if err != nil {
		releaseTenantWork()
		handle.Release()
		return packageRecoveryRequired("package schema planning needs recovery"), nil
	}
	ddlJSON, err := json.Marshal(ddl)
	if err != nil {
		releaseTenantWork()
		handle.Release()
		return packageRecoveryRequired("package schema plan encoding needs recovery"), nil
	}
	plan := string(ddlJSON)
	if err := configstore.UpdateActivationOperation(runtime.DB, dialect, info.Name, string(operation.ID), "management_package", configstore.ActivationApplying, plan, "replan-from-live-schema", ""); err != nil {
		releaseTenantWork()
		handle.Release()
		return packageRecoveryRequired("package schema application phase needs recovery"), nil
	}
	if err := schema.ApplySnapshotActivationDDL(ctx, runtime.DB, dialect, ddl); err != nil {
		journalErr := configstore.UpdateActivationOperation(runtime.DB, dialect, info.Name, string(operation.ID), "management_package", configstore.ActivationApplying, plan, "replan-from-live-schema", err.Error())
		releaseTenantWork()
		handle.Release()
		if journalErr != nil {
			return packageRecoveryRequired("package DDL failed and its recovery journal could not be updated"), fmt.Errorf("apply package DDL: %w; record activation failure: %v", err, journalErr)
		}
		return packageRecoveryRequired("package schema DDL application needs recovery"), nil
	}
	if err := configstore.UpdateActivationOperation(runtime.DB, dialect, info.Name, string(operation.ID), "management_package", configstore.ActivationValidated, plan, "replan-from-live-schema", ""); err != nil {
		releaseTenantWork()
		handle.Release()
		return packageRecoveryRequired("package schema validation phase needs recovery"), nil
	}
	tx, err := runtime.DB.BeginTx(ctx, nil)
	if err != nil {
		releaseTenantWork()
		handle.Release()
		return packageRecoveryRequired("package activation transaction could not begin"), nil
	}
	versionID, _, err := store.CommitManagementActivation(ctx, tx, target, info.Name, dialect, configstore.ManagementActivationCommit{
		OperationID: string(operation.ID), JournalKind: "management_package", FromPhase: configstore.ActivationValidated,
		RecoveryPlan: "replace-runtime-from-tenant-db", CreatedBy: string(operation.Caller),
		VersionLabel: "Apply package " + command.ArtifactRef, Fence: claim.Fence,
		ExpectedRevision: expectedRevision, ArtifactDigest: command.Digest, ModuleSnapshotRevision: moduleSnapshot.Revision,
		ModuleTenantID:     operation.Tenant,
		ProviderSelections: command.ProviderSelections,
		PackageConstraints: []contract.InstalledPackage{packageConstraint},
	})
	if err != nil {
		_ = tx.Rollback()
		releaseTenantWork()
		handle.Release()
		if errors.Is(err, configstore.ErrConfigRevisionConflict) {
			return failed(contract.CodeConflict, "tenant configuration changed during package activation"), nil
		}
		return packageRecoveryRequired("package configuration commit needs recovery"), nil
	}
	if err := tx.Commit(); err != nil {
		releaseTenantWork()
		handle.Release()
		return packageRecoveryRequired("package configuration commit outcome needs recovery"), nil
	}
	releaseTenantWork()
	handle.Release()
	if err := e.reconcileRuntime(ctx, info); err != nil {
		return packageRecoveryRequired("tenant runtime convergence needs recovery"), nil
	}
	if err := e.complete(ctx, info, string(operation.ID), claim.Fence, expectedRevision, command.Digest); err != nil {
		return packageRecoveryRequired("package activation completion record needs recovery"), nil
	}
	if versionID == "" {
		return management.FinishResult{}, errors.New("package activation committed without a config version ID")
	}
	return management.FinishResult{State: contract.OperationSucceeded, ObservedRevision: contract.Revision(strconv.FormatUint(expectedRevision+1, 10))}, nil
}

func packageRecoveryRequired(message string) management.FinishResult {
	return management.FinishResult{
		State: contract.OperationRecoveryRequired,
		Error: contract.NewError(contract.CodeDependencyUnavailable, message),
	}
}

func (e *managementPackageExecutor) tenantInfo(ctx context.Context, tenant string) (site.DBSiteInfo, error) {
	release, err := e.platformAdmission.Acquire(ctx)
	if err != nil {
		return site.DBSiteInfo{}, err
	}
	defer release()
	return e.directory.GetByIDContext(ctx, tenant)
}

func (e *managementPackageExecutor) directoryRevision(ctx context.Context) (uint64, error) {
	release, err := e.platformAdmission.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	return e.directory.CurrentDirectoryRevisionContext(ctx)
}

func (e *managementPackageExecutor) reconcileRuntime(ctx context.Context, original site.DBSiteInfo) error {
	current, err := e.tenantInfo(ctx, original.SiteID)
	if errors.Is(err, sql.ErrNoRows) {
		return e.coordinator.Retire(ctx, original.SiteID)
	}
	if err != nil {
		return err
	}
	if current.SiteID != original.SiteID || site.DataPlaneReference(current) != site.DataPlaneReference(original) {
		return fmt.Errorf("tenant directory identity or data allocation changed during package activation")
	}
	if current.Status == "active" || current.Status == "degraded" {
		revision, err := e.directoryRevision(ctx)
		if err != nil {
			return err
		}
		return e.coordinator.Replace(ctx, current, revision)
	}
	if current.Status == "suspended" || current.Status == "deleting" || current.Status == "failed" {
		return e.coordinator.Retire(ctx, original.SiteID)
	}
	return fmt.Errorf("tenant status %q cannot be reconciled after package activation", current.Status)
}

func (e *managementPackageExecutor) complete(ctx context.Context, original site.DBSiteInfo, operation string, fence, expectedRevision uint64, digest string) error {
	info, err := e.tenantInfo(ctx, original.SiteID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.SiteID != original.SiteID || site.DataPlaneReference(info) != site.DataPlaneReference(original) {
		return errors.New("tenant directory identity or data allocation changed before package activation completion")
	}
	config := &site.SiteConfig{
		DBType: info.DBType, DBHost: info.DBHost, DBPort: info.DBPort, DBName: info.DBName,
		DBUser: info.DBUser, DBPassword: info.DBPassword,
	}
	admission, err := site.ConnectionAdmission(config)
	if err != nil {
		return err
	}
	release, err := admission.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	database, err := site.ConnectContext(ctx, config)
	if err != nil {
		return err
	}
	defer database.Close()
	return configstore.CompleteManagementActivation(ctx, database, db.Resolve(info.DBType), info.Name, operation, fence, expectedRevision, digest)
}

func failed(code contract.Code, message string) management.FinishResult {
	return management.FinishResult{State: contract.OperationFailed, Error: contract.NewError(code, message)}
}

var _ management.OperationExecutor = (*managementPackageExecutor)(nil)
