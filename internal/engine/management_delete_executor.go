package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/management"
	"github.com/asenawritescode/kora/site"
)

// VerifiedCheckpoint is returned by a trusted backup-provider adapter. Engine
// still compares every identity and readiness field before allowing teardown.
type VerifiedCheckpoint struct {
	Reference    string
	TenantID     string
	DataPlaneRef string
	Complete     bool
	Immutable    bool
	Restorable   bool
}

// CheckpointVerifier resolves an opaque provider reference and verifies it
// against Engine's current tenant allocation. Implementations must be safe for
// concurrent calls and honor context cancellation and deadlines.
type CheckpointVerifier interface {
	Verify(context.Context, string, string, string) (VerifiedCheckpoint, error)
}

type managementTenantAllocationDirectory interface {
	GetByIDContext(context.Context, string) (site.DBSiteInfo, error)
	SetDeletingAtAllocation(context.Context, string, uint64, string) (site.SiteDescriptor, error)
}

type managementTenantDelete func(context.Context, site.DBSiteInfo) error

type managementDeleteExecutor struct {
	directory   managementTenantAllocationDirectory
	verifier    CheckpointVerifier
	coordinator managementStateCoordinator
	delete      managementTenantDelete
	admission   managementWorkAdmission
}

func newManagementDeleteExecutor(directory managementTenantAllocationDirectory, verifier CheckpointVerifier, coordinator managementStateCoordinator, admission managementWorkAdmission, deleteTenant managementTenantDelete) (*managementDeleteExecutor, error) {
	if directory == nil || verifier == nil || coordinator == nil || admission == nil || deleteTenant == nil {
		return nil, fmt.Errorf("management delete executor requires Engine allocation directory, checkpoint verifier, tenant coordinator, bounded admission, and tenant deleter")
	}
	return &managementDeleteExecutor{directory: directory, verifier: verifier, coordinator: coordinator, admission: admission, delete: deleteTenant}, nil
}

func (e *managementDeleteExecutor) Execute(ctx context.Context, claim management.Claim) (management.FinishResult, error) {
	failed := func(code contract.Code, message string) management.FinishResult {
		return management.FinishResult{State: contract.OperationFailed, Error: contract.NewError(code, message)}
	}
	if e == nil || e.directory == nil || e.verifier == nil || e.coordinator == nil || e.admission == nil || e.delete == nil {
		return management.FinishResult{}, fmt.Errorf("management delete executor is not configured")
	}
	operation := claim.Operation
	if claim.Owner == "" || claim.Fence == 0 {
		return management.FinishResult{}, fmt.Errorf("management delete executor requires a fenced claim")
	}
	if ctx == nil {
		return management.FinishResult{}, fmt.Errorf("management delete requires context")
	}
	if operation.Kind != management.CommandDeleteTenant {
		return failed(contract.CodeValidationFailed, "operation kind is not enabled on this Engine deployment"), nil
	}
	var command contract.DeleteTenant
	if err := json.Unmarshal(operation.Command, &command); err != nil {
		return failed(contract.CodeValidationFailed, "stored tenant delete command is invalid"), nil
	}
	if err := contract.ValidateDeleteTenant(command); err != nil {
		return failed(contract.CodeValidationFailed, "stored tenant delete command failed validation"), nil
	}
	release, err := e.admission.Acquire(ctx)
	if err != nil {
		return management.FinishResult{}, err
	}
	defer release()
	info, err := e.directory.GetByIDContext(ctx, string(operation.Tenant))
	if err != nil {
		return recoveryRequired(), nil
	}
	if info.SiteID != string(operation.Tenant) || strings.TrimSpace(info.Name) == "" {
		return failed(contract.CodeConflict, "Engine tenant directory identity does not match the delete operation"), nil
	}
	allocation := site.DataPlaneReference(info)
	if allocation == "" {
		return failed(contract.CodeDependencyUnavailable, "Engine tenant data allocation cannot be verified"), nil
	}
	checkpoint, err := e.verifier.Verify(ctx, string(operation.Tenant), allocation, command.RecoveryCheckpointRef)
	if err != nil {
		return recoveryRequired(), nil
	}
	if checkpoint.Reference != command.RecoveryCheckpointRef || checkpoint.TenantID != string(operation.Tenant) || checkpoint.DataPlaneRef != allocation || !checkpoint.Complete || !checkpoint.Immutable || !checkpoint.Restorable {
		return failed(contract.CodeConflict, "recovery checkpoint does not match the current tenant allocation or is not safely restorable"), nil
	}
	if _, err := e.directory.SetDeletingAtAllocation(ctx, info.SiteID, info.ConfigRevision, allocation); err != nil {
		if errors.Is(err, site.ErrSiteAllocationConflict) || errors.Is(err, site.ErrSiteRevisionConflict) {
			return failed(contract.CodeConflict, "tenant allocation or configuration changed while verifying its recovery checkpoint"), nil
		}
		return recoveryRequired(), nil
	}
	// Stop tenant work and join its in-flight users before dropping the
	// database. The directory feed may also request retirement; Retire is
	// idempotent and joins an existing drain for this tenant generation.
	if err := e.coordinator.Retire(ctx, info.SiteID); err != nil {
		return recoveryRequired(), nil
	}
	if err := e.delete(ctx, info); err != nil {
		return recoveryRequired(), nil
	}
	return management.FinishResult{State: contract.OperationSucceeded}, nil
}

var _ management.OperationExecutor = (*managementDeleteExecutor)(nil)

type managementOperationDispatcher struct {
	state        management.OperationExecutor
	provision    management.OperationExecutor
	packageApply management.OperationExecutor
	delete       management.OperationExecutor
	invitation   management.OperationExecutor
	moduleChange management.OperationExecutor
	memberRevoke management.OperationExecutor
	memberGrant  management.OperationExecutor
}

func (d managementOperationDispatcher) Execute(ctx context.Context, claim management.Claim) (management.FinishResult, error) {
	var executor management.OperationExecutor
	switch claim.Operation.Kind {
	case management.CommandSetTenantState:
		executor = d.state
	case management.CommandProvisionTenant:
		executor = d.provision
	case management.CommandApplyPackage:
		executor = d.packageApply
	case management.CommandDeleteTenant:
		executor = d.delete
	case management.CommandTenantInvitation:
		executor = d.invitation
	case management.CommandApplyModuleChange:
		executor = d.moduleChange
	case management.CommandTenantMemberRevoke:
		executor = d.memberRevoke
	case management.CommandTenantMemberGrant:
		executor = d.memberGrant
	}
	if executor == nil {
		return management.FinishResult{State: contract.OperationFailed, Error: contract.NewError(contract.CodeValidationFailed, "operation kind is not enabled on this Engine deployment")}, nil
	}
	return executor.Execute(ctx, claim)
}

var _ management.OperationExecutor = managementOperationDispatcher{}
