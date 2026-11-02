package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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
	pinned  *PinnedCapabilityAttempt
	commit  *StepCommit
}

func (s *externalDispatchStore) ClaimDue(context.Context, contract.TenantID, string, uint16, time.Duration) ([]Claim, error) {
	return nil, nil
}
func (s *externalDispatchStore) Renew(context.Context, Claim, time.Duration) (Claim, error) {
	return Claim{}, nil
}
func (s *externalDispatchStore) BeginExternalAttempt(_ context.Context, claim Claim, id string, key contract.IdempotencyKey, requested *contract.PinnedDispatch) (*contract.PinnedDispatch, error) {
	s.started, s.claim, s.ident, s.key = true, claim, id, key
	if requested != nil {
		return nil, errors.New("test store requires a durable lease for pinned capabilities")
	}
	return nil, nil
}
func (s *externalDispatchStore) BeginPinnedExternalAttempt(_ context.Context, claim Claim, id string, key contract.IdempotencyKey, requested contract.PinnedDispatch, lease contract.CapabilityLease) (*PinnedCapabilityAttempt, error) {
	s.started, s.claim, s.ident, s.key = true, claim, id, key
	leaseAttempt := PinnedCapabilityAttempt{Dispatch: requested, Lease: lease}
	if s.pinned != nil {
		leaseAttempt = *s.pinned
	} else {
		s.pinned = &leaseAttempt
	}
	return &leaseAttempt, nil
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

type pinnedDispatchCapture struct {
	dispatches []contract.PinnedDispatch
	outcome    contract.ExternalOutcome
	err        error
}

type testCapabilityLeaseAdmission struct {
	leases []contract.CapabilityLease
	err    error
}

func (g *testCapabilityLeaseAdmission) AdmitCapabilityAttempt(_ context.Context, lease contract.CapabilityLease, _ time.Time) error {
	g.leases = append(g.leases, lease)
	return g.err
}

func (p *pinnedDispatchCapture) Dispatch(_ context.Context, dispatch contract.PinnedDispatch, _ contract.CapabilityLease, _ ActionRegistration, _ json.RawMessage) (ActionResult, error) {
	p.dispatches = append(p.dispatches, dispatch)
	return ActionResult{Schema: contract.SchemaRef{ID: "output", Version: 1}, Data: json.RawMessage(`{"ok":true}`)}, p.err
}
func (p *pinnedDispatchCapture) Reconcile(_ context.Context, dispatch contract.PinnedDispatch) (contract.ExternalOutcome, ActionResult, error) {
	p.dispatches = append(p.dispatches, dispatch)
	return p.outcome, ActionResult{}, nil
}

type testCredentialResolver struct {
	value string
	err   error
}

func (r testCredentialResolver) ResolveCredential(context.Context, string) (string, error) {
	return r.value, r.err
}

func TestHTTPSCapabilityDispatcherUsesPinnedIdentityAndSecretReference(t *testing.T) {
	var calls int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer provider-secret" || r.Header.Get("Idempotency-Key") != "stable-key" || r.Header.Get("X-Kora-Provider") != "billing.provider-a" || r.Header.Get("X-Kora-Operation") != "issue" {
			t.Errorf("provider request headers missing pinned identity or credential")
		}
		_, _ = w.Write([]byte(`{"schema":{"id":"output","version":1},"data":{"ok":true}}`))
	}))
	defer server.Close()
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	dispatcher := &HTTPSCapabilityDispatcher{ProviderID: "billing.provider-a", Endpoint: server.URL, Client: client}
	lease := contract.CapabilityLease{ID: "lease-a", TenantID: "tenant-a", Scope: externalClaim().BusinessScope, CallerModuleID: "sales.orders", ProviderID: "billing.provider-a", Operation: "issue", BindingRevision: 8, GrantEpoch: 1, ExpiresAt: time.Now().Add(time.Hour), State: "active", CredentialRef: "billing-token@version-a"}
	registration := ActionRegistration{ProviderID: "billing.provider-a", CredentialResolver: testCredentialResolver{value: "provider-secret"}}
	input := json.RawMessage(`{"ok":true}`)
	pin := contract.PinnedDispatch{ExecutionID: "execution-a", Scope: lease.Scope, CallerModuleID: lease.CallerModuleID, ProviderID: lease.ProviderID, BindingRevision: lease.BindingRevision, Operation: contract.CapabilityAction{CapabilityName: "billing.invoice", OperationName: "issue", InputSchema: contract.SchemaRef{ID: "input", Version: 1}, OutputSchema: contract.SchemaRef{ID: "output", Version: 1}}, IdempotencyKey: "stable-key", InputHash: sha256Hex(input)}
	result, err := dispatcher.Dispatch(context.Background(), pin, lease, registration, input)
	if err != nil || result.Schema.ID != "output" || calls != 1 {
		t.Fatalf("dispatch result=%+v err=%v calls=%d", result, err, calls)
	}
	registration.CredentialResolver = testCredentialResolver{err: errors.New("stale secret reference")}
	if _, err := dispatcher.Dispatch(context.Background(), pin, lease, registration, input); err == nil || calls != 1 {
		t.Fatalf("stale credential reference reached provider: err=%v calls=%d", err, calls)
	}
}

func TestHTTPSCapabilityDispatcherRedactsAuthenticationFailure(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("token echoed provider-secret"))
	}))
	defer server.Close()
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	dispatcher := &HTTPSCapabilityDispatcher{ProviderID: "billing.provider-a", Endpoint: server.URL, Client: client}
	lease := contract.CapabilityLease{ID: "lease-a", TenantID: "tenant-a", Scope: externalClaim().BusinessScope, CallerModuleID: "sales.orders", ProviderID: "billing.provider-a", Operation: "issue", BindingRevision: 8, GrantEpoch: 1, ExpiresAt: time.Now().Add(time.Hour), State: "active", CredentialRef: "billing-token@version-a"}
	registration := ActionRegistration{ProviderID: "billing.provider-a", CredentialResolver: testCredentialResolver{value: "provider-secret"}}
	input := json.RawMessage(`{"ok":true}`)
	pin := contract.PinnedDispatch{ExecutionID: "execution-a", Scope: lease.Scope, CallerModuleID: lease.CallerModuleID, ProviderID: lease.ProviderID, BindingRevision: lease.BindingRevision, Operation: contract.CapabilityAction{CapabilityName: "billing.invoice", OperationName: "issue", InputSchema: contract.SchemaRef{ID: "input", Version: 1}, OutputSchema: contract.SchemaRef{ID: "output", Version: 1}}, IdempotencyKey: "stable-key", InputHash: sha256Hex(input)}
	_, err := dispatcher.Dispatch(context.Background(), pin, lease, registration, input)
	var failure *ExternalActionFailure
	if !errors.As(err, &failure) || failure.Kind != ExternalNotAppliedPermanent || strings.Contains(err.Error(), "provider-secret") || strings.Contains(err.Error(), "token echoed") {
		t.Fatalf("authentication error was not typed/redacted: %v", err)
	}
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
	executor, err := NewExternalActionExecutor(store, identity, externalDispatchSchemas{}, &testCapabilityLeaseAdmission{})
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

func TestPinnedCapabilityRetryUsesPersistedProviderAfterRebind(t *testing.T) {
	executor, store, registration := newExternalDispatch(t, nil)
	dispatcher := &pinnedDispatchCapture{}
	registration.External = nil
	registration.Capability = &contract.CapabilityAction{CapabilityName: "billing.invoice", OperationName: "create_obligation", InputSchema: registration.InputSchema, OutputSchema: registration.OutputSchema}
	registration.ProviderID = "billing.provider-a"
	registration.BindingRevision = 8
	registration.CallerModuleID = "sales.orders"
	registration.CapabilityLease = &contract.CapabilityLease{ID: "lease-provider-a", TenantID: "tenant-a", Scope: externalClaim().BusinessScope, CallerModuleID: "sales.orders", ProviderID: "billing.provider-a", Operation: "create_obligation", BindingRevision: 8, GrantEpoch: 1, ExpiresAt: time.Now().UTC().Add(time.Hour), State: "active"}
	registration.PinnedDispatcher = dispatcher
	definition, step := externalDefinitionAndStep()
	first, err := executor.Execute(context.Background(), externalClaim(), definition, step, registration)
	if err != nil || first.Status != contract.InstanceReady {
		t.Fatalf("first pinned execution = (%+v,%v)", first, err)
	}
	registration.ProviderID = "billing.provider-b"
	registration.BindingRevision = 9
	registration.CapabilityLease = &contract.CapabilityLease{ID: "lease-provider-b", TenantID: "tenant-a", Scope: externalClaim().BusinessScope, CallerModuleID: "sales.orders", ProviderID: "billing.provider-b", Operation: "create_obligation", BindingRevision: 9, GrantEpoch: 1, ExpiresAt: time.Now().UTC().Add(time.Hour), State: "active"}
	claim := externalClaim()
	claim.Attempt = 2
	second, err := executor.Execute(context.Background(), claim, definition, step, registration)
	if err != nil || second.Status != contract.InstanceReady {
		t.Fatalf("rebound retry = (%+v,%v)", second, err)
	}
	if len(dispatcher.dispatches) != 2 || dispatcher.dispatches[0].ProviderID != "billing.provider-a" || dispatcher.dispatches[1].ProviderID != "billing.provider-a" || dispatcher.dispatches[1].BindingRevision != 8 {
		t.Fatalf("retry silently rerouted pinned dispatch: %+v", dispatcher.dispatches)
	}
	if store.pinned == nil || store.pinned.Dispatch.ProviderID != "billing.provider-a" {
		t.Fatalf("durable dispatch pin = %+v", store.pinned)
	}
	gate := executor.leases.(*testCapabilityLeaseAdmission)
	if len(gate.leases) != 2 || gate.leases[0].ProviderID != "billing.provider-a" || gate.leases[1].ProviderID != "billing.provider-a" || gate.leases[1].ID != "lease-provider-a" {
		t.Fatalf("retry lease authorization followed new binding instead of stored pin: %+v", gate.leases)
	}
}

func TestPinnedCapabilityLeaseDenialPreventsProviderAttempt(t *testing.T) {
	executor, store, registration := newExternalDispatch(t, nil)
	dispatcher := &pinnedDispatchCapture{}
	registration.External = nil
	registration.Capability = &contract.CapabilityAction{CapabilityName: "billing.invoice", OperationName: "create_obligation", InputSchema: registration.InputSchema, OutputSchema: registration.OutputSchema}
	registration.ProviderID = "billing.provider-a"
	registration.BindingRevision = 8
	registration.CallerModuleID = "sales.orders"
	registration.CapabilityLease = &contract.CapabilityLease{ID: "lease-provider-a", TenantID: "tenant-a", Scope: externalClaim().BusinessScope, CallerModuleID: "sales.orders", ProviderID: "billing.provider-a", Operation: "create_obligation", BindingRevision: 8, GrantEpoch: 3, ExpiresAt: time.Now().UTC().Add(time.Hour), State: "active"}
	registration.PinnedDispatcher = dispatcher
	executor.leases.(*testCapabilityLeaseAdmission).err = contract.NewError(contract.CodePermissionDenied, "revoked")
	definition, step := externalDefinitionAndStep()
	change, err := executor.Execute(context.Background(), externalClaim(), definition, step, registration)
	if err != nil {
		t.Fatal("prepare denied lease retry transition:", err)
	}
	if change.Status != contract.InstanceFailed || !store.started || len(dispatcher.dispatches) != 0 {
		t.Fatalf("revoked capability was sent to provider: change=%+v started=%t dispatches=%d", change, store.started, len(dispatcher.dispatches))
	}
}

func TestPinnedProviderAuthenticationFailureIsExplicitAndRedacted(t *testing.T) {
	executor, _, registration := newExternalDispatch(t, nil)
	dispatcher := &pinnedDispatchCapture{err: &ExternalActionFailure{Kind: ExternalNotAppliedPermanent, Cause: errors.New("401 bearer credential-secret")}}
	registration.External = nil
	registration.Capability = &contract.CapabilityAction{CapabilityName: "billing.invoice", OperationName: "create_obligation", InputSchema: registration.InputSchema, OutputSchema: registration.OutputSchema}
	registration.ProviderID = "billing.provider-a"
	registration.BindingRevision = 8
	registration.CallerModuleID = "sales.orders"
	registration.CapabilityLease = &contract.CapabilityLease{ID: "lease-provider-a", TenantID: "tenant-a", Scope: externalClaim().BusinessScope, CallerModuleID: "sales.orders", ProviderID: "billing.provider-a", Operation: "create_obligation", BindingRevision: 8, GrantEpoch: 1, ExpiresAt: time.Now().UTC().Add(time.Hour), State: "active"}
	registration.PinnedDispatcher = dispatcher
	definition, step := externalDefinitionAndStep()
	change, err := executor.Execute(context.Background(), externalClaim(), definition, step, registration)
	if err != nil {
		t.Fatal("prepare provider authentication failure transition:", err)
	}
	if change.Status != contract.InstanceFailed || strings.Contains(change.Failure, "credential-secret") || change.Failure == "" {
		t.Fatalf("provider authentication failure was not explicitly and safely recorded: %+v", change)
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
	executor, err := NewExternalActionExecutor(store, identity, externalDispatchSchemas{}, nil)
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
	lease := contract.CapabilityLease{ID: "lease-a", TenantID: scope.TenantID, Scope: scope, CallerModuleID: "sales.orders", ProviderID: "billing-provider-a", Operation: action.OperationName, BindingRevision: 8, GrantEpoch: 1, ExpiresAt: time.Now().UTC().Add(time.Hour), State: "active"}
	first, err := NewPinnedDispatch("execution-a", scope, "sales.orders", 8, "billing-provider-a", lease, action, "idem-a", json.RawMessage(`{"amount":12}`))
	if err != nil {
		t.Fatal(err)
	}
	retry, err := NewPinnedDispatch("execution-a", scope, "sales.orders", 8, "billing-provider-a", lease, action, "idem-a", json.RawMessage(`{"amount":12}`))
	if err != nil {
		t.Fatal(err)
	}
	if first != retry {
		t.Fatalf("retry identity changed: %#v %#v", first, retry)
	}
	changed, err := NewPinnedDispatch("execution-a", scope, "sales.orders", 8, "billing-provider-a", lease, action, "idem-a", json.RawMessage(`{"amount":13}`))
	if err != nil {
		t.Fatal(err)
	}
	if first.InputHash == changed.InputHash {
		t.Fatal("changed request payload retained input hash")
	}
	reboundLease := contract.CapabilityLease{ID: "lease-b", TenantID: scope.TenantID, Scope: scope, CallerModuleID: "sales.orders", ProviderID: "billing-provider-b", Operation: action.OperationName, BindingRevision: 9, GrantEpoch: 1, ExpiresAt: lease.ExpiresAt, State: "active"}
	rebound, err := NewPinnedDispatch("execution-a", scope, "sales.orders", 9, "billing-provider-b", reboundLease, action, "idem-a", json.RawMessage(`{"amount":12}`))
	if err != nil {
		t.Fatal(err)
	}
	if first.ProviderID == rebound.ProviderID || first.BindingRevision == rebound.BindingRevision {
		t.Fatal("provider rebind not represented")
	}
}
