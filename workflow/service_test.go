package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/policy"
)

type testDefinitionAuthorizer struct{ allowed bool }

func (a testDefinitionAuthorizer) CanManageWorkflowDefinitions(context.Context, policy.Execution) (bool, error) {
	return a.allowed, nil
}

type testDefinitionRepository struct {
	called     bool
	tenant     contract.TenantID
	actor      contract.PrincipalID
	key        contract.IdempotencyKey
	definition contract.WorkflowDefinition
	now        time.Time
	stored     contract.WorkflowDefinition
	storedRef  contract.DefinitionVersionRef
	previews   []PreviewRun
}

type inputOnlySchemas struct{ testSchemas }

func (inputOnlySchemas) Registered(ref contract.SchemaRef) bool { return ref.ID == "input" }

type candidateRecordSchemas map[string]map[string]string

func (s candidateRecordSchemas) RecordTableExists(table string) bool {
	_, ok := s[table]
	return ok
}
func (s candidateRecordSchemas) RecordFieldExists(table, field string) bool {
	_, ok := s.RecordFieldType(table, field)
	return ok
}
func (s candidateRecordSchemas) RecordFieldType(table, field string) (string, bool) {
	fields, ok := s[table]
	if !ok {
		return "", false
	}
	fieldType, ok := fields[field]
	return fieldType, ok
}

func (r *testDefinitionRepository) PublishDefinition(_ context.Context, tenant contract.TenantID, definition contract.WorkflowDefinition, actor contract.PrincipalID, key contract.IdempotencyKey, now time.Time) (contract.DefinitionVersionRef, error) {
	r.called = true
	r.tenant = tenant
	r.actor = actor
	r.key = key
	r.definition = definition
	r.now = now
	return contract.DefinitionVersionRef{DefinitionID: definition.ID, Version: definition.Version, Digest: "digest"}, nil
}
func (r *testDefinitionRepository) GetDefinition(context.Context, contract.TenantID, contract.DefinitionID, uint32) (contract.WorkflowDefinition, contract.DefinitionVersionRef, error) {
	return r.stored, r.storedRef, nil
}
func (r *testDefinitionRepository) ListLatestDefinitions(context.Context, contract.TenantID) ([]contract.DefinitionVersionRef, error) {
	if r.stored.ID == "" {
		return nil, nil
	}
	return []contract.DefinitionVersionRef{r.storedRef}, nil
}
func (r *testDefinitionRepository) RecordPreview(_ context.Context, _ contract.TenantID, _ contract.PrincipalID, run PreviewRun) error {
	r.previews = append(r.previews, run)
	return nil
}
func (r *testDefinitionRepository) ListPreviews(context.Context, contract.TenantID, uint16) ([]PreviewRun, error) {
	return append([]PreviewRun(nil), r.previews...), nil
}
func (r *testDefinitionRepository) HasSuccessfulPreview(_ context.Context, _ contract.TenantID, id contract.DefinitionID, version uint32, digest string) (bool, error) {
	for _, run := range r.previews {
		if run.DefinitionID == id && run.DefinitionVersion == version && run.DefinitionDigest == digest && run.Status == "succeeded" {
			return true, nil
		}
	}
	return false, nil
}

type testInstanceRepository struct {
	input      StartInstanceInput
	instance   contract.WorkflowInstance
	called     bool
	reconciled ReconcileCommit
}

func (r *testInstanceRepository) StartInstance(_ context.Context, input StartInstanceInput) (contract.WorkflowInstance, error) {
	r.called, r.input = true, input
	r.instance = contract.WorkflowInstance{ID: input.ID, Revision: "1", DefinitionID: input.Definition.ID, DefinitionVersion: input.Definition.Version, Subject: input.Subject, Status: contract.InstanceReady, CurrentStep: input.Definition.Entry, CreatedAt: input.Now, UpdatedAt: input.Now}
	return r.instance, nil
}
func (r *testInstanceRepository) GetInstance(context.Context, contract.TenantID, contract.InstanceID) (contract.WorkflowInstance, error) {
	return r.instance, nil
}
func (r *testInstanceRepository) Reconcile(_ context.Context, commit ReconcileCommit) (contract.WorkflowInstance, error) {
	r.called, r.reconciled = true, commit
	r.instance.Status = commit.Status
	r.instance.CurrentStep = commit.NextStep
	return r.instance, nil
}

type testInstanceAuthorizer struct {
	start, read, reconcile                bool
	startCalls, readCalls, reconcileCalls int
}

func (a *testInstanceAuthorizer) CanStartWorkflow(context.Context, policy.Execution, contract.DefinitionID, contract.SubjectRef) (bool, error) {
	a.startCalls++
	return a.start, nil
}
func (a *testInstanceAuthorizer) CanReadWorkflow(context.Context, policy.Execution, contract.SubjectRef) (bool, error) {
	a.readCalls++
	return a.read, nil
}
func (a *testInstanceAuthorizer) CanReconcileWorkflow(context.Context, policy.Execution, contract.WorkflowInstance) (bool, error) {
	a.reconcileCalls++
	return a.reconcile, nil
}

func TestServicePublishesValidatedDefinitionUsingVerifiedIdentity(t *testing.T) {
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	repo := &testDefinitionRepository{}
	exec, err := policy.NewVerifiedExecution("stable-tenant-1", contract.ActorContext{PrincipalID: "user-1", PrincipalType: contract.PrincipalHuman, Site: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repo, testDefinitionAuthorizer{allowed: true}, frozenTestCatalog(t), testSchemas{}, testWaits{}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	definition := validTestDefinition()
	ref, err := service.Publish(context.Background(), exec, definition, "publish-1")
	if err != nil {
		t.Fatal(err)
	}
	if !repo.called || repo.tenant != "stable-tenant-1" || repo.actor != "user-1" || repo.key != "publish-1" || !repo.now.Equal(now) || ref.DefinitionID != definition.ID {
		t.Fatalf("publication did not preserve verified scope: repo=%+v ref=%+v", repo, ref)
	}
}

func TestValidateCandidateUsesProposedTableAndFieldSchema(t *testing.T) {
	service, err := NewService(&testDefinitionRepository{}, testDefinitionAuthorizer{allowed: true}, frozenTestCatalog(t), testSchemas{}, testWaits{}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	definition := contract.WorkflowDefinition{
		ID: "new-task-notify", Version: 1, Entry: "check",
		Trigger: &contract.RecordTrigger{Kind: "record.created", Table: "Task"},
		Steps: []contract.StepDefinition{
			{ID: "check", Kind: contract.StepCondition, Condition: &contract.ConditionSpec{Field: &contract.FieldCondition{Field: "status", Operator: "equals", Value: json.RawMessage(`"Open"`)}, IfTrue: "finish", IfFalse: "finish"}},
			{ID: "finish", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "complete"}},
		},
	}
	result := service.ValidateCandidateWithRecordSchemas(definition, candidateRecordSchemas{"Task": {"status": "Select"}})
	if !result.Valid || len(result.Issues) != 0 {
		t.Fatalf("valid workflow for a new proposed table was rejected: %+v", result)
	}
	result = service.ValidateCandidateWithRecordSchemas(definition, candidateRecordSchemas{"Task": {"title": "Data"}})
	if result.Valid || len(result.Issues) == 0 || result.Issues[0].Code != "unknown_field" {
		t.Fatalf("unknown field in proposed table was not rejected: %+v", result)
	}
}

func TestDefinitionDigestCanonicalizesEmbeddedJSONObjects(t *testing.T) {
	first := validTestDefinition()
	first.Steps[0].Action = &contract.ActionSpec{Name: "send", Version: 1, InputSchema: "kora.send", Input: json.RawMessage(`{"recipient":"owner@example.test","subject":"Ready"}`), Next: "finish"}
	second := first
	second.Steps = append([]contract.StepDefinition(nil), first.Steps...)
	second.Steps[0].Action = &contract.ActionSpec{Name: "send", Version: 1, InputSchema: "kora.send", Input: json.RawMessage(`{"subject":"Ready","recipient":"owner@example.test"}`), Next: "finish"}
	firstDigest, err := DefinitionDigest(first)
	if err != nil {
		t.Fatal("digest first definition:", err)
	}
	secondDigest, err := DefinitionDigest(second)
	if err != nil {
		t.Fatal("digest reordered definition:", err)
	}
	if firstDigest != secondDigest {
		t.Fatalf("equivalent action JSON produced different definition digests: %s != %s", firstDigest, secondDigest)
	}
}

func TestRecordTriggeredPublishRequiresSuccessfulPreviewOfExactDefinition(t *testing.T) {
	exec, err := policy.NewVerifiedExecution("tenant-1", contract.ActorContext{PrincipalID: "user-1", PrincipalType: contract.PrincipalHuman, Site: "tenant-1"})
	if err != nil {
		t.Fatal(err)
	}
	definition := validTestDefinition()
	definition.Trigger = &contract.RecordTrigger{Kind: "record.created", Table: "Invoice"}
	digest, err := DefinitionDigest(definition)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		preview *PreviewRun
		wantErr bool
	}{{name: "missing", wantErr: true}, {name: "failed", preview: &PreviewRun{DefinitionID: definition.ID, DefinitionVersion: definition.Version, DefinitionDigest: digest, Status: "failed"}, wantErr: true}, {name: "stale definition", preview: &PreviewRun{DefinitionID: definition.ID, DefinitionVersion: definition.Version, DefinitionDigest: strings.Repeat("f", 64), Status: "succeeded"}, wantErr: true}, {name: "passed", preview: &PreviewRun{DefinitionID: definition.ID, DefinitionVersion: definition.Version, DefinitionDigest: digest, Status: "succeeded"}}} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &testDefinitionRepository{}
			if tc.preview != nil {
				repo.previews = append(repo.previews, *tc.preview)
			}
			service, serviceErr := NewService(repo, testDefinitionAuthorizer{allowed: true}, frozenTestCatalog(t), testSchemas{}, testWaits{}, time.Now)
			if serviceErr != nil {
				t.Fatal(serviceErr)
			}
			_, publishErr := service.Publish(context.Background(), exec, definition, "publish-preview-test")
			if (publishErr != nil) != tc.wantErr {
				t.Fatalf("publish error = %v, wantErr %v", publishErr, tc.wantErr)
			}
			if repo.called == tc.wantErr {
				t.Fatalf("repository called=%v with publish error %v", repo.called, publishErr)
			}
		})
	}
}

func TestServiceRejectsUnauthorizedOrInvalidDefinitionBeforePersistence(t *testing.T) {
	exec, err := policy.NewVerifiedExecution("tenant-1", contract.ActorContext{PrincipalID: "user-1", PrincipalType: contract.PrincipalHuman, Site: "tenant-1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		allowed bool
		mutate  func(*contract.WorkflowDefinition)
	}{{"denied", false, func(*contract.WorkflowDefinition) {}}, {"invalid", true, func(d *contract.WorkflowDefinition) { d.Steps[0].Action.Name = "unknown" }}} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &testDefinitionRepository{}
			service, _ := NewService(repo, testDefinitionAuthorizer{allowed: tc.allowed}, frozenTestCatalog(t), testSchemas{}, testWaits{}, time.Now)
			definition := validTestDefinition()
			tc.mutate(&definition)
			if _, err := service.Publish(context.Background(), exec, definition, "key-1"); err == nil {
				t.Fatal("invalid or unauthorized publish was accepted")
			}
			if repo.called {
				t.Fatal("repository was called before authorization and validation")
			}
		})
	}
}

func TestServicePreviewPersistsRedactedSuccessAndFailureHistory(t *testing.T) {
	repo := &testDefinitionRepository{}
	exec, err := policy.NewVerifiedExecution("stable-tenant-1", contract.ActorContext{PrincipalID: "user-1", PrincipalType: contract.PrincipalHuman, Site: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repo, testDefinitionAuthorizer{allowed: true}, frozenTestCatalog(t), testSchemas{}, testWaits{}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	definition := validTestDefinition()
	definition.Trigger = &contract.RecordTrigger{Kind: "record.created", Table: "Invoice"}
	success, err := service.Preview(context.Background(), exec, definition, json.RawMessage(`{"customer":"private@example.test"}`))
	if err != nil {
		t.Fatal(err)
	}
	if success.Status != "succeeded" || len(repo.previews) != 1 || strings.Contains(success.Failure, "private@example.test") {
		t.Fatalf("preview success was not safely recorded: %+v", success)
	}
	failed, err := service.Preview(context.Background(), exec, definition, json.RawMessage(`[]`))
	if err == nil {
		t.Fatal("invalid sample unexpectedly succeeded")
	}
	if failed.Status != "failed" || len(repo.previews) != 2 || strings.Contains(failed.Failure, "private@example.test") {
		t.Fatalf("preview failure was not safely recorded: %+v", failed)
	}
	history, err := service.PreviewHistory(context.Background(), exec, 20)
	if err != nil || len(history) != 2 {
		t.Fatalf("preview history = %d, err=%v", len(history), err)
	}
}

func TestServiceAssemblyRejectsMissingRegisteredActionSchema(t *testing.T) {
	if _, err := NewService(&testDefinitionRepository{}, testDefinitionAuthorizer{allowed: true}, frozenTestCatalog(t), inputOnlySchemas{}, testWaits{}, time.Now); err == nil {
		t.Fatal("service accepted missing output schema")
	}
}

func TestInstanceServiceStartsFromImmutableDefinitionAndChecksReadAuthorization(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	definition := validTestDefinition()
	ref := contract.DefinitionVersionRef{DefinitionID: definition.ID, Version: definition.Version, Digest: "stored-digest"}
	defs := &testDefinitionRepository{stored: definition, storedRef: ref}
	instances := &testInstanceRepository{}
	authz := &testInstanceAuthorizer{start: true, read: true}
	exec, err := policy.NewVerifiedExecution("stable-tenant-1", contract.ActorContext{PrincipalID: "user-1", PrincipalType: contract.PrincipalHuman, Site: "tenant-route"})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewInstanceService(defs, instances, authz, frozenTestCatalog(t), testSchemas{}, testWaits{}, func() time.Time { return now }, func() string { return contract.NewIDAt(now) })
	if err != nil {
		t.Fatal(err)
	}
	cmd := contract.StartWorkflow{Meta: contract.CommandMeta{IdempotencyKey: "start-workflow-1"}, DefinitionID: definition.ID, DefinitionVersion: 1, Subject: contract.SubjectRef{Kind: "Invoice", ID: "INV-1", Revision: "3"}}
	instance, err := service.Start(context.Background(), exec, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if !instances.called || instances.input.Tenant != "stable-tenant-1" || instances.input.Actor != "user-1" || instances.input.IdempotencyKey != "start-workflow-1" || instances.input.DefinitionDigest != ref.Digest || instance.CurrentStep != "approve" || authz.startCalls != 1 {
		t.Fatalf("start did not bind verified inputs: instance=%+v input=%+v auth=%+v", instance, instances.input, authz)
	}
	if _, err := service.Get(context.Background(), exec, instance.ID); err != nil || authz.readCalls != 1 {
		t.Fatalf("authorized Get = %v, reads=%d", err, authz.readCalls)
	}
	authz.read = false
	if _, err := service.Get(context.Background(), exec, instance.ID); err == nil {
		t.Fatal("unauthorized workflow read succeeded")
	}
}

func TestInstanceServiceRejectsStartBeforePersistenceWhenNotAuthorized(t *testing.T) {
	definition := validTestDefinition()
	defs := &testDefinitionRepository{stored: definition, storedRef: contract.DefinitionVersionRef{DefinitionID: definition.ID, Version: 1, Digest: "digest"}}
	instances, authz := &testInstanceRepository{}, &testInstanceAuthorizer{}
	service, err := NewInstanceService(defs, instances, authz, frozenTestCatalog(t), testSchemas{}, testWaits{}, time.Now, func() string { return contract.NewID() })
	if err != nil {
		t.Fatal(err)
	}
	exec, _ := policy.NewVerifiedExecution("tenant-1", contract.ActorContext{PrincipalID: "user-1", PrincipalType: contract.PrincipalHuman, Site: "tenant-1"})
	_, err = service.Start(context.Background(), exec, contract.StartWorkflow{Meta: contract.CommandMeta{IdempotencyKey: "start-1"}, DefinitionID: definition.ID, DefinitionVersion: 1, Subject: contract.SubjectRef{Kind: "Invoice", ID: "INV-1", Revision: "1"}})
	if err == nil {
		t.Fatal("unauthorized start succeeded")
	}
	if instances.called {
		t.Fatal("unauthorized start reached repository")
	}
}

func TestRecordTriggerStartsMatchingDefinitionIdempotentlyAndFiltersWatchedFields(t *testing.T) {
	definition := validTestDefinition()
	definition.Trigger = &contract.RecordTrigger{Kind: "record.updated", Table: "Invoice", WatchedFields: []string{"status", "owner"}}
	ref := contract.DefinitionVersionRef{DefinitionID: definition.ID, Version: 1, Digest: "digest"}
	defs := &testDefinitionRepository{stored: definition, storedRef: ref}
	instances := &testInstanceRepository{}
	service, err := NewInstanceService(defs, instances, &testInstanceAuthorizer{}, frozenTestCatalog(t), testSchemas{}, testWaits{}, time.Now, func() string { return contract.NewID() })
	if err != nil {
		t.Fatal(err)
	}
	event := contract.EventEnvelope{ID: "evt-1", Type: "record.updated", Source: "kora.kernel", Site: "tenant-route", AggregateType: "Invoice", AggregateID: "INV-1", Actor: contract.ActorContext{PrincipalID: "user-1", PrincipalType: contract.PrincipalHuman, Site: "tenant-route"}, Data: json.RawMessage(`{"table":"Invoice","record_id":"INV-1","revision":"4","changed_fields":["status"]}`)}
	started, err := service.StartFromRecordEvent(context.Background(), "tenant-1", "tenant-route", event)
	if err != nil || len(started) != 1 || !instances.called {
		t.Fatalf("matching record event did not start workflow: started=%+v called=%v err=%v", started, instances.called, err)
	}
	if instances.input.Subject != (contract.SubjectRef{Kind: "Invoice", ID: "INV-1", Revision: "4"}) || !strings.HasPrefix(string(instances.input.IdempotencyKey), "trigger_") {
		t.Fatalf("trigger start was not bound to event subject/idempotency: %+v", instances.input)
	}
	instances.called = false
	event.ID = "evt-2"
	event.Data = json.RawMessage(`{"table":"Invoice","record_id":"INV-1","revision":"5","changed_fields":["description"]}`)
	started, err = service.StartFromRecordEvent(context.Background(), "tenant-1", "tenant-route", event)
	if err != nil || len(started) != 0 || instances.called {
		t.Fatalf("unwatched field triggered workflow: started=%+v called=%v err=%v", started, instances.called, err)
	}
}

func TestInstanceServiceReconcileRequiresPrivilegeAndUsesPinnedActionEdges(t *testing.T) {
	definition := validTestDefinition()
	defs := &testDefinitionRepository{stored: definition, storedRef: contract.DefinitionVersionRef{DefinitionID: definition.ID, Version: definition.Version, Digest: "digest"}}
	instances := &testInstanceRepository{instance: contract.WorkflowInstance{ID: "01K7INSTANCE00000000000000000", Revision: "7", DefinitionID: definition.ID, DefinitionVersion: definition.Version, Subject: contract.SubjectRef{Kind: "Invoice", ID: "INV-1", Revision: "2"}, Status: contract.InstanceUncertain, CurrentStep: "approve"}}
	authz := &testInstanceAuthorizer{reconcile: true}
	service, err := NewInstanceService(defs, instances, authz, frozenTestCatalog(t), testSchemas{}, testWaits{}, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	exec, err := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: "operator-1", PrincipalType: contract.PrincipalHuman, Site: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	cmd := contract.ReconcileWorkflow{Meta: contract.CommandMeta{IdempotencyKey: "reconcile-1", ExpectedRevision: "7"}, StepID: "approve", Outcome: contract.ReconcileSucceeded, EvidenceRef: "case:OPS-42", Reason: "Provider confirmed the approval was applied"}
	result, err := service.Reconcile(context.Background(), exec, instances.instance.ID, cmd)
	if err != nil {
		t.Fatal("Reconcile:", err)
	}
	commit := instances.reconciled
	if !instances.called || authz.reconcileCalls != 1 || commit.Tenant != "tenant-a" || commit.Actor != "operator-1" || commit.NextStep != "done" || commit.Status != contract.InstanceReady || result.CurrentStep != "done" {
		t.Fatalf("reconciliation did not preserve authorized pinned transition: commit=%+v result=%+v authz=%+v", commit, result, authz)
	}
	authz.reconcile = false
	instances.called = false
	cmd.Meta.IdempotencyKey = "reconcile-2"
	if _, err := service.Reconcile(context.Background(), exec, instances.instance.ID, cmd); err == nil || instances.called {
		t.Fatal("unauthorized reconciliation reached persistence")
	}
}

func TestInstanceServiceReconcileNotAppliedReusesUncertainStepAndAbandonFails(t *testing.T) {
	for _, test := range []struct {
		outcome contract.ReconciliationOutcome
		status  contract.InstanceStatus
		next    contract.StepID
	}{
		{contract.ReconcileNotApplied, contract.InstanceReady, "approve"},
		{contract.ReconcileAbandon, contract.InstanceFailed, "approve"},
	} {
		definition := validTestDefinition()
		defs := &testDefinitionRepository{stored: definition, storedRef: contract.DefinitionVersionRef{DefinitionID: definition.ID, Version: 1, Digest: "digest"}}
		instances := &testInstanceRepository{instance: contract.WorkflowInstance{ID: "01K7INSTANCE00000000000000000", Revision: "7", DefinitionID: definition.ID, DefinitionVersion: 1, Status: contract.InstanceUncertain, CurrentStep: "approve"}}
		authz := &testInstanceAuthorizer{reconcile: true}
		service, _ := NewInstanceService(defs, instances, authz, frozenTestCatalog(t), testSchemas{}, testWaits{}, time.Now, nil)
		exec, _ := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: "operator-1", PrincipalType: contract.PrincipalHuman, Site: "tenant-a"})
		cmd := contract.ReconcileWorkflow{Meta: contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("reconcile-" + string(test.outcome)), ExpectedRevision: "7"}, StepID: "approve", Outcome: test.outcome, EvidenceRef: "case:OPS-42", Reason: "Operator reviewed provider evidence"}
		if _, err := service.Reconcile(context.Background(), exec, instances.instance.ID, cmd); err != nil {
			t.Fatal("Reconcile:", err)
		}
		if instances.reconciled.Status != test.status || instances.reconciled.NextStep != test.next {
			t.Fatalf("outcome %s transition = %s/%s, want %s/%s", test.outcome, instances.reconciled.Status, instances.reconciled.NextStep, test.status, test.next)
		}
	}
}
