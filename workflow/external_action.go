package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/asenawritescode/kora/contract"
)

const (
	maxExternalActionAttempts uint8 = 8
	maxExternalRetryDelay           = 64 * time.Second
	maxExternalActionElapsed        = 24 * time.Hour
	minExternalRetryJitter          = 750
	maxExternalRetryJitter          = 1250
)

// ExternalActionExecutor is the durable dispatch boundary for provider-backed
// workflow actions. It persists attempt ownership before invoking the adapter
// and returns the fenced transition the caller must commit immediately after
// the effect result is known.
type ExternalActionExecutor struct {
	store      ExecutionStore
	identities ExecutionIdentityProvider
	schemas    PayloadSchemaRegistry
}

// PinnedCapabilityDispatcher ensures a durable external action keeps the
// selected provider and capability binding across retries.
type PinnedCapabilityDispatcher interface {
	Dispatch(context.Context, contract.PinnedDispatch, ActionRegistration, json.RawMessage) (ActionResult, error)
	Reconcile(context.Context, contract.PinnedDispatch) (contract.ExternalOutcome, ActionResult, error)
}

type CapabilityDispatchState struct {
	Dispatch     contract.PinnedDispatch
	Outcome      contract.ExternalOutcome
	ReconciledAt time.Time
	Evidence     string
}

func NewPinnedDispatch(executionID string, tenant contract.TenantID, scope string, caller contract.ModuleID, bindingRevision uint64, providerID string, action contract.CapabilityAction, idempotency contract.IdempotencyKey, input json.RawMessage) (contract.PinnedDispatch, error) {
	if executionID == "" || tenant == "" || scope == "" || caller == "" || providerID == "" || action.OperationName == "" || idempotency == "" || !json.Valid(input) {
		return contract.PinnedDispatch{}, fmt.Errorf("pinned capability dispatch identity is incomplete")
	}
	if action.InputSchema.ID == "" || action.InputSchema.Version == 0 || action.OutputSchema.ID == "" || action.OutputSchema.Version == 0 {
		return contract.PinnedDispatch{}, fmt.Errorf("pinned capability schemas are required")
	}
	hash := sha256.Sum256(input)
	return contract.PinnedDispatch{ExecutionID: executionID, TenantID: tenant, ScopeKey: scope, CallerModuleID: caller, ProviderID: providerID, BindingRevision: bindingRevision, Operation: action, IdempotencyKey: idempotency, InputHash: hex.EncodeToString(hash[:])}, nil
}

func NewExternalActionExecutor(store ExecutionStore, identities ExecutionIdentityProvider, schemas PayloadSchemaRegistry) (*ExternalActionExecutor, error) {
	if store == nil || identities == nil || schemas == nil {
		return nil, fmt.Errorf("external workflow action executor requires store, identity provider, and schemas")
	}
	return &ExternalActionExecutor{store: store, identities: identities, schemas: schemas}, nil
}

// Execute validates the pinned action input and service identity, writes the
// running-attempt marker, then calls the registered external adapter. Adapter
// errors and invalid output are uncertain because the provider may have
// applied the effect before the response was lost or rejected locally.
func (e *ExternalActionExecutor) Execute(ctx context.Context, claim Claim, definition contract.WorkflowDefinition, step contract.StepDefinition, registration ActionRegistration, subject ...contract.SubjectRef) (StepCommit, error) {
	if e == nil || e.store == nil || e.identities == nil || e.schemas == nil {
		return StepCommit{}, fmt.Errorf("external workflow action executor is not configured")
	}
	if ctx == nil {
		return StepCommit{}, fmt.Errorf("external workflow action requires context")
	}
	if err := ctx.Err(); err != nil {
		return StepCommit{}, err
	}
	if claim.Attempt == 0 || claim.InstanceID == "" || claim.StepID == "" || step.ID != claim.StepID || step.Kind != contract.StepAction || step.Action == nil {
		return StepCommit{}, contract.NewError(contract.CodeValidationFailed, "external action does not match the claimed workflow step")
	}
	if definition.ID == "" || definition.Version == 0 {
		return StepCommit{}, contract.NewError(contract.CodeValidationFailed, "external action requires its pinned workflow definition")
	}
	action := step.Action
	if registration.Class != ActionExternal || registration.External == nil || registration.Local != nil || registration.Key != (ActionKey{Name: action.Name, Version: action.Version}) {
		return StepCommit{}, contract.NewError(contract.CodeValidationFailed, "registered external action does not match the pinned step")
	}
	if action.InputSchema != registration.InputSchema.ID || !e.schemas.Registered(registration.InputSchema) || !e.schemas.Registered(registration.OutputSchema) {
		return StepCommit{}, contract.NewError(contract.CodeValidationFailed, "workflow action schema does not match the registered adapter")
	}
	input := ActionInput{Schema: registration.InputSchema, Data: append(json.RawMessage(nil), action.Input...)}
	if err := e.schemas.Validate(registration.InputSchema, input.Data); err != nil {
		return StepCommit{}, contract.NewError(contract.CodeValidationFailed, "workflow action input failed registered schema validation")
	}
	execution, err := e.identities.ForWorkflow(ctx, claim.Tenant, claim.InstanceID)
	if err != nil {
		return StepCommit{}, fmt.Errorf("resolve workflow service identity: %w", err)
	}
	if !execution.Valid() || execution.Tenant() != claim.Tenant || execution.Actor().PrincipalType != contract.PrincipalService {
		return StepCommit{}, contract.NewError(contract.CodeUnauthenticated, "workflow action requires a verified tenant service identity")
	}
	if !registration.RequiredGrant.Valid() || !execution.Allows(registration.RequiredGrant.Resource, registration.RequiredGrant.Operation) {
		return StepCommit{}, contract.NewError(contract.CodePermissionDenied, "workflow service identity lacks the registered action grant")
	}
	executionID, key := stableExternalActionIdentity(claim.Tenant, claim.InstanceID, claim.StepID)
	if err := e.store.BeginExternalAttempt(ctx, claim, executionID, key); err != nil {
		return StepCommit{}, fmt.Errorf("persist workflow external attempt: %w", err)
	}
	var subjectRef contract.SubjectRef
	if len(subject) > 0 {
		subjectRef = subject[0]
	}
	actionContext := ActionContext{Execution: execution, RequiredGrant: registration.RequiredGrant, InstanceID: claim.InstanceID, StepID: claim.StepID, Subject: subjectRef, IdempotencyKey: key}
	result, actionErr := callExternalAction(ctx, registration.External, actionContext, input)
	if actionErr != nil {
		var classified *ExternalActionFailure
		if errors.As(actionErr, &classified) {
			switch classified.Kind {
			case ExternalNotAppliedRetryable:
				return externalNotAppliedCommit(claim, executionID, key)
			case ExternalNotAppliedPermanent:
				change := StepCommit{Claim: claim, ExecutionID: executionID, Attempt: claim.Attempt, AttemptStarted: true, IdempotencyKey: key, Status: contract.InstanceFailed, Failure: "provider confirmed the external action was not applied"}
				return withExternalActionEvent(change, "failed")
			}
		}
		return withExternalActionEvent(externalUncertainCommit(claim, executionID, key, "external action returned without a confirmed outcome"), "outcome_unknown")
	}
	if result.Schema != registration.OutputSchema || len(result.Data) > 65536 || !json.Valid(result.Data) {
		return withExternalActionEvent(externalUncertainCommit(claim, executionID, key, "external action outcome could not be validated"), "outcome_unknown")
	}
	if err := e.schemas.Validate(registration.OutputSchema, result.Data); err != nil {
		return withExternalActionEvent(externalUncertainCommit(claim, executionID, key, "external action outcome failed registered schema validation"), "outcome_unknown")
	}
	change := StepCommit{
		Claim: claim, ExecutionID: executionID, Attempt: claim.Attempt, AttemptStarted: true,
		IdempotencyKey: key, Status: contract.InstanceReady, NextStep: action.Next,
		Result: ActionResult{Schema: result.Schema, Data: append(json.RawMessage(nil), result.Data...)},
	}
	return withExternalActionEvent(change, "advanced")
}

func externalNotAppliedCommit(claim Claim, executionID string, key contract.IdempotencyKey) (StepCommit, error) {
	status := contract.InstanceRetrying
	nextStep := claim.StepID
	failure := "provider confirmed the external action was not applied; retry is scheduled"
	var wake *time.Time
	changeKind := "retry_scheduled"
	if claim.Attempt >= maxExternalActionAttempts {
		status = contract.InstanceFailed
		nextStep = ""
		failure = "external action retry limit reached; provider confirmed no effect was applied"
		changeKind = "failed"
	} else {
		due := time.Now().UTC().Add(externalRetryDelay(claim))
		if !claim.FirstAttemptAt.IsZero() && due.After(claim.FirstAttemptAt.Add(maxExternalActionElapsed)) {
			status = contract.InstanceFailed
			nextStep = ""
			failure = "external action retry window elapsed; provider confirmed no effect was applied"
			changeKind = "failed"
		} else {
			wake = &due
		}
	}
	change := StepCommit{Claim: claim, ExecutionID: executionID, Attempt: claim.Attempt, AttemptStarted: true, IdempotencyKey: key, Status: status, NextStep: nextStep, NextWakeAt: wake, Failure: failure}
	return withExternalActionEvent(change, changeKind)
}

func externalRetryDelay(claim Claim) time.Duration {
	shift := claim.Attempt - 1
	if shift > 6 {
		shift = 6
	}
	base := time.Second * time.Duration(uint64(1)<<shift)
	if base > maxExternalRetryDelay {
		base = maxExternalRetryDelay
	}
	sum := sha256.Sum256([]byte(string(claim.Tenant) + "\x00" + string(claim.InstanceID) + "\x00" + string(claim.StepID) + "\x00" + fmt.Sprint(claim.Attempt)))
	jitter := minExternalRetryJitter + int(sum[0])%(maxExternalRetryJitter-minExternalRetryJitter+1)
	return time.Duration(int64(base) * int64(jitter) / 1000)
}

func callExternalAction(ctx context.Context, action Action, actionContext ActionContext, input ActionInput) (result ActionResult, err error) {
	defer func() {
		if recover() != nil {
			result = ActionResult{}
			err = fmt.Errorf("external workflow action panicked")
		}
	}()
	return action.Execute(ctx, actionContext, input)
}

func externalUncertainCommit(claim Claim, executionID string, key contract.IdempotencyKey, reason string) StepCommit {
	return StepCommit{
		Claim: claim, ExecutionID: executionID, Attempt: claim.Attempt, AttemptStarted: true,
		IdempotencyKey: key, Status: contract.InstanceUncertain, Failure: reason,
	}
}

func stableExternalActionIdentity(tenant contract.TenantID, instance contract.InstanceID, step contract.StepID) (string, contract.IdempotencyKey) {
	sum := sha256.Sum256([]byte(string(tenant) + "\x00" + string(instance) + "\x00" + string(step)))
	digest := hex.EncodeToString(sum[:])
	// MySQL stores execution_id in VARCHAR(64), so keep this stable SHA-256
	// identity at its exact encoded length. The separate idempotency key keeps
	// its human-readable namespace prefix.
	return digest, contract.IdempotencyKey("wf-action-" + digest)
}

func withExternalActionEvent(change StepCommit, kind string) (StepCommit, error) {
	revision, err := strconv.ParseUint(string(change.Claim.Revision), 10, 64)
	if err != nil || revision == ^uint64(0) {
		return StepCommit{}, contract.NewError(contract.CodeValidationFailed, "workflow revision cannot be advanced")
	}
	data, err := json.Marshal(contract.WorkflowEventData{InstanceID: change.Claim.InstanceID, Revision: contract.Revision(strconv.FormatUint(revision+1, 10)), ChangeKind: kind})
	if err != nil {
		return StepCommit{}, fmt.Errorf("encode workflow action event: %w", err)
	}
	eventType := "kora.workflow.advanced"
	switch kind {
	case "outcome_unknown":
		eventType = "kora.workflow.outcome_unknown"
	case "completed":
		eventType = "kora.workflow.completed"
	case "failed":
		eventType = "kora.workflow.failed"
	case "waiting":
		eventType = "kora.workflow.waiting"
	}
	event := contract.EventEnvelope{ID: contract.NewEventID(), Type: eventType, Version: contract.CurrentVersion, Source: "workflow", Site: string(change.Claim.Tenant), AggregateType: "workflow_instance", AggregateID: string(change.Claim.InstanceID), OccurredAt: time.Now().UTC(), Data: data}
	if err := event.Validate(); err != nil {
		return StepCommit{}, fmt.Errorf("validate workflow action event: %w", err)
	}
	change.Events = append(change.Events, event)
	return change, nil
}

// WithExecutionEvent adds the required data-minimal lifecycle event for a
// fenced transition performed by an in-process local action executor.
func WithExecutionEvent(change StepCommit, kind string) (StepCommit, error) {
	return withExternalActionEvent(change, kind)
}
