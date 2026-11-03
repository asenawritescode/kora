package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/expression"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/requests"
)

// ConditionScopeProvider supplies authorized, read-only subject data for a
// pure workflow condition. It must use the supplied service identity and the
// instance's pinned subject revision, and project only fields visible under
// the tenant's current read policy.
type ConditionScopeProvider interface {
	Scope(context.Context, policy.Execution, contract.WorkflowInstance) (expression.Scope, error)
}

type WorkflowRequestPreparer interface {
	PrepareWorkflowSubmit(context.Context, policy.Execution, contract.SubmitRequest, contract.RequestSource) (*requests.PreparedSubmission, error)
}

type HumanWaitCreator interface {
	CreateHumanWait(context.Context, *requests.PreparedSubmission, StepCommit) (requests.RequestRecord, error)
}

type HumanWaitRuntime struct {
	Requests WorkflowRequestPreparer
	Store    HumanWaitCreator
	Now      func() time.Time
}

func workflowGrantAllows(execution policy.Execution, required policy.Grant, scope contract.BusinessScope) bool {
	if !required.Valid() || scope.Validate() != nil || scope.TenantID != execution.Tenant() {
		return false
	}
	if (required.Scope.TenantID != "" || required.Scope.Kind != "" || required.Scope.LegalEntityID != "") && required.Scope != scope {
		return false
	}
	if scope.Kind == contract.ScopeKindTenant {
		grant, ok := execution.GrantFor(required.Resource, required.Operation)
		return ok && grant.OwnerOnly == required.OwnerOnly
	}
	grant, ok := execution.GrantForScope(required.Resource, required.Operation, scope)
	return ok && grant.OwnerOnly == required.OwnerOnly
}

// WorkflowGrantAllows checks a trusted action registration against the exact
// persisted workflow business scope.
func WorkflowGrantAllows(execution policy.Execution, required policy.Grant, scope contract.BusinessScope) bool {
	return workflowGrantAllows(execution, required, scope)
}

func workflowReadGrant(execution policy.Execution, resource string, scope contract.BusinessScope) (policy.Grant, bool) {
	if scope.Kind == contract.ScopeKindTenant {
		return execution.GrantFor(resource, "read")
	}
	return execution.GrantForScope(resource, "read", scope)
}

// DefinitionClaimHandler loads the instance's immutable pinned definition and
// executes one action, condition, end, or transactionally-created human wait
// step. Timed and event waits require a separate durable continuation worker.
type DefinitionClaimHandler struct {
	store       ExecutionStore
	instances   InstanceRepository
	definitions DefinitionRepository
	catalog     *Catalog
	schemas     PayloadSchemaRegistry
	identities  ExecutionIdentityProvider
	conditions  ConditionScopeProvider
	evaluator   expression.Evaluator
	external    *ExternalActionExecutor
	human       HumanWaitRuntime
}

func NewDefinitionClaimHandler(store ExecutionStore, instances InstanceRepository, definitions DefinitionRepository, catalog *Catalog, schemas PayloadSchemaRegistry, identities ExecutionIdentityProvider, conditions ConditionScopeProvider, evaluator expression.Evaluator, leases CapabilityLeaseAdmission, humanWait ...HumanWaitRuntime) (*DefinitionClaimHandler, error) {
	if store == nil || instances == nil || definitions == nil || catalog == nil || !catalog.Frozen() || schemas == nil || identities == nil || conditions == nil || evaluator == nil {
		return nil, fmt.Errorf("workflow definition claim handler dependencies are incomplete")
	}
	if len(humanWait) > 1 {
		return nil, fmt.Errorf("workflow definition claim handler accepts at most one human wait runtime")
	}
	var human HumanWaitRuntime
	if len(humanWait) == 1 {
		human = humanWait[0]
		if (human.Requests == nil) != (human.Store == nil) {
			return nil, fmt.Errorf("workflow human wait runtime requires both request service and transaction store")
		}
		if human.Requests != nil && human.Now == nil {
			human.Now = time.Now
		}
	}
	if err := catalog.ValidateSchemas(schemas); err != nil {
		return nil, err
	}
	external, err := NewExternalActionExecutor(store, identities, schemas, leases)
	if err != nil {
		return nil, err
	}
	return &DefinitionClaimHandler{store: store, instances: instances, definitions: definitions, catalog: catalog, schemas: schemas, identities: identities, conditions: conditions, evaluator: evaluator, external: external, human: human}, nil
}

func (h *DefinitionClaimHandler) HandleClaim(ctx context.Context, claim Claim) error {
	if h == nil || ctx == nil {
		return fmt.Errorf("workflow definition claim handler is not configured")
	}
	instance, err := h.instances.GetInstance(ctx, claim.Tenant, claim.InstanceID)
	if err != nil {
		return fmt.Errorf("load claimed workflow instance: %w", err)
	}
	if instance.Status != contract.InstanceRunning || instance.CurrentStep != claim.StepID || instance.Revision != claim.Revision {
		return contract.NewError(contract.CodeConflict, "claimed workflow instance no longer matches its step")
	}
	if instance.BusinessScope.Validate() != nil || instance.BusinessScope.TenantID != claim.Tenant || claim.BusinessScope != instance.BusinessScope {
		return contract.NewError(contract.CodeConflict, "claimed workflow business scope no longer matches persisted instance")
	}
	definition, ref, err := h.definitions.GetDefinition(ctx, claim.Tenant, instance.DefinitionID, instance.DefinitionVersion)
	if err != nil {
		return fmt.Errorf("load pinned workflow definition: %w", err)
	}
	if definition.ID != instance.DefinitionID || definition.Version != instance.DefinitionVersion || ref.DefinitionID != instance.DefinitionID || ref.Version != instance.DefinitionVersion || ref.Digest == "" {
		return contract.NewError(contract.CodeConflict, "pinned workflow definition identity is invalid")
	}
	step, found := definitionStepByID(definition, claim.StepID)
	if !found {
		return contract.NewError(contract.CodeConflict, "claimed step is absent from the pinned workflow definition")
	}
	switch step.Kind {
	case contract.StepAction:
		return h.handleAction(ctx, claim, instance, definition, step)
	case contract.StepCondition:
		return h.handleCondition(ctx, claim, instance, step)
	case contract.StepEnd:
		return h.handleEnd(ctx, claim, step)
	case contract.StepHuman:
		return h.handleHumanWait(ctx, claim, instance, step)
	case contract.StepWait:
		return h.handleTimedWait(ctx, claim, instance, step)
	default:
		return contract.NewError(contract.CodeValidationFailed, "pinned workflow step kind is unsupported")
	}
}

func (h *DefinitionClaimHandler) handleTimedWait(ctx context.Context, claim Claim, instance contract.WorkflowInstance, step contract.StepDefinition) error {
	if step.Wait == nil || step.Wait.TimeoutSeconds == 0 || step.Wait.TimeoutSeconds > 2678400 {
		return contract.NewError(contract.CodeValidationFailed, "pinned workflow wait step has invalid timeout")
	}
	wake := time.Now().UTC().Add(time.Duration(step.Wait.TimeoutSeconds) * time.Second)
	change, err := preparedTransition(claim, contract.InstanceWaiting, claim.StepID, ActionResult{}, "waiting")
	if err != nil {
		return err
	}
	change.NextWakeAt = &wake
	if step.Wait.EventType != "" {
		if step.Wait.CorrelationField == "" || step.Wait.OnEvent == "" || instance.Subject.ID == "" || len(instance.Subject.ID) > 512 {
			return contract.NewError(contract.CodeValidationFailed, "pinned workflow event wait has invalid correlation")
		}
		change.EventWait = &EventWaitSubscription{
			EventType: step.Wait.EventType, CorrelationField: step.Wait.CorrelationField,
			CorrelationValue: instance.Subject.ID, OnEvent: step.Wait.OnEvent,
		}
	}
	if err := h.store.CommitStep(ctx, change); err != nil {
		return fmt.Errorf("persist workflow timeout wait: %w", err)
	}
	return nil
}

func (h *DefinitionClaimHandler) handleHumanWait(ctx context.Context, claim Claim, instance contract.WorkflowInstance, step contract.StepDefinition) error {
	if h.human.Requests == nil || h.human.Store == nil {
		return contract.NewError(contract.CodeDependencyUnavailable, "workflow human request service is not assembled")
	}
	if step.Human == nil || step.Human.Request.Subject != contract.SubjectInstance {
		return contract.NewError(contract.CodeValidationFailed, "pinned human step has an invalid subject binding")
	}
	execution, err := h.identities.ForWorkflow(ctx, claim.Tenant, claim.InstanceID)
	if err != nil {
		return fmt.Errorf("resolve workflow human request identity: %w", err)
	}
	if !execution.Valid() || execution.Tenant() != claim.Tenant || execution.Actor().PrincipalType != contract.PrincipalService {
		return contract.NewError(contract.CodeUnauthenticated, "workflow human request requires a verified tenant service identity")
	}
	template := step.Human.Request
	dueAt := h.human.Now().UTC().Add(time.Duration(template.DueAfterSeconds) * time.Second)
	executionID, key := stableExternalActionIdentity(claim.Tenant, claim.InstanceID, claim.StepID)
	subject := instance.Subject
	command := contract.SubmitRequest{
		Meta: contract.CommandMeta{IdempotencyKey: key}, Kind: template.Kind,
		Subject: &subject, Title: template.Title, Message: template.Message,
		Assignment: template.Assignment, DueAt: &dueAt, PolicyID: template.PolicyID,
		InformationSchema: template.InformationSchema,
	}
	prepared, err := h.human.Requests.PrepareWorkflowSubmit(ctx, execution, command, contract.RequestSource{InstanceID: claim.InstanceID, StepID: claim.StepID})
	if err != nil {
		return fmt.Errorf("authorize workflow human request: %w", err)
	}
	change, err := withExternalActionEvent(StepCommit{
		Claim: claim, ExecutionID: executionID, Attempt: claim.Attempt,
		IdempotencyKey: key, Status: contract.InstanceWaiting, NextStep: claim.StepID,
		NextWakeAt: &dueAt,
	}, "waiting")
	if err != nil {
		return err
	}
	if _, err := h.human.Store.CreateHumanWait(ctx, prepared, change); err != nil {
		return fmt.Errorf("atomically create human request and pause workflow: %w", err)
	}
	return nil
}

func (h *DefinitionClaimHandler) handleAction(ctx context.Context, claim Claim, instance contract.WorkflowInstance, definition contract.WorkflowDefinition, step contract.StepDefinition) error {
	if step.Action == nil {
		return contract.NewError(contract.CodeValidationFailed, "pinned action step has no action payload")
	}
	key := ActionKey{Name: step.Action.Name, Version: step.Action.Version}
	registration, err := h.catalog.Resolve(key)
	if err != nil {
		return contract.NewError(contract.CodeDependencyUnavailable, "pinned workflow action is not registered")
	}
	if step.Action.InputSchema != registration.InputSchema.ID || !h.schemas.Registered(registration.InputSchema) || !h.schemas.Registered(registration.OutputSchema) {
		return contract.NewError(contract.CodeConflict, "pinned action schema differs from its registered adapter")
	}
	if err := h.schemas.Validate(registration.InputSchema, step.Action.Input); err != nil {
		return contract.NewError(contract.CodeValidationFailed, "pinned action input failed registered schema validation")
	}
	execution, err := h.identities.ForWorkflow(ctx, claim.Tenant, claim.InstanceID)
	if err != nil {
		return fmt.Errorf("resolve workflow service identity: %w", err)
	}
	if !execution.Valid() || execution.Tenant() != claim.Tenant || execution.Actor().PrincipalType != contract.PrincipalService {
		return contract.NewError(contract.CodeUnauthenticated, "workflow action requires a verified tenant service identity")
	}
	if !workflowGrantAllows(execution, registration.RequiredGrant, instance.BusinessScope) {
		return contract.NewError(contract.CodePermissionDenied, "workflow service identity lacks the registered action grant")
	}
	identity, idem := stableExternalActionIdentity(claim.Tenant, claim.InstanceID, claim.StepID)
	actionContext := ActionContext{Execution: execution, BusinessScope: instance.BusinessScope, CausationDepth: instance.CausationDepth, RequiredGrant: registration.RequiredGrant, InstanceID: claim.InstanceID, StepID: claim.StepID, ExecutionID: identity, Subject: instance.Subject, IdempotencyKey: idem}
	input := ActionInput{Schema: registration.InputSchema, Data: append(json.RawMessage(nil), step.Action.Input...)}
	if registration.Class == ActionLocal {
		_, err := registration.Local.ExecuteLocal(ctx, claim, actionContext, registration.Key, input, step.Action.Next)
		if err != nil {
			var localFailure *LocalActionFailure
			if errors.As(err, &localFailure) {
				change, commitErr := localActionFailureCommit(claim, identity, idem, localFailure)
				if commitErr != nil {
					return errors.Join(err, commitErr)
				}
				if commitErr := h.store.CommitStep(ctx, change); commitErr != nil {
					return errors.Join(err, fmt.Errorf("persist local action failure outcome: %w", commitErr))
				}
				return nil
			}
			uncertain, eventErr := withExternalActionEvent(StepCommit{
				Claim: claim, ExecutionID: identity, Attempt: claim.Attempt,
				IdempotencyKey: idem, Status: contract.InstanceUncertain,
				Failure: "local action transaction outcome could not be confirmed",
			}, "outcome_unknown")
			if eventErr != nil {
				return errors.Join(err, eventErr)
			}
			if commitErr := h.store.CommitStep(ctx, uncertain); commitErr != nil {
				return errors.Join(fmt.Errorf("local workflow action outcome is uncertain: %w", err), fmt.Errorf("persist uncertain local action: %w", commitErr))
			}
			return nil
		}
		current, err := h.instances.GetInstance(ctx, claim.Tenant, claim.InstanceID)
		if err != nil {
			return fmt.Errorf("verify locally committed workflow action: %w", err)
		}
		if current.Revision == claim.Revision && current.CurrentStep == claim.StepID && current.Status == contract.InstanceRunning {
			uncertain, eventErr := withExternalActionEvent(StepCommit{
				Claim: claim, ExecutionID: identity, Attempt: claim.Attempt,
				IdempotencyKey: idem, Status: contract.InstanceUncertain,
				Failure: "local action returned without committing workflow progress",
			}, "outcome_unknown")
			if eventErr != nil {
				return eventErr
			}
			return h.store.CommitStep(ctx, uncertain)
		}
		return nil
	}
	change, err := h.external.Execute(ctx, claim, definition, step, registration, instance.Subject)
	if err != nil {
		return err
	}
	if change.ExecutionID != identity || change.IdempotencyKey != idem {
		return contract.NewError(contract.CodeInternal, "workflow action identity derivation is inconsistent")
	}
	if err := h.store.CommitStep(ctx, change); err != nil {
		return fmt.Errorf("commit external workflow action: %w", err)
	}
	return nil
}

func localActionFailureCommit(claim Claim, executionID string, key contract.IdempotencyKey, failure *LocalActionFailure) (StepCommit, error) {
	if failure == nil || failure.Kind < LocalActionNotAppliedRetryable || failure.Kind > LocalActionOutcomeUnknown {
		return StepCommit{}, contract.NewError(contract.CodeValidationFailed, "local workflow action failure classification is invalid")
	}
	change := StepCommit{Claim: claim, ExecutionID: executionID, Attempt: claim.Attempt, IdempotencyKey: key}
	kind := "failed"
	switch failure.Kind {
	case LocalActionNotAppliedRetryable:
		if claim.Attempt >= maxExternalActionAttempts {
			change.Status = contract.InstanceFailed
			change.Failure = "local action retry limit reached after confirmed rollback"
		} else {
			wake := time.Now().UTC().Add(externalRetryDelay(claim))
			if !claim.FirstAttemptAt.IsZero() && wake.After(claim.FirstAttemptAt.Add(maxExternalActionElapsed)) {
				change.Status = contract.InstanceFailed
				change.Failure = "local action retry window elapsed after confirmed rollback"
			} else {
				change.Status = contract.InstanceRetrying
				change.NextStep = claim.StepID
				change.NextWakeAt = &wake
				change.Failure = "local action failed before commit and is scheduled for retry"
				kind = "retry_scheduled"
			}
		}
	case LocalActionNotAppliedPermanent:
		change.Status = contract.InstanceFailed
		change.Failure = "local domain action was rejected before commit"
	case LocalActionOutcomeUnknown:
		change.Status = contract.InstanceUncertain
		change.Failure = "local action commit outcome is unknown"
		kind = "outcome_unknown"
	}
	change, err := withExternalActionEvent(change, kind)
	return change, err
}

func (h *DefinitionClaimHandler) handleCondition(ctx context.Context, claim Claim, instance contract.WorkflowInstance, step contract.StepDefinition) error {
	if step.Condition == nil {
		return h.failCondition(ctx, claim, "pinned condition step is invalid")
	}
	execution, err := h.identities.ForWorkflow(ctx, claim.Tenant, claim.InstanceID)
	if err != nil {
		return fmt.Errorf("resolve workflow condition identity: %w", err)
	}
	if !execution.Valid() || execution.Tenant() != claim.Tenant || execution.Actor().PrincipalType != contract.PrincipalService {
		return h.failCondition(ctx, claim, "workflow condition identity is invalid")
	}
	readGrant, allowed := workflowReadGrant(execution, string(instance.Subject.Kind), instance.BusinessScope)
	if !allowed || readGrant.OwnerOnly {
		// A workflow service has no authenticated human subject against which an
		// owner-only grant can be evaluated. Do not treat the workflow starter or
		// a recorded request assignee as current authorization.
		return h.failCondition(ctx, claim, "workflow condition is not authorized to read its subject")
	}
	expressionText, err := ConditionExpression(step.Condition)
	if err != nil {
		return h.failCondition(ctx, claim, "pinned workflow condition is invalid")
	}
	parsed, err := expression.Parse(expressionText, 100, 64)
	if err != nil {
		return h.failCondition(ctx, claim, "pinned workflow condition is invalid")
	}
	scope, err := h.conditions.Scope(ctx, execution, instance)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if terminalConditionError(err) {
			return h.failCondition(ctx, claim, "workflow condition subject is stale or unauthorized")
		}
		return fmt.Errorf("load authorized workflow condition scope: %w", err)
	}
	value, err := h.evaluator.Eval(ctx, parsed.AST, scope)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if terminalConditionError(err) {
			return h.failCondition(ctx, claim, "workflow condition evaluation was rejected")
		}
		return fmt.Errorf("evaluate workflow condition: %w", err)
	}
	if value.Kind != expression.ValueBool {
		return h.failCondition(ctx, claim, "workflow condition did not produce a boolean")
	}
	next := step.Condition.IfFalse
	if value.Bool {
		next = step.Condition.IfTrue
	}
	change, err := preparedTransition(claim, contract.InstanceReady, next, ActionResult{}, "advanced")
	if err != nil {
		return err
	}
	if err := h.store.CommitStep(ctx, change); err != nil {
		return fmt.Errorf("commit workflow condition: %w", err)
	}
	return nil
}

func (h *DefinitionClaimHandler) failCondition(ctx context.Context, claim Claim, reason string) error {
	change, err := preparedTransition(claim, contract.InstanceFailed, "", ActionResult{}, "condition_failed")
	if err != nil {
		return err
	}
	change.Failure = reason
	if err := h.store.CommitStep(ctx, change); err != nil {
		return fmt.Errorf("persist terminal workflow condition failure: %w", err)
	}
	return nil
}

func terminalConditionError(err error) bool {
	var typed *contract.Error
	if !errors.As(err, &typed) {
		return false
	}
	switch typed.Type {
	case contract.CodePermissionDenied, contract.CodeUnauthenticated, contract.CodeValidationFailed, contract.CodeNotFound, contract.CodeStaleSubject:
		return true
	default:
		return false
	}
}

func (h *DefinitionClaimHandler) handleEnd(ctx context.Context, claim Claim, step contract.StepDefinition) error {
	if step.End == nil || step.End.Outcome == "" {
		return contract.NewError(contract.CodeValidationFailed, "pinned workflow end step is invalid")
	}
	result, err := json.Marshal(struct {
		Outcome string `json:"outcome"`
	}{Outcome: step.End.Outcome})
	if err != nil {
		return fmt.Errorf("encode workflow end outcome: %w", err)
	}
	change, err := preparedTransition(claim, contract.InstanceCompleted, "", ActionResult{Data: result}, "completed")
	if err != nil {
		return err
	}
	if err := h.store.CommitStep(ctx, change); err != nil {
		return fmt.Errorf("commit workflow completion: %w", err)
	}
	return nil
}

func preparedTransition(claim Claim, status contract.InstanceStatus, next contract.StepID, result ActionResult, eventKind string) (StepCommit, error) {
	executionID, key := stableExternalActionIdentity(claim.Tenant, claim.InstanceID, claim.StepID)
	change := StepCommit{Claim: claim, ExecutionID: executionID, Attempt: claim.Attempt, IdempotencyKey: key, Status: status, NextStep: next, Result: result}
	return withExternalActionEvent(change, eventKind)
}
