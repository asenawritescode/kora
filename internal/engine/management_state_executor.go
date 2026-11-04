package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/management"
	"github.com/asenawritescode/kora/site"
)

type managementStateDirectory interface {
	SetStatusAtRevision(context.Context, string, string, uint64) (site.SiteDescriptor, error)
}

type managementStateCoordinator interface {
	ActivateFromDirectory(context.Context, string) error
	Retire(context.Context, string) error
}

// managementStateExecutor owns the Engine-side tenant traffic transition. It
// uses the stable operation command and directory revision so a lease retry
// cannot overwrite a later Cloud lifecycle decision.
type managementStateExecutor struct {
	directory   managementStateDirectory
	coordinator managementStateCoordinator
}

func newManagementStateExecutor(directory managementStateDirectory, coordinator managementStateCoordinator) (*managementStateExecutor, error) {
	if directory == nil || coordinator == nil {
		return nil, errors.New("management state executor requires the Engine directory and tenant coordinator")
	}
	return &managementStateExecutor{directory: directory, coordinator: coordinator}, nil
}

func (e *managementStateExecutor) Execute(ctx context.Context, claim management.Claim) (management.FinishResult, error) {
	if e == nil || e.directory == nil || e.coordinator == nil {
		return management.FinishResult{}, errors.New("management state executor is not configured")
	}
	operation := claim.Operation
	if claim.Owner == "" || claim.Fence == 0 {
		return management.FinishResult{}, errors.New("management state executor requires a fenced claim")
	}
	if operation.Kind != management.CommandSetTenantState {
		return management.FinishResult{
			State: contract.OperationFailed,
			Error: contract.NewError(contract.CodeValidationFailed, "operation kind is not enabled on this Engine deployment"),
		}, nil
	}
	var command contract.SetTenantState
	if err := json.Unmarshal(operation.Command, &command); err != nil {
		return management.FinishResult{
			State: contract.OperationFailed,
			Error: contract.NewError(contract.CodeValidationFailed, "stored tenant state command is invalid"),
		}, nil
	}
	if err := contract.ValidateSetTenantState(command); err != nil {
		return management.FinishResult{
			State: contract.OperationFailed,
			Error: contract.NewError(contract.CodeValidationFailed, "stored tenant state command failed validation"),
		}, nil
	}
	expected, err := strconv.ParseUint(string(command.ExpectedConfigRevision), 10, 64)
	if err != nil {
		return management.FinishResult{
			State: contract.OperationFailed,
			Error: contract.NewError(contract.CodeValidationFailed, "stored tenant state revision is invalid"),
		}, nil
	}
	descriptor, err := e.directory.SetStatusAtRevision(ctx, string(operation.Tenant), string(command.State), expected)
	if errors.Is(err, site.ErrSiteRevisionConflict) {
		return management.FinishResult{
			State: contract.OperationFailed,
			Error: contract.NewError(contract.CodeConflict, "tenant lifecycle changed since the requested revision"),
		}, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return management.FinishResult{
			State: contract.OperationFailed,
			Error: contract.NewError(contract.CodeNotFound, "tenant was not found in the Engine directory"),
		}, nil
	}
	if err != nil {
		return recoveryRequired(), nil
	}
	if command.State == contract.TenantSuspended {
		err = e.coordinator.Retire(ctx, string(operation.Tenant))
	} else {
		err = e.coordinator.ActivateFromDirectory(ctx, string(operation.Tenant))
	}
	if err != nil {
		return management.FinishResult{
			State:            contract.OperationRecoveryRequired,
			ObservedRevision: contract.Revision(strconv.FormatUint(descriptor.ConfigRevision, 10)),
			Error:            contract.NewError(contract.CodeDependencyUnavailable, "tenant state is durable but local runtime convergence needs recovery"),
		}, nil
	}
	return management.FinishResult{
		State:            contract.OperationSucceeded,
		ObservedRevision: contract.Revision(strconv.FormatUint(descriptor.ConfigRevision, 10)),
	}, nil
}

func recoveryRequired() management.FinishResult {
	return management.FinishResult{
		State: contract.OperationRecoveryRequired,
		Error: contract.NewError(contract.CodeDependencyUnavailable, "tenant state outcome is uncertain; inspect the Engine directory and reconcile this operation"),
	}
}

var _ management.OperationExecutor = (*managementStateExecutor)(nil)
