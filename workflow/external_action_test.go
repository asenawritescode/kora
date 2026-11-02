package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/policy"
)

type externalDispatchStore struct {
	started bool
	claim   Claim
	key     contract.IdempotencyKey
	ident   string
	commit  *StepCommit
}

func (s *externalDispatchStore) ClaimDue(context.Context, contract.TenantID, string, uint16, time.Duration) ([]Claim, error) {
	return nil, nil
}
func (s *externalDispatchStore) Renew(context.Context, Claim, time.Duration) (Claim, error) {
	return Claim{}, nil
}
func (s *externalDispatchStore) BeginExternalAttempt(_ context.Context, claim Claim, id string, key contract.IdempotencyKey) error {
	s.started, s.claim, s.ident, s.key = true, claim, id, key
	return nil
}
func (s *externalDispatchStore) CommitStep(_ context.Context, change StepCommit) error {
	s.commit = &change
	return nil
}
func (s *externalDispatchStore) Release(context.Context, Claim) error { return nil }

type externalDispatchSchemas struct{}

func (externalDispatchSchemas) Registered(ref contract.SchemaRef) bool {
	return ref.ID == "input" || ref.ID == "output"
}
func (externalDispatchSchemas) Validate(ref contract.SchemaRef, data json.RawMessage) error {
	if !json.Valid(data) || string(data) != `{"ok":true}` {
		return errors.New("payload rejected")
	}
	return nil
}

type orderedExternalAction struct {
	store    *externalDispatchStore
	err      error
	called   bool
	gotKey   contract.IdempotencyKey
	gotActor contract.PrincipalType
}

// responseLostExternalAction models a provider that accepts an effect but
// loses the response before the caller can learn its outcome. It deliberately
// returns the raw transport error so the executor must preserve uncertainty.
type responseLostExternalAction struct {
	client   *http.Client
	endpoint string
}

func (a responseLostExternalAction) Execute(ctx context.Context, action ActionContext, input ActionInput) (ActionResult, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, bytes.NewReader(input.Data))
	if err != nil {
		return ActionResult{}, err
	}
	request.Header.Set("Idempotency-Key", string(action.IdempotencyKey))
	response, err := a.client.Do(request)
	if err != nil {
		return ActionResult{}, err
	}
	defer response.Body.Close()
	return ActionResult{}, errors.New("test provider unexpectedly returned a response")
}

func (a *orderedExternalAction) Execute(_ context.Context, action ActionContext, _ ActionInput) (ActionResult, error) {
	a.called = true
	a.gotKey = action.IdempotencyKey
	a.gotActor = action.Execution.Actor().PrincipalType
	if !a.store.started {
		return ActionResult{}, errors.New("attempt was not durable before effect")
	}
	return ActionResult{Schema: contract.SchemaRef{ID: "output", Version: 1}, Data: json.RawMessage(`{"ok":true}`)}, a.err
}

func newExternalDispatch(t *testing.T, action Action) (*ExternalActionExecutor, *externalDispatchStore, ActionRegistration) {
	t.Helper()
	store := &externalDispatchStore{}
	identity, err := NewTenantServiceIdentityProvider("tenant-a", contract.ActorContext{
		PrincipalID: "workflow-runtime", PrincipalType: contract.PrincipalService,
		Site: "tenant-a.example.test", AuthenticatedAt: time.Now().UTC(),
	}, []policy.Grant{{Resource: "Invoice", Operation: "submit"}, testActionGrant()})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewExternalActionExecutor(store, identity, externalDispatchSchemas{})
	if err != nil {
		t.Fatal(err)
	}
	registration := ActionRegistration{Key: ActionKey{Name: "invoice.submit", Version: 2}, Class: ActionExternal, InputSchema: contract.SchemaRef{ID: "input", Version: 1}, OutputSchema: contract.SchemaRef{ID: "output", Version: 1}, RequiredGrant: testActionGrant(), External: action}
	if ordered, ok := action.(*orderedExternalAction); ok {
		ordered.store = store
	}
	return executor, store, registration
}

func externalClaim() Claim {
	return Claim{Tenant: "tenant-a", BusinessScope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}, InstanceID: "instance-1", StepID: "submit", Attempt: 1, Owner: "worker-1", Fence: 3, Revision: "7"}
}

func externalDefinitionAndStep() (contract.WorkflowDefinition, contract.StepDefinition) {
	step := contract.StepDefinition{ID: "submit", Kind: contract.StepAction, Action: &contract.ActionSpec{Name: "invoice.submit", Version: 2, InputSchema: "input", Input: json.RawMessage(`{"ok":true}`), Next: "done"}}
	return contract.WorkflowDefinition{ID: "invoice-flow", Version: 1}, step
}

func TestExternalActionPersistsAttemptBeforeEffectAndUsesStableIdentity(t *testing.T) {
	store := &externalDispatchStore{}
	action := &orderedExternalAction{store: store}
	executor, store, registration := newExternalDispatch(t, action)
	definition, step := externalDefinitionAndStep()
	change, err := executor.Execute(context.Background(), externalClaim(), definition, step, registration)
	if err != nil {
		t.Fatal("execute external action:", err)
	}
	if !action.called || !store.started || change.Status != contract.InstanceReady || change.NextStep != "done" || !change.AttemptStarted {
		t.Fatalf("unexpected dispatch: called=%t started=%t change=%+v", action.called, store.started, change)
	}
	if change.ExecutionID != store.ident || change.IdempotencyKey != store.key || change.IdempotencyKey != action.gotKey || action.gotActor != contract.PrincipalService {
		t.Fatalf("dispatch identities differ: change=%+v persisted=%q/%q action=%q actor=%s", change, store.ident, store.key, action.gotKey, action.gotActor)
	}
	secondID, secondKey := stableExternalActionIdentity("tenant-a", "instance-1", "submit")
	if secondID != change.ExecutionID || secondKey != change.IdempotencyKey {
		t.Fatal("external execution and idempotency identities are not stable across retries")
	}
	if len(change.ExecutionID) != 64 {
		t.Fatalf("execution ID length=%d, want 64 for MySQL VARCHAR(64)", len(change.ExecutionID))
	}
	if len(change.Events) != 1 || change.Events[0].Type != "kora.workflow.advanced" {
		t.Fatalf("successful action event = %+v", change.Events)
	}
}

func TestExternalActionRejectsMissingRegisteredGrantBeforeAttempt(t *testing.T) {
	store := &externalDispatchStore{}
	action := &orderedExternalAction{store: store}
	identity, err := NewTenantServiceIdentityProvider("tenant-a", contract.ActorContext{
		PrincipalID: "workflow-runtime", PrincipalType: contract.PrincipalService,
		Site: "tenant-a.example.test", AuthenticatedAt: time.Now().UTC(),
	}, nil)
	if err != nil {
		t.Fatal("construct identity:", err)
	}
	executor, err := NewExternalActionExecutor(store, identity, externalDispatchSchemas{})
	if err != nil {
		t.Fatal("construct executor:", err)
	}
	registration := ActionRegistration{Key: ActionKey{Name: "invoice.submit", Version: 2}, Class: ActionExternal,
		InputSchema: contract.SchemaRef{ID: "input", Version: 1}, OutputSchema: contract.SchemaRef{ID: "output", Version: 1},
		RequiredGrant: testActionGrant(), External: action}
	definition, step := externalDefinitionAndStep()
	if _, err := executor.Execute(context.Background(), externalClaim(), definition, step, registration); err == nil {
		t.Fatal("external workflow action executed without its registered grant")
	}
	if action.called || store.started {
		t.Fatalf("missing grant caused side effects: action_called=%t attempt_started=%t", action.called, store.started)
	}
}

func TestExternalActionErrorBecomesRecoverableUnknownOutcome(t *testing.T) {
	store := &externalDispatchStore{}
	action := &orderedExternalAction{store: store, err: errors.New("provider response lost")}
	executor, _, registration := newExternalDispatch(t, action)
	definition, step := externalDefinitionAndStep()
	change, err := executor.Execute(context.Background(), externalClaim(), definition, step, registration)
	if err != nil {
		t.Fatal("prepare unknown outcome transition:", err)
	}
	if !action.called || !change.AttemptStarted || change.Status != contract.InstanceUncertain || change.Failure == "provider response lost" {
		t.Fatalf("provider error was not safely classified: %+v", change)
	}
	if len(change.Events) != 1 || change.Events[0].Type != "kora.workflow.outcome_unknown" {
		t.Fatalf("uncertain action event = %+v", change.Events)
	}
}

func TestExternalActionResponseLostAfterProviderEffectStaysUncertain(t *testing.T) {
	var effectCount int
	var providerKey string
	providerFinished := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(providerFinished)
		effectCount++ // The provider applies the effect before losing its reply.
		providerKey = r.Header.Get("Idempotency-Key")
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("test HTTP server does not support connection hijacking")
			return
		}
		connection, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack provider response: %v", err)
			return
		}
		_ = connection.Close()
	}))
	defer provider.Close()

	transportAction := responseLostExternalAction{client: provider.Client(), endpoint: provider.URL}
	executor, store, registration := newExternalDispatch(t, transportAction)
	definition, step := externalDefinitionAndStep()
	change, err := executor.Execute(context.Background(), externalClaim(), definition, step, registration)
	if err != nil {
		t.Fatal("prepare uncertain external transition:", err)
	}
	select {
	case <-providerFinished:
	case <-time.After(time.Second):
		t.Fatal("provider did not finish the response-loss scenario")
	}
	if effectCount != 1 {
		t.Fatalf("provider effect count = %d, want exactly one observed effect", effectCount)
	}
	if !store.started || providerKey == "" || providerKey != string(store.key) {
		t.Fatalf("provider key %q does not match persisted idempotency key %q (started=%t)", providerKey, store.key, store.started)
	}
	if change.Status != contract.InstanceUncertain || change.NextStep != "" || len(change.Events) != 1 || change.Events[0].Type != "kora.workflow.outcome_unknown" {
		t.Fatalf("lost provider response was not preserved as an operator-visible uncertain outcome: %+v", change)
	}
}

func TestExternalActionOnlyRetriesWhenAdapterProvesNoEffect(t *testing.T) {
	store := &externalDispatchStore{}
	action := &orderedExternalAction{store: store, err: &ExternalActionFailure{Kind: ExternalNotAppliedRetryable, Cause: errors.New("provider rejected before processing")}}
	executor, _, registration := newExternalDispatch(t, action)
	definition, step := externalDefinitionAndStep()
	change, err := executor.Execute(context.Background(), externalClaim(), definition, step, registration)
	if err != nil {
		t.Fatal("prepare safe retry:", err)
	}
	if change.Status != contract.InstanceRetrying || change.NextStep != "submit" || change.NextWakeAt == nil || !change.AttemptStarted || change.Failure == "provider rejected before processing" {
		t.Fatalf("not-applied failure did not schedule bounded retry: %+v", change)
	}
	if delay := time.Until(*change.NextWakeAt); delay < 700*time.Millisecond || delay > 1300*time.Millisecond {
		t.Fatalf("first retry delay = %s", delay)
	}

	store = &externalDispatchStore{}
	action = &orderedExternalAction{store: store, err: &ExternalActionFailure{Kind: ExternalNotAppliedRetryable, Cause: errors.New("provider rejected before processing")}}
	executor, _, registration = newExternalDispatch(t, action)
	definition, step = externalDefinitionAndStep()
	claim := externalClaim()
	claim.Attempt = maxExternalActionAttempts
	change, err = executor.Execute(context.Background(), claim, definition, step, registration)
	if err != nil {
		t.Fatal("prepare retry-limit terminal state:", err)
	}
	if change.Status != contract.InstanceFailed || change.NextWakeAt != nil || change.NextStep != "" || change.Failure == "" {
		t.Fatalf("retry limit did not fail safely: %+v", change)
	}

	store = &externalDispatchStore{}
	action = &orderedExternalAction{store: store, err: &ExternalActionFailure{Kind: ExternalNotAppliedRetryable, Cause: errors.New("provider rejected before processing")}}
	executor, _, registration = newExternalDispatch(t, action)
	definition, step = externalDefinitionAndStep()
	claim = externalClaim()
	claim.Attempt = 2
	claim.FirstAttemptAt = time.Now().UTC().Add(-maxExternalActionElapsed - time.Minute)
	change, err = executor.Execute(context.Background(), claim, definition, step, registration)
	if err != nil {
		t.Fatal("prepare elapsed-window terminal state:", err)
	}
	if change.Status != contract.InstanceFailed || change.NextWakeAt != nil || change.NextStep != "" {
		t.Fatalf("elapsed retry window did not fail safely: %+v", change)
	}
}

func TestExternalActionValidatesBeforePersistingOrCalling(t *testing.T) {
	action := &orderedExternalAction{store: &externalDispatchStore{}}
	executor, store, registration := newExternalDispatch(t, action)
	definition, step := externalDefinitionAndStep()
	step.Action.Input = json.RawMessage(`{"wrong":true}`)
	if _, err := executor.Execute(context.Background(), externalClaim(), definition, step, registration); err == nil {
		t.Fatal("invalid registered input was accepted")
	}
	if store.started || action.called {
		t.Fatalf("invalid input crossed the effect boundary: started=%t called=%t", store.started, action.called)
	}
}

func TestPinnedCapabilityDispatchIdentityIsStableAndHashesInput(t *testing.T) {
	action := contract.CapabilityAction{CapabilityName: "billing.issue", OperationName: "IssueInvoice", InputSchema: contract.SchemaRef{ID: "invoice.issue.in", Version: 1}, OutputSchema: contract.SchemaRef{ID: "invoice.issue.out", Version: 1}}
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	first, err := NewPinnedDispatch("execution-a", scope, "sales.orders", 8, "billing-provider-a", action, "idem-a", json.RawMessage(`{"amount":12}`))
	if err != nil {
		t.Fatal(err)
	}
	retry, err := NewPinnedDispatch("execution-a", scope, "sales.orders", 8, "billing-provider-a", action, "idem-a", json.RawMessage(`{"amount":12}`))
	if err != nil {
		t.Fatal(err)
	}
	if first != retry {
		t.Fatalf("retry identity changed: %#v %#v", first, retry)
	}
	changed, err := NewPinnedDispatch("execution-a", scope, "sales.orders", 8, "billing-provider-a", action, "idem-a", json.RawMessage(`{"amount":13}`))
	if err != nil {
		t.Fatal(err)
	}
	if first.InputHash == changed.InputHash {
		t.Fatal("changed request payload retained input hash")
	}
	rebound, err := NewPinnedDispatch("execution-a", scope, "sales.orders", 9, "billing-provider-b", action, "idem-a", json.RawMessage(`{"amount":12}`))
	if err != nil {
		t.Fatal(err)
	}
	if first.ProviderID == rebound.ProviderID || first.BindingRevision == rebound.BindingRevision {
		t.Fatal("provider rebind not represented")
	}
}
