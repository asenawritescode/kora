package process

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/workflow"
)

// WorkflowOperationBuilder is trusted assembly code registered for one
// immutable action key. It translates validated action input into a domain
// command; workflow JSON cannot select a Go function or command policy.
type WorkflowOperationBuilder func(context.Context, workflow.ActionContext, workflow.ActionInput) (kernel.Operation, error)

type workflowLocalActionBinding struct {
	input, output contract.SchemaRef
	grant         policy.Grant
	lease         *contract.CapabilityLease
	build         WorkflowOperationBuilder
}

type WorkflowCapabilityLeaseAdmission interface {
	AdmitCapabilityAttemptInTx(context.Context, *sql.Tx, contract.CapabilityLease, time.Time) error
}

// WorkflowLocalStepStore is implemented by the durable workflow SQL store.
// Both methods operate on the caller-owned transaction.
type WorkflowLocalStepStore interface {
	ValidateClaimInTx(context.Context, *sql.Tx, workflow.Claim) error
	CommitStepInTx(context.Context, *sql.Tx, workflow.StepCommit) error
}

// WorkflowLocalActionExecutor executes only registered domain operations and
// commits their idempotency receipt, outbox effects, and workflow progress in
// the same tenant transaction.
type WorkflowLocalActionExecutor struct {
	engine   *kernel.Kernel
	registry *doctype.Registry
	begin    TenantTransaction
	store    WorkflowLocalStepStore
	schemas  workflow.PayloadSchemaRegistry
	bindings map[workflow.ActionKey]workflowLocalActionBinding
	leases   WorkflowCapabilityLeaseAdmission
}

func NewWorkflowLocalActionExecutor(engine *kernel.Kernel, registry *doctype.Registry, begin TenantTransaction, store WorkflowLocalStepStore, schemas workflow.PayloadSchemaRegistry, registrations []workflow.ActionRegistration, builders map[workflow.ActionKey]WorkflowOperationBuilder, leaseAdmission ...WorkflowCapabilityLeaseAdmission) (*WorkflowLocalActionExecutor, error) {
	if engine == nil || registry == nil || begin == nil || store == nil || schemas == nil {
		return nil, fmt.Errorf("workflow local action executor dependencies are required")
	}
	bindings := make(map[workflow.ActionKey]workflowLocalActionBinding, len(registrations))
	for _, registration := range registrations {
		if registration.Class != workflow.ActionLocal || registration.Key.Name == "" || registration.Key.Version == 0 || !schemas.Registered(registration.InputSchema) || !schemas.Registered(registration.OutputSchema) {
			return nil, fmt.Errorf("workflow local action registration or schemas are invalid")
		}
		builder := builders[registration.Key]
		if builder == nil {
			return nil, fmt.Errorf("workflow local action %s@%d has no trusted operation builder", registration.Key.Name, registration.Key.Version)
		}
		if !registration.RequiredGrant.Valid() {
			return nil, fmt.Errorf("workflow local action %s@%d lacks a valid required grant", registration.Key.Name, registration.Key.Version)
		}
		bindings[registration.Key] = workflowLocalActionBinding{input: registration.InputSchema, output: registration.OutputSchema, grant: registration.RequiredGrant, lease: registration.LocalCapabilityLease, build: builder}
	}
	if len(bindings) != len(builders) {
		return nil, fmt.Errorf("workflow local action builder set does not match registered actions")
	}
	var leases WorkflowCapabilityLeaseAdmission
	if len(leaseAdmission) > 0 {
		leases = leaseAdmission[0]
	}
	for _, binding := range bindings {
		if binding.lease != nil && leases == nil {
			return nil, fmt.Errorf("local module workflow action requires transactional capability lease admission")
		}
	}
	return &WorkflowLocalActionExecutor{engine: engine, registry: registry, begin: begin, store: store, schemas: schemas, bindings: bindings, leases: leases}, nil
}

func (e *WorkflowLocalActionExecutor) ExecuteLocal(ctx context.Context, claim workflow.Claim, action workflow.ActionContext, key workflow.ActionKey, input workflow.ActionInput, next contract.StepID) (workflow.ActionResult, error) {
	if e == nil || ctx == nil || !action.Execution.Valid() || action.Execution.Tenant() != claim.Tenant || action.InstanceID != claim.InstanceID || action.StepID != claim.StepID || action.ExecutionID == "" || action.IdempotencyKey == "" || next == "" {
		return workflow.ActionResult{}, &workflow.LocalActionFailure{Kind: workflow.LocalActionNotAppliedPermanent, Cause: contract.NewError(contract.CodeValidationFailed, "local workflow action identity is invalid")}
	}
	binding, ok := e.bindings[key]
	if !ok || input.Schema != binding.input || action.RequiredGrant != binding.grant || !action.RequiredGrant.Valid() {
		return workflow.ActionResult{}, &workflow.LocalActionFailure{Kind: workflow.LocalActionNotAppliedPermanent, Cause: contract.NewError(contract.CodeValidationFailed, "local workflow action is not registered for this schema")}
	}
	if !workflow.WorkflowGrantAllows(action.Execution, binding.grant, action.BusinessScope) {
		return workflow.ActionResult{}, &workflow.LocalActionFailure{Kind: workflow.LocalActionNotAppliedPermanent, Cause: contract.NewError(contract.CodePermissionDenied, "workflow service identity lacks the registered action grant")}
	}
	if err := e.schemas.Validate(binding.input, input.Data); err != nil {
		return workflow.ActionResult{}, &workflow.LocalActionFailure{Kind: workflow.LocalActionNotAppliedPermanent, Cause: contract.NewError(contract.CodeValidationFailed, "local workflow action input failed schema validation")}
	}
	operation, err := binding.build(ctx, action, input)
	if err != nil {
		return workflow.ActionResult{}, &workflow.LocalActionFailure{Kind: workflow.LocalActionNotAppliedPermanent, Cause: err}
	}
	operationContext, contextErr := verifiedWorkflowOperationContext(action, operation.Context, operation.Command)
	if contextErr != nil {
		return workflow.ActionResult{}, &workflow.LocalActionFailure{Kind: workflow.LocalActionNotAppliedPermanent, Cause: contextErr}
	}
	operation.Context = operationContext
	if binding.lease != nil {
		moduleExecution := operation.Context.ModuleExecution
		lease := binding.lease
		if moduleExecution == nil || !moduleExecution.Verified() || moduleExecution.Scope != lease.Scope || moduleExecution.TenantID != lease.TenantID || moduleExecution.CallerModuleID != lease.CallerModuleID || moduleExecution.BindingRevision != lease.BindingRevision || moduleExecution.AllowedOperation != lease.Operation {
			return workflow.ActionResult{}, &workflow.LocalActionFailure{Kind: workflow.LocalActionNotAppliedPermanent, Cause: contract.NewError(contract.CodePermissionDenied, "local workflow capability lease does not match the trusted module command")}
		}
	}
	database, tx, release, err := e.begin(ctx, claim.Tenant)
	if err != nil {
		if release != nil {
			release()
		}
		return workflow.ActionResult{}, newLocalActionFailure(err, workflow.LocalActionNotAppliedRetryable)
	}
	if database == nil || tx == nil {
		if release != nil {
			release()
		}
		return workflow.ActionResult{}, newLocalActionFailure(contract.NewError(contract.CodeDependencyUnavailable, "workflow transaction opener returned no transaction"), workflow.LocalActionNotAppliedRetryable)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
		if release != nil {
			release()
		}
	}()
	if err := e.store.ValidateClaimInTx(ctx, tx, claim); err != nil {
		return workflow.ActionResult{}, newLocalActionFailure(err, workflow.LocalActionNotAppliedRetryable)
	}
	if binding.lease != nil {
		if err := e.leases.AdmitCapabilityAttemptInTx(ctx, tx, *binding.lease, time.Now().UTC()); err != nil {
			return workflow.ActionResult{}, newLocalActionFailure(err, workflow.LocalActionNotAppliedPermanent)
		}
	}
	commandResult, afterCommit, commandErr := e.engine.ExecuteInTx(ctx, database, tx, e.registry, operation)
	if commandErr != nil {
		return workflow.ActionResult{}, newLocalActionFailure(commandErr, workflow.LocalActionNotAppliedPermanent)
	}
	if err := e.schemas.Validate(binding.output, commandResult.Data); err != nil {
		return workflow.ActionResult{}, &workflow.LocalActionFailure{Kind: workflow.LocalActionNotAppliedPermanent, Cause: contract.NewError(contract.CodeValidationFailed, "local workflow command result failed registered output schema")}
	}
	result := workflow.ActionResult{Schema: binding.output, Data: append([]byte(nil), commandResult.Data...)}
	change, err := workflow.WithExecutionEvent(workflow.StepCommit{Claim: claim, ExecutionID: action.ExecutionID, Attempt: claim.Attempt,
		IdempotencyKey: action.IdempotencyKey, Status: contract.InstanceReady, NextStep: next, Result: result}, "step_succeeded")
	if err != nil {
		return workflow.ActionResult{}, newLocalActionFailure(err, workflow.LocalActionNotAppliedPermanent)
	}
	if err := e.store.CommitStepInTx(ctx, tx, change); err != nil {
		return workflow.ActionResult{}, newLocalActionFailure(err, workflow.LocalActionNotAppliedRetryable)
	}
	if err := tx.Commit(); err != nil {
		return workflow.ActionResult{}, &workflow.LocalActionFailure{Kind: workflow.LocalActionOutcomeUnknown, Cause: err}
	}
	committed = true
	if afterCommit != nil {
		afterCommit()
	}
	return result, nil
}

func newLocalActionFailure(err error, fallback workflow.LocalActionFailureKind) *workflow.LocalActionFailure {
	kind := fallback
	var commandErr *contract.Error
	if errors.As(err, &commandErr) {
		switch commandErr.Type {
		case contract.CodeDependencyUnavailable, contract.CodeDeadlineExceeded, contract.CodeResourceExhausted:
			kind = workflow.LocalActionNotAppliedRetryable
		default:
			kind = workflow.LocalActionNotAppliedPermanent
		}
	}
	return &workflow.LocalActionFailure{Kind: kind, Cause: err}
}

func verifiedWorkflowOperationContext(action workflow.ActionContext, requested kernel.OperationContext, command string) (kernel.OperationContext, error) {
	var moduleExecution *contract.ModuleExecution
	if requested.ModuleExecution != nil {
		candidate := requested.ModuleExecution
		if !candidate.Verified() || candidate.BindingRevision == 0 || candidate.TenantID != action.Execution.Tenant() || candidate.Scope != action.BusinessScope || candidate.AuthorizedCommand != command || candidate.AllowedOperation != command {
			return kernel.OperationContext{}, contract.NewError(contract.CodePermissionDenied, "workflow module execution does not match the trusted action")
		}
		copy := *candidate
		pinned, err := copy.WithWorkflowCommandPin(action.InstanceID, action.StepID, action.IdempotencyKey)
		if err != nil {
			return kernel.OperationContext{}, contract.NewError(contract.CodePermissionDenied, "workflow module command lacks a durable action pin")
		}
		copy = pinned
		moduleExecution = &copy
	}
	actor := action.Execution.Actor()
	user := actor.SubjectUserID
	if user == "" {
		user = actor.PrincipalID
	}
	primaryRole := ""
	if len(actor.Roles) > 0 {
		primaryRole = actor.Roles[0]
	}
	return kernel.OperationContext{ModuleExecution: moduleExecution, Site: actor.Site, TenantID: action.Execution.Tenant(), Execution: action.Execution, BusinessScope: action.BusinessScope, CausationDepth: action.CausationDepth, Actor: actor,
		User: user, UserRole: primaryRole, Roles: append([]string(nil), actor.Roles...), Source: kernel.SourceWorker,
		CorrelationID: requested.CorrelationID, CausationID: string(action.InstanceID), ExpectedVersion: requested.ExpectedVersion,
		IdempotencyKey: string(action.IdempotencyKey)}, nil
}
