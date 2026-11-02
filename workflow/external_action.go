package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
	leases     CapabilityLeaseAdmission
}

// CapabilityLeaseAdmission is implemented by the tenant module store. The
// admission update serializes with lease revocation before a new provider call.
type CapabilityLeaseAdmission interface {
	AdmitCapabilityAttempt(context.Context, contract.CapabilityLease, time.Time) error
}

type CapabilityLeaseManager interface {
	CapabilityLeaseAdmission
	EnsureCapabilityLease(context.Context, contract.CapabilityLease) error
}

// PinnedCapabilityDispatcher ensures a durable external action keeps the
// selected provider and capability binding across retries.
type PinnedCapabilityDispatcher interface {
	Dispatch(context.Context, contract.PinnedDispatch, contract.CapabilityLease, ActionRegistration, json.RawMessage) (ActionResult, error)
	Reconcile(context.Context, contract.PinnedDispatch) (contract.ExternalOutcome, ActionResult, error)
}

// HTTPSCapabilityDispatcher sends pinned operations to one configured external
// provider endpoint. Provider identity and URL are fixed at assembly; only the
// encrypted-secret reference is resolved immediately before transport.
type HTTPSCapabilityDispatcher struct {
	ProviderID     string
	Endpoint       string
	StatusEndpoint string
	Client         *http.Client
}

type externalProviderReconciliation struct {
	Outcome contract.ExternalOutcome `json:"outcome"`
	Result  ActionResult             `json:"result"`
}

func (d *HTTPSCapabilityDispatcher) Dispatch(ctx context.Context, pin contract.PinnedDispatch, lease contract.CapabilityLease, registration ActionRegistration, input json.RawMessage) (ActionResult, error) {
	if d == nil || ctx == nil || pin.Validate() != nil || d.ProviderID != pin.ProviderID || registration.ProviderID != pin.ProviderID || registration.CredentialResolver == nil || lease.Validate() != nil || lease.CredentialRef == "" || lease.ProviderID != pin.ProviderID || lease.CallerModuleID != pin.CallerModuleID || lease.BindingRevision != pin.BindingRevision || lease.Operation != pin.Operation.OperationName || lease.Scope != pin.Scope {
		return ActionResult{}, contract.NewError(contract.CodePermissionDenied, "pinned external provider configuration is unavailable")
	}
	endpoint, err := url.Parse(d.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.Fragment != "" {
		return ActionResult{}, contract.NewError(contract.CodeValidationFailed, "external provider endpoint must be a fixed HTTPS URL")
	}
	if d.Client != nil {
		if d.Client.CheckRedirect == nil {
			return ActionResult{}, contract.NewError(contract.CodeValidationFailed, "external provider HTTP client must reject redirects")
		}
	} else if transport, ok := http.DefaultTransport.(*http.Transport); !ok || transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
		return ActionResult{}, contract.NewError(contract.CodeValidationFailed, "external provider TLS transport is unavailable")
	}
	if len(input) > 1<<20 || !json.Valid(input) || sha256Hex(input) != pin.InputHash {
		return ActionResult{}, contract.NewError(contract.CodeValidationFailed, "pinned external provider input does not match its durable digest")
	}
	credential, err := registration.CredentialResolver.ResolveCredential(ctx, lease.CredentialRef)
	if err != nil || credential == "" {
		return ActionResult{}, contract.NewError(contract.CodeDependencyUnavailable, "external provider credential is expired, rotated, or disabled")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(string(input)))
	if err != nil {
		return ActionResult{}, contract.NewError(contract.CodeValidationFailed, "external provider request could not be prepared")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Idempotency-Key", string(pin.IdempotencyKey))
	request.Header.Set("X-Kora-Provider", pin.ProviderID)
	request.Header.Set("X-Kora-Operation", pin.Operation.OperationName)
	request.Header.Set("X-Kora-Binding-Revision", fmt.Sprint(pin.BindingRevision))
	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	response, err := client.Do(request)
	if err != nil {
		return ActionResult{}, fmt.Errorf("external provider outcome is unknown")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return ActionResult{}, &ExternalActionFailure{Kind: ExternalNotAppliedPermanent, Cause: contract.NewError(contract.CodeUnauthenticated, "external provider authentication failed")}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil || len(body) > 1<<20 {
		return ActionResult{}, fmt.Errorf("external provider outcome is unknown")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return ActionResult{}, fmt.Errorf("external provider outcome is unknown")
	}
	var result ActionResult
	if err := json.Unmarshal(body, &result); err != nil || result.Schema != pin.Operation.OutputSchema || len(result.Data) == 0 || !json.Valid(result.Data) {
		return ActionResult{}, fmt.Errorf("external provider returned an invalid operation result")
	}
	return result, nil
}

func (d *HTTPSCapabilityDispatcher) Reconcile(ctx context.Context, pin contract.PinnedDispatch) (contract.ExternalOutcome, ActionResult, error) {
	return contract.ExternalUnknown, ActionResult{}, contract.NewError(contract.CodeDependencyUnavailable, "external provider reconciliation endpoint is not configured")
}

func (d *HTTPSCapabilityDispatcher) ReconcilePinned(ctx context.Context, pin contract.PinnedDispatch, lease contract.CapabilityLease, registration ActionRegistration) (contract.ExternalOutcome, ActionResult, error) {
	if d == nil || ctx == nil || pin.Validate() != nil || lease.Validate() != nil || d.ProviderID != pin.ProviderID || lease.ProviderID != pin.ProviderID || lease.Scope != pin.Scope || lease.CallerModuleID != pin.CallerModuleID || lease.BindingRevision != pin.BindingRevision || lease.Operation != pin.Operation.OperationName {
		return contract.ExternalUnknown, ActionResult{}, contract.NewError(contract.CodePermissionDenied, "pinned reconciliation identity is invalid")
	}
	endpoint, err := url.Parse(d.StatusEndpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.Fragment != "" || lease.CredentialRef == "" {
		return contract.ExternalUnknown, ActionResult{}, contract.NewError(contract.CodeDependencyUnavailable, "external provider reconciliation endpoint is not configured")
	}
	if d.Client != nil && d.Client.CheckRedirect == nil {
		return contract.ExternalUnknown, ActionResult{}, contract.NewError(contract.CodeValidationFailed, "external provider HTTP client must reject redirects")
	}
	return d.reconcileRequest(ctx, endpoint.String(), pin, lease, registration.CredentialResolver)
}

func (d *HTTPSCapabilityDispatcher) reconcileRequest(ctx context.Context, endpoint string, pin contract.PinnedDispatch, lease contract.CapabilityLease, resolver ProviderCredentialResolver) (contract.ExternalOutcome, ActionResult, error) {
	if resolver == nil {
		return contract.ExternalUnknown, ActionResult{}, contract.NewError(contract.CodeDependencyUnavailable, "external provider credential resolver is unavailable")
	}
	credential, err := resolver.ResolveCredential(ctx, lease.CredentialRef)
	if err != nil || credential == "" {
		return contract.ExternalUnknown, ActionResult{}, contract.NewError(contract.CodeDependencyUnavailable, "external provider credential is expired, rotated, or disabled")
	}
	body, _ := json.Marshal(map[string]any{"execution_id": pin.ExecutionID, "provider_id": pin.ProviderID, "operation": pin.Operation.OperationName, "binding_revision": pin.BindingRevision, "idempotency_key": pin.IdempotencyKey, "input_hash": pin.InputHash})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return contract.ExternalUnknown, ActionResult{}, contract.NewError(contract.CodeValidationFailed, "external provider reconciliation request is invalid")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+credential)
	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	response, err := client.Do(request)
	if err != nil {
		return contract.ExternalUnknown, ActionResult{}, contract.NewError(contract.CodeDependencyUnavailable, "external provider reconciliation outcome is unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return contract.ExternalUnknown, ActionResult{}, contract.NewError(contract.CodeUnauthenticated, "external provider authentication failed during reconciliation")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return contract.ExternalUnknown, ActionResult{}, contract.NewError(contract.CodeDependencyUnavailable, "external provider reconciliation outcome is unavailable")
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil || len(encoded) > 1<<20 {
		return contract.ExternalUnknown, ActionResult{}, contract.NewError(contract.CodeDependencyUnavailable, "external provider reconciliation response is invalid")
	}
	var result externalProviderReconciliation
	if err := json.Unmarshal(encoded, &result); err != nil || result.Outcome != contract.ExternalApplied && result.Outcome != contract.ExternalNotApplied && result.Outcome != contract.ExternalUnknown {
		return contract.ExternalUnknown, ActionResult{}, contract.NewError(contract.CodeDependencyUnavailable, "external provider reconciliation response is invalid")
	}
	if result.Outcome == contract.ExternalApplied && (result.Result.Schema != pin.Operation.OutputSchema || !json.Valid(result.Result.Data)) {
		return contract.ExternalUnknown, ActionResult{}, contract.NewError(contract.CodeDependencyUnavailable, "external provider reconciliation result is invalid")
	}
	return result.Outcome, result.Result, nil
}

func sha256Hex(input []byte) string {
	digest := sha256.Sum256(input)
	return hex.EncodeToString(digest[:])
}

type CapabilityDispatchState struct {
	Dispatch     contract.PinnedDispatch
	Outcome      contract.ExternalOutcome
	ReconciledAt time.Time
	Evidence     string
}

func NewPinnedDispatch(executionID string, scope contract.BusinessScope, caller contract.ModuleID, bindingRevision uint64, providerID string, lease contract.CapabilityLease, action contract.CapabilityAction, idempotency contract.IdempotencyKey, input json.RawMessage) (contract.PinnedDispatch, error) {
	if executionID == "" || scope.Validate() != nil || caller == "" || providerID == "" || action.OperationName == "" || idempotency == "" || !json.Valid(input) {
		return contract.PinnedDispatch{}, fmt.Errorf("pinned capability dispatch identity is incomplete")
	}
	if lease.Validate() != nil || lease.State != "active" || lease.TenantID != scope.TenantID || lease.Scope != scope || lease.CallerModuleID != caller || lease.ProviderID != providerID || lease.BindingRevision != bindingRevision || lease.Operation != action.OperationName {
		return contract.PinnedDispatch{}, fmt.Errorf("pinned capability lease does not match the provider binding")
	}
	if action.InputSchema.ID == "" || action.InputSchema.Version == 0 || action.OutputSchema.ID == "" || action.OutputSchema.Version == 0 {
		return contract.PinnedDispatch{}, fmt.Errorf("pinned capability schemas are required")
	}
	hash := sha256.Sum256(input)
	return contract.PinnedDispatch{ExecutionID: executionID, Scope: scope, CallerModuleID: caller, ProviderID: providerID, BindingRevision: bindingRevision, Operation: action, IdempotencyKey: idempotency, InputHash: hex.EncodeToString(hash[:])}, nil
}

func NewExternalActionExecutor(store ExecutionStore, identities ExecutionIdentityProvider, schemas PayloadSchemaRegistry, leases CapabilityLeaseAdmission) (*ExternalActionExecutor, error) {
	if store == nil || identities == nil || schemas == nil {
		return nil, fmt.Errorf("external workflow action executor requires store, identity provider, and schemas")
	}
	return &ExternalActionExecutor{store: store, identities: identities, schemas: schemas, leases: leases}, nil
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
	if claim.BusinessScope.Validate() != nil || claim.BusinessScope.TenantID != claim.Tenant {
		return StepCommit{}, contract.NewError(contract.CodeValidationFailed, "external action workflow business scope is invalid")
	}
	if definition.ID == "" || definition.Version == 0 {
		return StepCommit{}, contract.NewError(contract.CodeValidationFailed, "external action requires its pinned workflow definition")
	}
	action := step.Action
	pinned := registration.PinnedDispatcher != nil
	if registration.Class != ActionExternal || (!pinned && registration.External == nil) || (pinned && (registration.External != nil || registration.Capability == nil)) || registration.Local != nil || registration.Key != (ActionKey{Name: action.Name, Version: action.Version}) {
		return StepCommit{}, contract.NewError(contract.CodeValidationFailed, "registered external action does not match the pinned step")
	}
	if pinned && (e.leases == nil || registration.CapabilityLease == nil || registration.CapabilityLease.Scope != claim.BusinessScope || registration.CapabilityLease.TenantID != claim.Tenant || registration.CapabilityLease.ProviderID != registration.ProviderID || registration.CapabilityLease.CallerModuleID != registration.CallerModuleID || registration.CapabilityLease.BindingRevision != registration.BindingRevision || registration.CapabilityLease.Operation != registration.Capability.OperationName) {
		return StepCommit{}, contract.NewError(contract.CodePermissionDenied, "pinned capability has no matching durable lease")
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
	if !workflowGrantAllows(execution, registration.RequiredGrant, claim.BusinessScope) {
		return StepCommit{}, contract.NewError(contract.CodePermissionDenied, "workflow service identity lacks the registered action grant")
	}
	executionID, key := stableExternalActionIdentity(claim.Tenant, claim.InstanceID, claim.StepID)
	var requested *contract.PinnedDispatch
	var requestedLease *contract.CapabilityLease
	if registration.Capability != nil {
		p, err := NewPinnedDispatch(executionID, claim.BusinessScope, registration.CallerModuleID, registration.BindingRevision, registration.ProviderID, *registration.CapabilityLease, *registration.Capability, key, input.Data)
		if err != nil {
			return StepCommit{}, contract.NewError(contract.CodeValidationFailed, "pinned capability dispatch is invalid")
		}
		requested = &p
		requestedLease = registration.CapabilityLease
	}
	var storedAttempt *PinnedCapabilityAttempt
	var stored *contract.PinnedDispatch
	if pinned {
		pinnedStore, ok := e.store.(PinnedCapabilityAttemptStore)
		if !ok || requested == nil || requestedLease == nil {
			return StepCommit{}, contract.NewError(contract.CodeInternal, "workflow store cannot persist pinned capability leases")
		}
		storedAttempt, err = pinnedStore.BeginPinnedExternalAttempt(ctx, claim, executionID, key, *requested, *requestedLease)
		if storedAttempt != nil {
			stored = &storedAttempt.Dispatch
		}
	} else {
		stored, err = e.store.BeginExternalAttempt(ctx, claim, executionID, key, nil)
	}
	if err != nil {
		return StepCommit{}, fmt.Errorf("persist workflow external attempt: %w", err)
	}
	var subjectRef contract.SubjectRef
	if len(subject) > 0 {
		subjectRef = subject[0]
	}
	var storedDispatch *contract.PinnedDispatch
	if stored != nil {
		storedDispatch = stored
	}
	actionContext := ActionContext{Execution: execution, BusinessScope: claim.BusinessScope, CausationDepth: claim.CausationDepth, RequiredGrant: registration.RequiredGrant, InstanceID: claim.InstanceID, StepID: claim.StepID, Subject: subjectRef, IdempotencyKey: key, PinnedDispatch: storedDispatch}
	var result ActionResult
	var actionErr error
	if pinned {
		if stored == nil {
			return StepCommit{}, contract.NewError(contract.CodeInternal, "pinned dispatch was not durably stored")
		}
		if storedAttempt == nil || storedAttempt.Validate() != nil {
			return StepCommit{}, contract.NewError(contract.CodeConflict, "persisted pinned capability lease is invalid")
		}
		if err := e.leases.AdmitCapabilityAttempt(ctx, storedAttempt.Lease, time.Now().UTC()); err != nil {
			var denial *contract.Error
			if errors.As(err, &denial) && denial.Type == contract.CodePermissionDenied {
				return externalLeaseDeniedCommit(claim, executionID, key)
			}
			// No provider request was started. Persist a safe retry transition;
			// an unavailable lease store can be retried without provider effects.
			return externalNotAppliedCommit(claim, executionID, key)
		}
		result, actionErr = registration.PinnedDispatcher.Dispatch(ctx, storedAttempt.Dispatch, storedAttempt.Lease, registration, input.Data)
	} else {
		result, actionErr = callExternalAction(ctx, registration.External, actionContext, input)
	}
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

func externalLeaseDeniedCommit(claim Claim, executionID string, key contract.IdempotencyKey) (StepCommit, error) {
	change := StepCommit{Claim: claim, ExecutionID: executionID, Attempt: claim.Attempt, AttemptStarted: true, IdempotencyKey: key,
		Status: contract.InstanceFailed, Failure: "pinned capability lease is revoked, expired, or stale"}
	return withExternalActionEvent(change, "failed")
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
