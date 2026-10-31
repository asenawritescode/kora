// Package workflow contains transport-neutral authorization for durable
// workflow definitions and instances. Persistence access is supplied by the
// workflow SQL store; current tenant permissions come from the active registry.
package workflow

import (
	"context"
	"fmt"
	"strconv"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/policy"
	processworkflow "github.com/asenawritescode/kora/workflow"
)

const (
	DefinitionResource = "Workflow Definition"
	InstanceResource   = "Workflow Instance"
)

// Authorizer evaluates every decision against live tenant membership. It
// never treats the role snapshot captured during authentication as current.
type Authorizer struct {
	store    processworkflow.AuthorizationStore
	registry *doctype.Registry
}

func NewAuthorizer(store processworkflow.AuthorizationStore, registry *doctype.Registry) (*Authorizer, error) {
	if store == nil || registry == nil || registry.Permissions == nil {
		return nil, fmt.Errorf("workflow authorizer requires live membership store and tenant permission registry")
	}
	return &Authorizer{store: store, registry: registry}, nil
}

var _ processworkflow.DefinitionAuthorizer = (*Authorizer)(nil)
var _ processworkflow.InstanceAuthorizer = (*Authorizer)(nil)
var _ processworkflow.WorkflowCancellationAuthorizer = (*Authorizer)(nil)

func (a *Authorizer) CanManageWorkflowDefinitions(ctx context.Context, exec policy.Execution) (bool, error) {
	roles, ok, err := a.currentHumanRoles(ctx, exec)
	if err != nil || !ok {
		return false, err
	}
	return a.allowed(roles, DefinitionResource, "write"), nil
}

func (a *Authorizer) CanStartWorkflow(ctx context.Context, exec policy.Execution, _ contract.DefinitionID, subject contract.SubjectRef) (bool, error) {
	roles, ok, err := a.currentHumanRoles(ctx, exec)
	if err != nil || !ok || !a.allowed(roles, DefinitionResource, "read") || !a.allowed(roles, InstanceResource, "create") {
		return false, err
	}
	return a.canReadSubject(ctx, exec, roles, subject, true)
}

func (a *Authorizer) CanReadWorkflow(ctx context.Context, exec policy.Execution, subject contract.SubjectRef) (bool, error) {
	roles, ok, err := a.currentHumanRoles(ctx, exec)
	if err != nil || !ok {
		return false, err
	}
	return a.canReadSubject(ctx, exec, roles, subject, false)
}

func (a *Authorizer) CanReconcileWorkflow(ctx context.Context, exec policy.Execution, instance contract.WorkflowInstance) (bool, error) {
	roles, ok, err := a.currentHumanRoles(ctx, exec)
	if err != nil || !ok || !a.allowed(roles, InstanceResource, "write") {
		return false, err
	}
	return a.canReadSubject(ctx, exec, roles, instance.Subject, false)
}

func (a *Authorizer) CanCancelWorkflow(ctx context.Context, exec policy.Execution, instance contract.WorkflowInstance) (bool, error) {
	roles, ok, err := a.currentHumanRoles(ctx, exec)
	if err != nil || !ok {
		return false, err
	}
	canCancelAny := a.allowed(roles, InstanceResource, "cancel")
	if !canCancelAny {
		starter, err := a.store.WorkflowStarter(ctx, exec.Tenant(), instance.ID)
		if err != nil {
			return false, err
		}
		canCancelAny = starter == contract.PrincipalID(exec.Actor().PrincipalID)
	}
	if !canCancelAny {
		return false, nil
	}
	return a.canReadSubject(ctx, exec, roles, instance.Subject, false)
}

func (a *Authorizer) currentHumanRoles(ctx context.Context, exec policy.Execution) ([]string, bool, error) {
	if ctx == nil || !exec.Valid() || exec.Actor().PrincipalType != contract.PrincipalHuman || exec.Actor().SubjectUserID == "" || exec.Actor().SubjectUserID != exec.Actor().PrincipalID {
		return nil, false, nil
	}
	roles, active, err := a.store.UserRoles(ctx, exec.Actor().Site, contract.PrincipalID(exec.Actor().PrincipalID))
	if err != nil || !active {
		return nil, active, err
	}
	return roles, true, nil
}

func (a *Authorizer) canReadSubject(ctx context.Context, exec policy.Execution, roles []string, subject contract.SubjectRef, requireCurrentRevision bool) (bool, error) {
	if subject.Kind == "" || subject.ID == "" {
		return false, contract.NewError(contract.CodeValidationFailed, "workflow subject reference is invalid")
	}
	allowed, _ := a.registry.Permissions.UserCan(roles, subject.Kind, "read")
	if !allowed {
		return false, nil
	}
	current, err := a.store.Subject(ctx, subject)
	if err != nil {
		return false, err
	}
	ownerOnly, assignedFields := a.registry.Permissions.RowScopeFor(roles, subject.Kind, "read")
	if ownerOnly || len(assignedFields) > 0 {
		actorID := exec.Actor().SubjectUserID
		if actorID == "" {
			actorID = exec.Actor().PrincipalID
		}
		visible := ownerOnly && string(current.Owner) == actorID
		for _, field := range assignedFields {
			if current.UserFields[field] == actorID {
				visible = true
				break
			}
		}
		if !visible {
			return false, nil
		}
	}
	if requireCurrentRevision {
		revision, err := contract.ParseRevision(string(subject.Revision))
		if err != nil {
			return false, contract.NewError(contract.CodeValidationFailed, "workflow subject revision is invalid")
		}
		requestedRevision, err := strconv.ParseUint(string(revision), 10, 64)
		currentRevision, parseErr := strconv.ParseUint(string(current.Revision), 10, 64)
		if err != nil || parseErr != nil || requestedRevision != currentRevision {
			return false, contract.NewError(contract.CodeStaleSubject, "workflow subject changed before start")
		}
	}
	return true, nil
}

func (a *Authorizer) allowed(roles []string, resource, operation string) bool {
	allowed, _ := a.registry.Permissions.UserCan(roles, resource, operation)
	return allowed
}
