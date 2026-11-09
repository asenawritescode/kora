package workflow

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/expression"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/requests"
)

type claimHandlerDefinitions struct {
	definition contract.WorkflowDefinition
	ref        contract.DefinitionVersionRef
}

func (r claimHandlerDefinitions) PublishDefinition(context.Context, contract.TenantID, contract.WorkflowDefinition, contract.PrincipalID, contract.IdempotencyKey, time.Time) (contract.DefinitionVersionRef, error) {
	return r.ref, nil
}
func (r claimHandlerDefinitions) GetDefinition(context.Context, contract.TenantID, contract.DefinitionID, uint32) (contract.WorkflowDefinition, contract.DefinitionVersionRef, error) {
	return r.definition, r.ref, nil
}

type claimHandlerInstances struct{ instance contract.WorkflowInstance }

func (r claimHandlerInstances) StartInstance(context.Context, StartInstanceInput) (contract.WorkflowInstance, error) {
	return r.instance, nil
}
func (r claimHandlerInstances) GetInstance(context.Context, contract.TenantID, contract.InstanceID) (contract.WorkflowInstance, error) {
	return r.instance, nil
}

type claimHandlerScope struct{ value expression.Value }

func (s claimHandlerScope) Lookup(path string) (expression.Value, bool) {
	return s.value, path == "amount"
}
func (claimHandlerScope) Aggregate(string, string, string) (expression.Value, error) {
	return expression.NullValue(), nil
}

type claimHandlerScopes struct{ scope expression.Scope }

func (p claimHandlerScopes) Scope(context.Context, policy.Execution, contract.WorkflowInstance) (expression.Scope, error) {
	return p.scope, nil
}

type countingConditionScope struct {
	calls int
	scope expression.Scope
}

func (p *countingConditionScope) Scope(context.Context, policy.Execution, contract.WorkflowInstance) (expression.Scope, error) {
	p.calls++
	return p.scope, nil
}

type failedConditionScope struct{ err error }

func (p failedConditionScope) Scope(context.Context, policy.Execution, contract.WorkflowInstance) (expression.Scope, error) {
	return nil, p.err
}

type claimHandlerHumanRequests struct {
	command contract.SubmitRequest
	source  contract.RequestSource
}

func (p *claimHandlerHumanRequests) PrepareWorkflowSubmit(_ context.Context, _ policy.Execution, command contract.SubmitRequest, source contract.RequestSource) (*requests.PreparedSubmission, error) {
	p.command, p.source = command, source
	return &requests.PreparedSubmission{}, nil
}

type claimHandlerHumanStore struct{ change *StepCommit }

func (s *claimHandlerHumanStore) CreateHumanWait(_ context.Context, _ *requests.PreparedSubmission, change StepCommit) (requests.RequestRecord, error) {
	s.change = &change
	return requests.RequestRecord{}, nil
}

type failingLocalWorkflowAction struct{ err error }

func (a failingLocalWorkflowAction) ExecuteLocal(context.Context, Claim, ActionContext, ActionKey, ActionInput, contract.StepID) (ActionResult, error) {
	return ActionResult{}, a.err
}

func newDefinitionHandler(t *testing.T, store *externalDispatchStore, definition contract.WorkflowDefinition, stepScope expression.Scope) (*DefinitionClaimHandler, ActionRegistration) {
	return newDefinitionHandlerWithGrants(t, store, definition, stepScope, []policy.Grant{
		{Resource: "Invoice", Operation: "read"},
		{Resource: "Invoice", Operation: "submit"},
		testActionGrant(),
	})
}

func TestWorkflowActionGrantIsBoundToClaimScopeAndCurrentIdentity(t *testing.T) {
	scopeA := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	scopeB := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-b"}
	actor := contract.ActorContext{PrincipalID: "workflow-runtime", PrincipalType: contract.PrincipalService, Site: "tenant-a.example.test", AuthenticatedAt: time.Now().UTC()}
	required := policy.Grant{Resource: "Invoice", Operation: "submit"}
	current, err := policy.NewVerifiedExecutionWithGrants("tenant-a", actor, []policy.Grant{{Resource: "Invoice", Operation: "submit", Scope: scopeA}})
	if err != nil {
		t.Fatal(err)
	}
	if !workflowGrantAllows(current, required, scopeA) {
		t.Fatal("exact entity grant was rejected")
	}
	if workflowGrantAllows(current, required, scopeB) {
		t.Fatal("entity A grant authorized entity B workflow")
	}
	if workflowGrantAllows(current, required, contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}) {
		t.Fatal("entity grant authorized tenant-scoped workflow")
	}
	revoked, err := policy.NewVerifiedExecutionWithGrants("tenant-a", actor, nil)
	if err != nil {
		t.Fatal(err)
	}
	if workflowGrantAllows(revoked, required, scopeA) {
		t.Fatal("resumed workflow continued after its scoped grant was revoked")
	}
}

func newDefinitionHandlerWithGrants(t *testing.T, store *externalDispatchStore, definition contract.WorkflowDefinition, stepScope expression.Scope, grants []policy.Grant) (*DefinitionClaimHandler, ActionRegistration) {
	t.Helper()
	instances := claimHandlerInstances{instance: contract.WorkflowInstance{
		ID: "instance-1", Revision: "7", DefinitionID: definition.ID, DefinitionVersion: definition.Version,
		Subject: contract.SubjectRef{Kind: "Invoice", ID: "INV-1", Revision: "4"}, Status: contract.InstanceRunning,
		BusinessScope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}, CurrentStep: definition.Steps[0].ID,
	}}
	defs := claimHandlerDefinitions{definition: definition, ref: contract.DefinitionVersionRef{DefinitionID: definition.ID, Version: definition.Version, Digest: "digest-1"}}
	identity, err := NewTenantServiceIdentityProvider("tenant-a", contract.ActorContext{
		PrincipalID: "workflow-runtime", PrincipalType: contract.PrincipalService,
		Site: "tenant-a.example.test", AuthenticatedAt: time.Now().UTC(),
	}, grants)
	if err != nil {
		t.Fatal(err)
	}
	action := &orderedExternalAction{store: store}
	registration := ActionRegistration{Key: ActionKey{Name: "invoice.submit", Version: 2}, Class: ActionExternal, InputSchema: contract.SchemaRef{ID: "input", Version: 1}, OutputSchema: contract.SchemaRef{ID: "output", Version: 1}, RequiredGrant: testActionGrant(), External: action}
	catalog := NewCatalog()
	if err := catalog.Register(registration); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Freeze(); err != nil {
		t.Fatal(err)
	}
	handler, err := NewDefinitionClaimHandler(store, instances, defs, catalog, externalDispatchSchemas{}, identity, claimHandlerScopes{scope: stepScope}, expression.NewTreeEvaluator(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return handler, registration
}

func TestDefinitionClaimHandlerRequiresUnscopedSubjectReadGrantForConditions(t *testing.T) {
	condition := contract.StepDefinition{ID: "branch", Kind: contract.StepCondition, Condition: &contract.ConditionSpec{Expression: "amount > 0", IfTrue: "success", IfFalse: "failure"}}
	definition := contract.WorkflowDefinition{ID: "flow", Version: 1, Entry: "branch", Steps: []contract.StepDefinition{condition}}
	claim := externalClaim()
	claim.StepID = "branch"
	for _, tc := range []struct {
		name   string
		grants []policy.Grant
	}{
		{name: "missing", grants: []policy.Grant{testActionGrant()}},
		{name: "owner scoped", grants: []policy.Grant{{Resource: "Invoice", Operation: "read", OwnerOnly: true}, testActionGrant()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &externalDispatchStore{}
			scopes := &countingConditionScope{scope: claimHandlerScope{value: expression.NumberValue(2)}}
			handler, _ := newDefinitionHandlerWithGrants(t, store, definition, scopes.scope, tc.grants)
			handler.conditions = scopes
			if err := handler.HandleClaim(context.Background(), claim); err != nil {
				t.Fatalf("persist terminal authorization failure: %v", err)
			}
			if scopes.calls != 0 || store.commit == nil || store.commit.Status != contract.InstanceFailed {
				t.Fatalf("unauthorized condition reached scope/commit: calls=%d commit=%+v", scopes.calls, store.commit)
			}
		})
	}
}

func TestDefinitionClaimHandlerMakesStaleConditionTerminal(t *testing.T) {
	condition := contract.StepDefinition{ID: "branch", Kind: contract.StepCondition, Condition: &contract.ConditionSpec{Expression: "amount > 0", IfTrue: "success", IfFalse: "failure"}}
	definition := contract.WorkflowDefinition{ID: "flow", Version: 1, Entry: "branch", Steps: []contract.StepDefinition{condition}}
	store := &externalDispatchStore{}
	handler, _ := newDefinitionHandler(t, store, definition, claimHandlerScope{value: expression.NumberValue(2)})
	handler.conditions = failedConditionScope{err: contract.NewError(contract.CodeStaleSubject, "stale")}
	claim := externalClaim()
	claim.StepID = "branch"
	if err := handler.HandleClaim(context.Background(), claim); err != nil {
		t.Fatal("persist stale condition terminal result:", err)
	}
	if store.commit == nil || store.commit.Status != contract.InstanceFailed || store.commit.Failure == "" {
		t.Fatalf("stale condition was not made terminal and visible: %+v", store.commit)
	}
}

func TestDefinitionClaimHandlerLoadsPinnedDefinitionAndCommitsExternalAction(t *testing.T) {
	store := &externalDispatchStore{}
	_, step := externalDefinitionAndStep()
	definition := contract.WorkflowDefinition{ID: "invoice-flow", Version: 3, Entry: step.ID, Steps: []contract.StepDefinition{step}}
	handler, _ := newDefinitionHandler(t, store, definition, claimHandlerScope{value: expression.NumberValue(5)})
	claim := externalClaim()
	if err := handler.HandleClaim(context.Background(), claim); err != nil {
		t.Fatal("handle pinned action claim:", err)
	}
	if !store.started || store.commit == nil || store.commit.Status != contract.InstanceReady || store.commit.NextStep != "done" || !store.commit.AttemptStarted {
		t.Fatalf("pinned action was not durably completed: started=%t commit=%+v", store.started, store.commit)
	}
}

func TestDefinitionClaimHandlerRoutesConditionAndCompletesEnd(t *testing.T) {
	claim := externalClaim()
	claim.StepID = "branch"
	store := &externalDispatchStore{}
	condition := contract.StepDefinition{ID: "branch", Kind: contract.StepCondition, Condition: &contract.ConditionSpec{Expression: "amount > 0", IfTrue: "success", IfFalse: "failure"}}
	conditionDef := contract.WorkflowDefinition{ID: "flow", Version: 1, Entry: "branch", Steps: []contract.StepDefinition{condition}}
	conditionHandler, _ := newDefinitionHandler(t, store, conditionDef, claimHandlerScope{value: expression.NumberValue(2)})
	if err := conditionHandler.HandleClaim(context.Background(), claim); err != nil {
		t.Fatal("handle condition:", err)
	}
	if store.commit == nil || store.commit.NextStep != "success" || store.commit.Status != contract.InstanceReady {
		t.Fatalf("condition route = %+v", store.commit)
	}

	store = &externalDispatchStore{}
	claim.StepID = "finish"
	end := contract.StepDefinition{ID: "finish", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "approved"}}
	endDef := contract.WorkflowDefinition{ID: "flow", Version: 1, Entry: "finish", Steps: []contract.StepDefinition{end}}
	endHandler, _ := newDefinitionHandler(t, store, endDef, claimHandlerScope{value: expression.NumberValue(2)})
	if err := endHandler.HandleClaim(context.Background(), claim); err != nil {
		t.Fatal("handle end:", err)
	}
	if store.commit == nil || store.commit.Status != contract.InstanceCompleted || store.commit.NextStep != "" || !json.Valid(store.commit.Result.Data) || len(store.commit.Events) != 1 || store.commit.Events[0].Type != "kora.workflow.completed" {
		t.Fatalf("end transition = %+v", store.commit)
	}
}

func TestDefinitionClaimHandlerRefusesUnassembledHumanContinuation(t *testing.T) {
	store := &externalDispatchStore{}
	human := contract.StepDefinition{ID: "approve", Kind: contract.StepHuman, Human: &contract.HumanSpec{}}
	definition := contract.WorkflowDefinition{ID: "flow", Version: 1, Entry: "approve", Steps: []contract.StepDefinition{human}}
	handler, _ := newDefinitionHandler(t, store, definition, claimHandlerScope{value: expression.NumberValue(1)})
	claim := externalClaim()
	if err := handler.HandleClaim(context.Background(), claim); err == nil {
		t.Fatal("unassembled human continuation was silently advanced")
	}
	if store.commit != nil || store.started {
		t.Fatalf("human continuation produced effects: %+v", store)
	}
}

func TestDefinitionClaimHandlerCreatesHumanRequestFromPinnedSubjectAndPauses(t *testing.T) {
	store := &externalDispatchStore{}
	step := contract.StepDefinition{ID: "approve", Kind: contract.StepHuman, Human: &contract.HumanSpec{Request: contract.HumanRequestTemplate{
		Kind: contract.RequestTask, Subject: contract.SubjectInstance, Title: "Review invoice", Message: "Please review",
		DueAfterSeconds: 300, Assignment: &contract.AssignmentSpec{Mode: contract.AssignmentAny, Assignees: []contract.Assignee{{Kind: contract.AssigneeUser, ID: "reviewer"}}},
	}}}
	definition := contract.WorkflowDefinition{ID: "flow", Version: 2, Entry: step.ID, Steps: []contract.StepDefinition{step}}
	handler, _ := newDefinitionHandler(t, store, definition, claimHandlerScope{value: expression.NullValue()})
	requests := &claimHandlerHumanRequests{}
	waits := &claimHandlerHumanStore{}
	due := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	handler.human = HumanWaitRuntime{Requests: requests, Store: waits, Now: func() time.Time { return due }}
	claim := externalClaim()
	claim.StepID = step.ID
	if err := handler.HandleClaim(context.Background(), claim); err != nil {
		t.Fatal("handle human wait:", err)
	}
	if requests.source != (contract.RequestSource{InstanceID: claim.InstanceID, StepID: claim.StepID}) || requests.command.Subject == nil || *requests.command.Subject != (contract.SubjectRef{Kind: "Invoice", ID: "INV-1", Revision: "4"}) {
		t.Fatalf("human request identity/source was caller-controlled or lost: command=%+v source=%+v", requests.command, requests.source)
	}
	if requests.command.DueAt == nil || !requests.command.DueAt.Equal(due.Add(5*time.Minute)) || requests.command.Meta.IdempotencyKey == "" || requests.command.Title != "Review invoice" {
		t.Fatalf("human request omitted runtime-derived key/deadline or template fields: %+v", requests.command)
	}
	if waits.change == nil || waits.change.Status != contract.InstanceWaiting || waits.change.NextStep != claim.StepID || waits.change.NextWakeAt == nil || !waits.change.NextWakeAt.Equal(*requests.command.DueAt) || len(waits.change.Events) != 1 || waits.change.Events[0].Type != "kora.workflow.waiting" {
		t.Fatalf("workflow was not paused with matching durable deadline: %+v", waits.change)
	}
}

func TestDefinitionClaimHandlerPersistsTimeoutOnlyWait(t *testing.T) {
	store := &externalDispatchStore{}
	step := contract.StepDefinition{ID: "wait", Kind: contract.StepWait, Wait: &contract.WaitSpec{TimeoutSeconds: 30, OnTimeout: "timed-out"}}
	definition := contract.WorkflowDefinition{ID: "flow", Version: 1, Entry: step.ID, Steps: []contract.StepDefinition{step}}
	handler, _ := newDefinitionHandler(t, store, definition, claimHandlerScope{value: expression.NullValue()})
	claim := externalClaim()
	claim.StepID = step.ID
	started := time.Now().UTC()
	if err := handler.HandleClaim(context.Background(), claim); err != nil {
		t.Fatal("handle timeout wait:", err)
	}
	if store.commit == nil || store.commit.Status != contract.InstanceWaiting || store.commit.NextStep != step.ID || store.commit.NextWakeAt == nil || store.commit.NextWakeAt.Before(started.Add(29*time.Second)) || store.commit.NextWakeAt.After(started.Add(31*time.Second)) {
		t.Fatalf("timeout wait was not durably scheduled: %+v", store.commit)
	}
	if len(store.commit.Events) != 1 || store.commit.Events[0].Type != "kora.workflow.waiting" {
		t.Fatalf("timeout wait event missing: %+v", store.commit.Events)
	}
}

func TestDefinitionClaimHandlerRegistersSubjectBoundEventWait(t *testing.T) {
	store := &externalDispatchStore{}
	step := contract.StepDefinition{ID: "wait", Kind: contract.StepWait, Wait: &contract.WaitSpec{EventType: "invoice.paid", CorrelationField: "invoice_id", TimeoutSeconds: 30, OnEvent: "paid", OnTimeout: "timed-out"}}
	definition := contract.WorkflowDefinition{ID: "flow", Version: 1, Entry: step.ID, Steps: []contract.StepDefinition{step}}
	handler, _ := newDefinitionHandler(t, store, definition, claimHandlerScope{value: expression.NullValue()})
	claim := externalClaim()
	claim.StepID = step.ID
	if err := handler.HandleClaim(context.Background(), claim); err != nil {
		t.Fatalf("handle event wait: %v", err)
	}
	if store.commit == nil || store.commit.Status != contract.InstanceWaiting || store.commit.EventWait == nil {
		t.Fatalf("event wait did not persist a subscription: %+v", store.commit)
	}
	wait := store.commit.EventWait
	if wait.EventType != "invoice.paid" || wait.CorrelationField != "invoice_id" || wait.CorrelationValue != "INV-1" || wait.OnEvent != "paid" {
		t.Fatalf("event wait subscription = %+v", wait)
	}
}

func TestDefinitionClaimHandlerDoesNotBlindlyRetryAmbiguousLocalCommit(t *testing.T) {
	store := &externalDispatchStore{}
	step := contract.StepDefinition{ID: "submit", Kind: contract.StepAction, Action: &contract.ActionSpec{Name: "invoice.local-submit", Version: 1, InputSchema: "input", Input: json.RawMessage(`{"ok":true}`), Next: "done"}}
	definition := contract.WorkflowDefinition{ID: "invoice-flow", Version: 1, Entry: step.ID, Steps: []contract.StepDefinition{step}}
	instance := contract.WorkflowInstance{ID: "instance-1", Revision: "7", DefinitionID: definition.ID, DefinitionVersion: 1, BusinessScope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}, Status: contract.InstanceRunning, CurrentStep: step.ID}
	instances := claimHandlerInstances{instance: instance}
	definitions := claimHandlerDefinitions{definition: definition, ref: contract.DefinitionVersionRef{DefinitionID: definition.ID, Version: 1, Digest: "digest-1"}}
	identity, err := NewTenantServiceIdentityProvider("tenant-a", contract.ActorContext{PrincipalID: "workflow-runtime", PrincipalType: contract.PrincipalService, Site: "tenant-a.example.test", AuthenticatedAt: time.Now().UTC()}, []policy.Grant{testActionGrant()})
	if err != nil {
		t.Fatal(err)
	}
	catalog := NewCatalog()
	registration := ActionRegistration{Key: ActionKey{Name: "invoice.local-submit", Version: 1}, Class: ActionLocal, InputSchema: contract.SchemaRef{ID: "input", Version: 1}, OutputSchema: contract.SchemaRef{ID: "output", Version: 1}, RequiredGrant: testActionGrant(), Local: failingLocalWorkflowAction{err: context.DeadlineExceeded}}
	if err := catalog.Register(registration); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Freeze(); err != nil {
		t.Fatal(err)
	}
	handler, err := NewDefinitionClaimHandler(store, instances, definitions, catalog, externalDispatchSchemas{}, identity, claimHandlerScopes{scope: claimHandlerScope{value: expression.NullValue()}}, expression.NewTreeEvaluator(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.HandleClaim(context.Background(), externalClaim()); err != nil {
		t.Fatal("record uncertain local transaction:", err)
	}
	if store.started || store.commit == nil || store.commit.Status != contract.InstanceUncertain || store.commit.AttemptStarted || store.commit.Failure != "local action transaction outcome could not be confirmed" {
		t.Fatalf("local action error was blindly retryable: started=%t commit=%+v", store.started, store.commit)
	}
}

func TestLocalActionRetryWindowIsBoundedAcrossWorkerRestarts(t *testing.T) {
	claim := externalClaim()
	claim.Attempt = 2
	claim.FirstAttemptAt = time.Now().UTC().Add(-maxExternalActionElapsed - time.Minute)
	change, err := localActionFailureCommit(claim, "01K7EXECUTION000000000000000000", "workflow-action-key-1", &LocalActionFailure{Kind: LocalActionNotAppliedRetryable, Cause: context.DeadlineExceeded})
	if err != nil {
		t.Fatal(err)
	}
	if change.Status != contract.InstanceFailed || change.NextWakeAt != nil || change.Failure == "" {
		t.Fatalf("local action exceeded elapsed retry window: %+v", change)
	}
}
