// Package management owns durable control-plane operations accepted by an
// Engine. It contains no tenant business records or deployment credentials.
package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
)

type CommandKind string

const (
	CommandProvisionTenant   CommandKind = "provision_tenant"
	CommandApplyPackage      CommandKind = "apply_package"
	CommandSetTenantState    CommandKind = "set_tenant_state"
	CommandDeleteTenant      CommandKind = "delete_tenant"
	CommandTenantInvitation  CommandKind = "tenant_invitation"
	CommandApplyModuleChange CommandKind = "apply_module_change"
)

type Operation struct {
	ID               contract.OperationID
	Tenant           contract.TenantID
	Caller           contract.PrincipalID
	IdempotencyKey   contract.IdempotencyKey
	Kind             CommandKind
	CommandDigest    string
	Command          json.RawMessage
	State            contract.OperationState
	ObservedRevision contract.Revision
	Error            *contract.Error
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type AcceptCommand struct {
	ID             contract.OperationID
	Tenant         contract.TenantID
	Caller         contract.PrincipalID
	IdempotencyKey contract.IdempotencyKey
	Kind           CommandKind
	Command        json.RawMessage
	Now            time.Time
}

type Claim struct {
	Operation  Operation
	Owner      string
	Fence      uint64
	LeaseUntil time.Time // informational; stores fence mutations using DB time.
}

type FinishResult struct {
	State            contract.OperationState
	ObservedRevision contract.Revision
	Error            *contract.Error
}

type Store interface {
	Accept(context.Context, AcceptCommand) (Operation, error)
	Get(context.Context, contract.PrincipalID, contract.OperationID) (Operation, error)
	ClaimDue(context.Context, string, uint16, time.Duration) ([]Claim, error)
	Renew(context.Context, Claim, time.Duration) (Claim, error)
	Release(context.Context, Claim) error
	Finish(context.Context, Claim, FinishResult) error
}

// DeploymentClaimStore lets a worker claim provisioning operations assigned to
// its deployment while retaining shared handling for deployment independent
// management work.
type DeploymentClaimStore interface {
	ClaimDueForDeployment(context.Context, string, uint16, time.Duration, contract.DeploymentID) ([]Claim, error)
}

// StateRecoveryStore permits an audited replay only for a tenant-state
// operation. Implementations must retain the same operation identity and
// reject every other kind because its external effects may not be replayable.
type StateRecoveryStore interface {
	RetryStateOperation(context.Context, contract.PrincipalID, contract.OperationID, contract.IdempotencyKey, string) (Operation, error)
}

// ProvisionRecoveryStore permits audited replay only for base provisioning
// commands with no package effect; implementations must recheck stored command
// identity before requeueing the same operation.
type ProvisionRecoveryStore interface {
	RetryProvisionOperation(context.Context, contract.PrincipalID, contract.OperationID, contract.IdempotencyKey, string) (Operation, error)
}

// PackageRecoveryStore permits an audited retry of the same immutable package
// command. The Engine executor must verify the original artifact digest and
// resume the durable activation journal; it must never replace the command.
type PackageRecoveryStore interface {
	RetryPackageOperation(context.Context, contract.PrincipalID, contract.OperationID, contract.IdempotencyKey, string) (Operation, error)
}

// DeleteRecoveryStore permits an audited retry of a deletion only; the
// executor must reverify the stored checkpoint and current allocation before
// repeating teardown.
type DeleteRecoveryStore interface {
	RetryDeleteOperation(context.Context, contract.PrincipalID, contract.OperationID, contract.IdempotencyKey, string) (Operation, error)
}

// InvitationRecoveryStore records an operator-confirmed delivery outcome and
// requeues only the original uncertain invitation operation.
type InvitationRecoveryStore interface {
	RetryInvitationOperation(context.Context, contract.PrincipalID, contract.OperationID, contract.IdempotencyKey, contract.InvitationDeliveryOutcome, string) (Operation, error)
}

type IDSource interface{ NewID() string }

type Service struct {
	store Store
	ids   IDSource
	now   func() time.Time
}

func NewService(store Store, ids IDSource, now func() time.Time) (*Service, error) {
	if store == nil || ids == nil {
		return nil, fmt.Errorf("management service requires store and ID source")
	}
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, ids: ids, now: now}, nil
}

// Accept validates a typed management command, then persists its idempotent
// operation before returning. Callers must authenticate and authorize before
// invoking it; caller identity is passed separately from the request body.
func (s *Service) Accept(ctx context.Context, caller contract.PrincipalID, command any) (Operation, error) {
	if s == nil || s.store == nil || s.ids == nil {
		return Operation{}, fmt.Errorf("management service is not configured")
	}
	var tenant contract.TenantID
	var key contract.IdempotencyKey
	var kind CommandKind
	switch value := command.(type) {
	case contract.ProvisionTenant:
		if err := contract.ValidateProvisionTenant(value); err != nil {
			return Operation{}, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		tenant, key, kind = value.Tenant, value.Meta.IdempotencyKey, CommandProvisionTenant
	case contract.ApplyPackage:
		if err := contract.ValidateApplyPackage(value); err != nil {
			return Operation{}, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		tenant, key, kind = value.Tenant, value.Meta.IdempotencyKey, CommandApplyPackage
	case contract.SetTenantState:
		if err := contract.ValidateSetTenantState(value); err != nil {
			return Operation{}, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		// Tenant identity comes from the authenticated route and is attached by
		// the API adapter, so this command is accepted through AcceptForTenant.
		return Operation{}, contract.NewError(contract.CodeValidationFailed, "state operation requires tenant route scope")
	case contract.DeleteTenant:
		if err := contract.ValidateDeleteTenant(value); err != nil {
			return Operation{}, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		return Operation{}, contract.NewError(contract.CodeValidationFailed, "delete operation requires tenant route scope")
	default:
		return Operation{}, contract.NewError(contract.CodeValidationFailed, "unsupported management command")
	}
	return s.accept(ctx, caller, tenant, key, kind, command)
}

// AcceptForTenant accepts tenant-route commands whose tenant is derived from
// the authenticated URL rather than duplicated in their JSON bodies.
func (s *Service) AcceptForTenant(ctx context.Context, caller contract.PrincipalID, tenant contract.TenantID, command any) (Operation, error) {
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return Operation{}, contract.NewError(contract.CodeValidationFailed, "invalid tenant route identity")
	}
	var key contract.IdempotencyKey
	var kind CommandKind
	switch value := command.(type) {
	case contract.SetTenantState:
		if err := contract.ValidateSetTenantState(value); err != nil {
			return Operation{}, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		key, kind = value.Meta.IdempotencyKey, CommandSetTenantState
	case contract.DeleteTenant:
		if err := contract.ValidateDeleteTenant(value); err != nil {
			return Operation{}, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		key, kind = value.Meta.IdempotencyKey, CommandDeleteTenant
	case contract.RequestTenantInvitation:
		if err := contract.ValidateRequestTenantInvitation(value); err != nil {
			return Operation{}, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		key, kind = value.Meta.IdempotencyKey, CommandTenantInvitation
	case contract.ApplyModuleChange:
		if err := contract.ValidateApplyModuleChange(value); err != nil {
			return Operation{}, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		key, kind = value.Meta.IdempotencyKey, CommandApplyModuleChange
	default:
		return Operation{}, contract.NewError(contract.CodeValidationFailed, "command is not valid for a tenant route")
	}
	return s.accept(ctx, caller, tenant, key, kind, command)
}

func (s *Service) accept(ctx context.Context, caller contract.PrincipalID, tenant contract.TenantID, key contract.IdempotencyKey, kind CommandKind, command any) (Operation, error) {
	if ctx == nil {
		return Operation{}, fmt.Errorf("management operation requires context")
	}
	if _, err := contract.ParsePrincipalID(string(caller)); err != nil {
		return Operation{}, contract.NewError(contract.CodeUnauthenticated, "authenticated management caller is required")
	}
	payload, err := json.Marshal(command)
	if err != nil {
		return Operation{}, fmt.Errorf("encode management command: %w", err)
	}
	if len(payload) == 0 || len(payload) > 1<<20 {
		return Operation{}, contract.NewError(contract.CodeValidationFailed, "management command exceeds 1 MiB")
	}
	id, err := contract.ParseOperationID(s.ids.NewID())
	if err != nil {
		return Operation{}, fmt.Errorf("generate management operation ID: %w", err)
	}
	return s.store.Accept(ctx, AcceptCommand{ID: id, Tenant: tenant, Caller: caller, IdempotencyKey: key, Kind: kind, Command: payload, Now: s.now().UTC()})
}

func (o Operation) View() contract.ManagedOperation {
	return contract.ManagedOperation{ID: o.ID, Tenant: o.Tenant, State: o.State, ObservedRevision: o.ObservedRevision, Error: o.Error}
}

func (s *Service) Get(ctx context.Context, caller contract.PrincipalID, id contract.OperationID) (Operation, error) {
	if s == nil || s.store == nil {
		return Operation{}, fmt.Errorf("management service is not configured")
	}
	if _, err := contract.ParsePrincipalID(string(caller)); err != nil {
		return Operation{}, contract.NewError(contract.CodeUnauthenticated, "authenticated management caller is required")
	}
	if _, err := contract.ParseOperationID(string(id)); err != nil {
		return Operation{}, contract.NewError(contract.CodeValidationFailed, "invalid operation ID")
	}
	return s.store.Get(ctx, caller, id)
}

// ReconcileTenantState records an operator reason and returns the same durable
// operation to the requested queue. The store enforces caller ownership,
// operation kind/state, and idempotent recovery submission transactionally.
func (s *Service) ReconcileTenantState(ctx context.Context, caller contract.PrincipalID, id contract.OperationID, command contract.ReconcileTenantStateOperation) (Operation, error) {
	if s == nil || s.store == nil {
		return Operation{}, fmt.Errorf("management service is not configured")
	}
	if ctx == nil {
		return Operation{}, fmt.Errorf("management reconciliation requires context")
	}
	if _, err := contract.ParsePrincipalID(string(caller)); err != nil {
		return Operation{}, contract.NewError(contract.CodeUnauthenticated, "authenticated management caller is required")
	}
	if _, err := contract.ParseOperationID(string(id)); err != nil {
		return Operation{}, contract.NewError(contract.CodeValidationFailed, "invalid management operation ID")
	}
	if err := contract.ValidateReconcileTenantStateOperation(command); err != nil {
		return Operation{}, contract.NewError(contract.CodeValidationFailed, err.Error())
	}
	store, ok := s.store.(StateRecoveryStore)
	if !ok {
		return Operation{}, contract.NewError(contract.CodeDependencyUnavailable, "tenant-state operation recovery is unavailable")
	}
	return store.RetryStateOperation(ctx, caller, id, command.Meta.IdempotencyKey, command.Reason)
}

func (s *Service) ReconcileTenantProvision(ctx context.Context, caller contract.PrincipalID, id contract.OperationID, command contract.ReconcileTenantProvisionOperation) (Operation, error) {
	if s == nil || s.store == nil {
		return Operation{}, fmt.Errorf("management service is not configured")
	}
	if ctx == nil {
		return Operation{}, fmt.Errorf("management reconciliation requires context")
	}
	if _, err := contract.ParsePrincipalID(string(caller)); err != nil {
		return Operation{}, contract.NewError(contract.CodeUnauthenticated, "authenticated management caller is required")
	}
	if _, err := contract.ParseOperationID(string(id)); err != nil {
		return Operation{}, contract.NewError(contract.CodeValidationFailed, "invalid management operation ID")
	}
	if err := contract.ValidateReconcileTenantProvisionOperation(command); err != nil {
		return Operation{}, contract.NewError(contract.CodeValidationFailed, err.Error())
	}
	store, ok := s.store.(ProvisionRecoveryStore)
	if !ok {
		return Operation{}, contract.NewError(contract.CodeDependencyUnavailable, "tenant provisioning operation recovery is unavailable")
	}
	return store.RetryProvisionOperation(ctx, caller, id, command.Meta.IdempotencyKey, command.Reason)
}

func (s *Service) ReconcileTenantPackage(ctx context.Context, caller contract.PrincipalID, id contract.OperationID, command contract.ReconcileTenantPackageOperation) (Operation, error) {
	if s == nil || s.store == nil {
		return Operation{}, fmt.Errorf("management service is not configured")
	}
	if ctx == nil {
		return Operation{}, fmt.Errorf("management reconciliation requires context")
	}
	if _, err := contract.ParsePrincipalID(string(caller)); err != nil {
		return Operation{}, contract.NewError(contract.CodeUnauthenticated, "authenticated management caller is required")
	}
	if _, err := contract.ParseOperationID(string(id)); err != nil {
		return Operation{}, contract.NewError(contract.CodeValidationFailed, "invalid management operation ID")
	}
	if err := contract.ValidateReconcileTenantPackageOperation(command); err != nil {
		return Operation{}, contract.NewError(contract.CodeValidationFailed, err.Error())
	}
	store, ok := s.store.(PackageRecoveryStore)
	if !ok {
		return Operation{}, contract.NewError(contract.CodeDependencyUnavailable, "package application recovery is unavailable")
	}
	return store.RetryPackageOperation(ctx, caller, id, command.Meta.IdempotencyKey, command.Reason)
}

func (s *Service) ReconcileTenantDelete(ctx context.Context, caller contract.PrincipalID, id contract.OperationID, command contract.ReconcileTenantDeleteOperation) (Operation, error) {
	if s == nil || s.store == nil {
		return Operation{}, fmt.Errorf("management service is not configured")
	}
	if ctx == nil {
		return Operation{}, fmt.Errorf("management reconciliation requires context")
	}
	if _, err := contract.ParsePrincipalID(string(caller)); err != nil {
		return Operation{}, contract.NewError(contract.CodeUnauthenticated, "authenticated management caller is required")
	}
	if _, err := contract.ParseOperationID(string(id)); err != nil {
		return Operation{}, contract.NewError(contract.CodeValidationFailed, "invalid management operation ID")
	}
	if err := contract.ValidateReconcileTenantDeleteOperation(command); err != nil {
		return Operation{}, contract.NewError(contract.CodeValidationFailed, err.Error())
	}
	store, ok := s.store.(DeleteRecoveryStore)
	if !ok {
		return Operation{}, contract.NewError(contract.CodeDependencyUnavailable, "tenant deletion recovery is unavailable")
	}
	return store.RetryDeleteOperation(ctx, caller, id, command.Meta.IdempotencyKey, command.Reason)
}

func (s *Service) ReconcileTenantInvitation(ctx context.Context, caller contract.PrincipalID, id contract.OperationID, command contract.ReconcileTenantInvitationOperation) (Operation, error) {
	if s == nil || s.store == nil {
		return Operation{}, fmt.Errorf("management service is not configured")
	}
	if ctx == nil {
		return Operation{}, contract.NewError(contract.CodeValidationFailed, "management reconciliation requires context")
	}
	if _, err := contract.ParsePrincipalID(string(caller)); err != nil {
		return Operation{}, contract.NewError(contract.CodeUnauthenticated, "authenticated management caller is required")
	}
	if _, err := contract.ParseOperationID(string(id)); err != nil {
		return Operation{}, contract.NewError(contract.CodeValidationFailed, "invalid management operation ID")
	}
	if err := contract.ValidateReconcileTenantInvitationOperation(command); err != nil {
		return Operation{}, contract.NewError(contract.CodeValidationFailed, err.Error())
	}
	store, ok := s.store.(InvitationRecoveryStore)
	if !ok {
		return Operation{}, contract.NewError(contract.CodeDependencyUnavailable, "tenant invitation recovery is unavailable")
	}
	return store.RetryInvitationOperation(ctx, caller, id, command.Meta.IdempotencyKey, command.Outcome, command.Reason)
}

func Digest(command []byte) string {
	canonical, err := canonicalJSON(command)
	if err != nil {
		return ""
	}
	value := sha256.Sum256(canonical)
	return hex.EncodeToString(value[:])
}

func canonicalJSON(command []byte) ([]byte, error) {
	var decoded any
	decoder := json.NewDecoder(strings.NewReader(string(command)))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("trailing JSON value")
		}
		return nil, err
	}
	return json.Marshal(decoded)
}

func CommandDigest(kind CommandKind, command []byte) string {
	canonical, err := canonicalJSON(command)
	if kind == "" || err != nil {
		return ""
	}
	payload, err := json.Marshal(struct {
		Kind    CommandKind     `json:"kind"`
		Command json.RawMessage `json:"command"`
	}{Kind: kind, Command: canonical})
	if err != nil {
		return ""
	}
	value := sha256.Sum256(payload)
	return hex.EncodeToString(value[:])
}
