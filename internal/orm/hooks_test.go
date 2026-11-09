package orm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/script"
)

type hookRequestSink struct{ request AsyncHookRequest }

func (s *hookRequestSink) Enqueue(_ context.Context, req AsyncHookRequest) error {
	s.request = req
	return nil
}

type hookTestRunner struct{}

func (hookTestRunner) Execute(context.Context, script.ExecuteRequest) (*script.ExecuteResult, error) {
	return &script.ExecuteResult{}, nil
}
func (hookTestRunner) Validate(string) error { return nil }
func (hookTestRunner) Close() error          { return nil }

type hookFailureRunner struct {
	panicValue any
	err        error
	seenCtxErr error
}

type moduleExtensionRunner struct {
	requests    []script.ExecuteRequest
	modified    bool
	mutateInput bool
}

func (r *moduleExtensionRunner) Execute(_ context.Context, req script.ExecuteRequest) (*script.ExecuteResult, error) {
	r.requests = append(r.requests, req)
	if r.mutateInput {
		req.Document["status"] = "corrupted"
	}
	return &script.ExecuteResult{Result: map[string]any{"valid": true}, Modified: r.modified}, nil
}
func (*moduleExtensionRunner) Validate(string) error { return nil }
func (*moduleExtensionRunner) Close() error          { return nil }

type moduleExtensionSchemas struct {
	failRef string
	err     error
}

func (s moduleExtensionSchemas) Validate(ref contract.SchemaRef, _ json.RawMessage) error {
	if ref.ID == s.failRef {
		return s.err
	}
	return nil
}

type capturedAsyncHookStore struct {
	invocations []script.AsyncHookInvocation
	err         error
}

func (s *capturedAsyncHookStore) AppendAsyncHookTx(_ context.Context, _ *sql.Tx, invocation script.AsyncHookInvocation) error {
	if s.err != nil {
		return s.err
	}
	s.invocations = append(s.invocations, invocation)
	return nil
}
func (*capturedAsyncHookStore) ClaimAsyncHooks(context.Context, contract.TenantID, string, uint16, time.Duration) ([]script.AsyncHookClaim, error) {
	return nil, nil
}
func (*capturedAsyncHookStore) BeginAsyncHook(context.Context, script.AsyncHookClaim) (*sql.Tx, error) {
	return nil, nil
}
func (*capturedAsyncHookStore) MarkAsyncHookExternalEffect(context.Context, script.AsyncHookClaim) error {
	return nil
}
func (*capturedAsyncHookStore) SucceedAsyncHookTx(context.Context, *sql.Tx, script.AsyncHookClaim) error {
	return nil
}
func (*capturedAsyncHookStore) FinishAsyncHook(context.Context, script.AsyncHookClaim, script.AsyncHookOutcome) error {
	return nil
}

func (r *hookFailureRunner) Execute(ctx context.Context, _ script.ExecuteRequest) (*script.ExecuteResult, error) {
	r.seenCtxErr = ctx.Err()
	if r.panicValue != nil {
		panic(r.panicValue)
	}
	return nil, r.err
}
func (*hookFailureRunner) Validate(string) error { return nil }
func (*hookFailureRunner) Close() error          { return nil }

func TestBeforeHookPanicRejectsMutationAndLogsFailure(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sql mock: %v", err)
	}
	defer database.Close()
	mock.ExpectExec("INSERT INTO _kora_script_execution").
		WithArgs(sqlmock.AnyArg(), "test-site", "validate-task", sqlmock.AnyArg(), "Task", "TASK-1", string(script.EventValidate), "alice@example.test", int64(0), "error", "script panic: broken hook").
		WillReturnResult(sqlmock.NewResult(1, 1))
	runner := &hookFailureRunner{panicValue: "broken hook"}
	tx := &TxManager{
		ScriptRunner: runner,
		ScriptStore:  &script.Store{DB: database, Dialect: kdb.Resolve("mysql")},
		SiteName:     "test-site",
		CurrentUser:  "alice@example.test",
		HookScriptsByDoctype: map[string][]script.ScriptRecord{
			"Task": {{Name: "validate-task", ScriptType: script.TypeDocEvent, Event: script.EventValidate}},
		},
	}
	err = tx.runHooks(&doctype.DocType{Name: "Task"}, script.EventValidate, &doctype.Document{DocType: "Task", Name: "TASK-1"}, nil)
	if err == nil || err.Error() != `script "validate-task" (validate): script panic: broken hook` {
		t.Fatalf("panic hook error = %v, want wrapped panic error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expected panic execution log: %v", err)
	}
}

func TestBeforeHookReceivesCanceledCommandContext(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sql mock: %v", err)
	}
	defer database.Close()
	mock.ExpectExec("INSERT INTO _kora_script_execution").
		WithArgs(sqlmock.AnyArg(), "test-site", "validate-task", sqlmock.AnyArg(), "Task", "TASK-1", string(script.EventValidate), "alice@example.test", int64(0), "error", context.Canceled.Error()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := &hookFailureRunner{err: context.Canceled}
	tx := &TxManager{
		Context:      ctx,
		ScriptRunner: runner,
		ScriptStore:  &script.Store{DB: database, Dialect: kdb.Resolve("mysql")},
		SiteName:     "test-site",
		CurrentUser:  "alice@example.test",
		HookScriptsByDoctype: map[string][]script.ScriptRecord{
			"Task": {{Name: "validate-task", ScriptType: script.TypeDocEvent, Event: script.EventValidate}},
		},
	}
	err = tx.runHooks(&doctype.DocType{Name: "Task"}, script.EventValidate, &doctype.Document{DocType: "Task", Name: "TASK-1"}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled hook error = %v, want context.Canceled", err)
	}
	if !errors.Is(runner.seenCtxErr, context.Canceled) {
		t.Fatalf("runner context error = %v, want context.Canceled", runner.seenCtxErr)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expected canceled execution log: %v", err)
	}
}

func TestOwnerModuleValidationExtensionsAreOrderedBoundedAndEffectFree(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	target := contract.ResourceRef{Namespace: "tenant", Name: "Invoice", Version: 1}
	handlerA := contract.ResourceRef{Namespace: "tenant", Name: "tax.vat.validate", Version: 1}
	handlerB := contract.ResourceRef{Namespace: "tenant", Name: "tax.discount.validate", Version: 1}
	point := contract.ExtensionPoint{OwnerModuleID: "finance.ar", ID: "invoice.validation", Version: 1, Target: target,
		AllowedKinds: []string{string(contract.ExtensionValidation)}, InputSchema: contract.SchemaRef{ID: "invoice", Version: 1},
		OutputSchema: contract.SchemaRef{ID: "validation-result", Version: 1}, ExecutionPhase: "before_commit", FailurePolicy: "reject"}
	owner := contract.ModuleDescriptor{ID: "finance.ar", Version: "1.0.0", Resources: []contract.OwnedResource{{Ref: target}}, ExtensionPoints: []contract.ExtensionPoint{point}}
	extenderA := contract.ModuleDescriptor{ID: "tax.vat", Version: "1.0.0", Resources: []contract.OwnedResource{{Ref: handlerA}}, Extensions: []contract.ExtensionRegistration{{ID: "vat-check", ExtenderModuleID: "tax.vat", PointID: "finance.ar/invoice.validation", PointVersion: 1, Kind: contract.ExtensionValidation, ArtifactDigest: strings.Repeat("a", 64), HandlerRef: handlerA, Order: 10}}}
	extenderB := contract.ModuleDescriptor{ID: "tax.discount", Version: "1.0.0", Resources: []contract.OwnedResource{{Ref: handlerB}}, Extensions: []contract.ExtensionRegistration{{ID: "discount-check", ExtenderModuleID: "tax.discount", PointID: "finance.ar/invoice.validation", PointVersion: 1, Kind: contract.ExtensionValidation, ArtifactDigest: strings.Repeat("b", 64), HandlerRef: handlerB, Order: 20, DependsOn: []string{"tax.vat/vat-check"}}}}
	modules := []contract.ModuleDescriptor{extenderB, owner, extenderA}
	ordered, err := contract.OrderModuleExtensions(modules, nil)
	if err != nil {
		t.Fatal("order module validation extensions:", err)
	}
	runner := &moduleExtensionRunner{}
	logSuccess := func(name string) {
		mock.ExpectExec("INSERT INTO _kora_script_execution").WithArgs(sqlmock.AnyArg(), "", name, string(script.TypeDocEvent), "Invoice", "INV-1", string(script.EventValidate), "", sqlmock.AnyArg(), "success", "").WillReturnResult(sqlmock.NewResult(1, 1))
	}
	logFailure := func(name string) {
		mock.ExpectExec("INSERT INTO _kora_script_execution").WithArgs(sqlmock.AnyArg(), "", name, string(script.TypeDocEvent), "Invoice", "INV-1", string(script.EventValidate), "", sqlmock.AnyArg(), "error", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	}
	logSuccess(handlerA.Name)
	logSuccess(handlerB.Name)
	logSuccess(handlerA.Name)
	logSuccess(handlerB.Name)
	logFailure(handlerA.Name)
	logFailure(handlerA.Name)
	logSuccess(handlerA.Name)
	tx := &TxManager{
		ScriptRunner: runner, ScriptStore: &script.Store{DB: database, Dialect: kdb.Resolve("mysql")}, ModuleDescriptors: modules,
		ModuleExtensions: ordered, ModuleExtensionSchemas: moduleExtensionSchemas{},
		HookScriptsByDoctype: map[string][]script.ScriptRecord{"Invoice": {
			{Name: handlerB.Name, ScriptType: script.TypeDocEvent, DocType: "Invoice", Event: script.EventValidate, IsActive: true, TimeoutMs: 500, Script: "return {result: {valid: true}}"},
			{Name: handlerA.Name, ScriptType: script.TypeDocEvent, DocType: "Invoice", Event: script.EventValidate, IsActive: true, TimeoutMs: 500, Script: "return {result: {valid: true}}"},
		}},
	}
	if err := tx.runHooks(&doctype.DocType{Name: "Invoice"}, script.EventValidate, &doctype.Document{DocType: "Invoice", Name: "INV-1", Fields: map[string]any{"status": "draft"}}, nil); err != nil {
		t.Fatal("run owner validation extensions:", err)
	}
	if len(runner.requests) != 2 || runner.requests[0].ScriptName != handlerA.Name || runner.requests[1].ScriptName != handlerB.Name {
		t.Fatalf("validation execution order = %+v", runner.requests)
	}
	for _, req := range runner.requests {
		if req.Provider != nil || !req.Deterministic || req.Timeout != 500*time.Millisecond {
			t.Fatalf("extension validation was not isolated and bounded: provider=%T deterministic=%t timeout=%s", req.Provider, req.Deterministic, req.Timeout)
		}
	}

	doc := &doctype.Document{DocType: "Invoice", Name: "INV-1", Fields: map[string]any{"status": "draft"}}
	runner.mutateInput = true
	if err := tx.runHooks(&doctype.DocType{Name: "Invoice"}, script.EventValidate, doc, nil); err != nil {
		t.Fatal("run isolated validation input:", err)
	}
	if doc.Fields["status"] != "draft" {
		t.Fatalf("validation extension mutated owner document: %#v", doc.Fields)
	}
	runner.mutateInput = false

	runner.modified = true
	if err := tx.runHooks(&doctype.DocType{Name: "Invoice"}, script.EventValidate, &doctype.Document{DocType: "Invoice", Name: "INV-1"}, nil); err == nil {
		t.Fatal("validation extension modified the owner document")
	}
	runner.modified = false
	tx.ModuleExtensionSchemas = moduleExtensionSchemas{failRef: point.OutputSchema.ID, err: errors.New("schema rejected")}
	if err := tx.runHooks(&doctype.DocType{Name: "Invoice"}, script.EventValidate, &doctype.Document{DocType: "Invoice", Name: "INV-1"}, nil); err == nil {
		t.Fatal("invalid extension result bypassed the registered output schema")
	}
	tx.ModuleExtensions = ordered[:1]
	tx.ModuleExtensionSchemas = moduleExtensionSchemas{}
	beforeDisabledAttempt := len(runner.requests)
	if err := tx.runHooks(&doctype.DocType{Name: "Invoice"}, script.EventValidate, &doctype.Document{DocType: "Invoice", Name: "INV-1"}, nil); err != nil {
		t.Fatal("disabled extension handler was not skipped:", err)
	}
	if len(runner.requests) != beforeDisabledAttempt+1 || runner.requests[len(runner.requests)-1].ScriptName != handlerA.Name {
		t.Fatalf("disabled extension ran after deactivation: %+v", runner.requests[beforeDisabledAttempt:])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("extension execution logs: %v", err)
	}
}

func TestAsyncAfterHookCarriesRecursionGuard(t *testing.T) {
	const hookName = "task-after-save"
	sink := &hookRequestSink{}
	dt := &doctype.DocType{Name: "Task"}
	doc := doctype.NewDocument("Task")
	doc.Name = "TASK-1"
	tx := &TxManager{
		Registry:      doctype.NewRegistry(),
		ScriptRunner:  hookTestRunner{},
		ScriptStore:   &script.Store{},
		AsyncHookSink: sink,
		SiteName:      "test-site",
		HookScriptsByDoctype: map[string][]script.ScriptRecord{
			"Task": {{Name: hookName, Event: script.EventAfterSave}},
		},
		SkipHookScripts: []string{"another-hook"},
	}
	if err := tx.runHooks(dt, script.EventAfterSave, doc, nil); err != nil {
		t.Fatalf("run after-save hook: %v", err)
	}
	if sink.request.Rec.Name != hookName {
		t.Fatalf("enqueued hook = %q, want %q", sink.request.Rec.Name, hookName)
	}
	want := []string{"another-hook", hookName}
	if len(sink.request.SkipHookScripts) != len(want) {
		t.Fatalf("skip list = %#v, want %#v", sink.request.SkipHookScripts, want)
	}
	for i := range want {
		if sink.request.SkipHookScripts[i] != want[i] {
			t.Fatalf("skip list = %#v, want %#v", sink.request.SkipHookScripts, want)
		}
	}
	if len(tx.SkipHookScripts) != 1 || tx.SkipHookScripts[0] != "another-hook" {
		t.Fatalf("enqueue mutated caller's skip list: %#v", tx.SkipHookScripts)
	}
}

func TestAsyncAfterHookCarriesVerifiedExecution(t *testing.T) {
	sink := &hookRequestSink{}
	execution, err := policy.NewVerifiedExecution("test-site", contract.ActorContext{
		PrincipalID: "alice@example.test", PrincipalType: contract.PrincipalHuman,
		SubjectUserID: "alice@example.test", Site: "test-site", Roles: []string{"Reader", "Auditor"},
	})
	if err != nil {
		t.Fatal("create execution:", err)
	}
	tx := &TxManager{
		Registry: doctype.NewRegistry(), ScriptRunner: hookTestRunner{}, ScriptStore: &script.Store{},
		AsyncHookSink: sink, SiteName: "test-site", CurrentUser: "alice@example.test", Execution: execution,
		HookScriptsByDoctype: map[string][]script.ScriptRecord{
			"Task": {{Name: "task-after-save", Event: script.EventAfterSave}},
		},
	}
	if err := tx.runHooks(&doctype.DocType{Name: "Task"}, script.EventAfterSave, doctype.NewDocument("Task"), nil); err != nil {
		t.Fatal("queue async hook:", err)
	}
	if !sink.request.Execution.Valid() || sink.request.Execution.Actor().PrincipalID != "alice@example.test" {
		t.Fatalf("async hook execution = %#v, want verified originating actor", sink.request.Execution)
	}
	if got, want := sink.request.UserRoles, []string{"Reader", "Auditor"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("async hook roles = %#v, want full verified role set %#v", got, want)
	}
}

func TestNormalizeHookDocumentFields_RestoresChildTables(t *testing.T) {
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Sale Item", IsChildTable: true, Fields: []doctype.Field{
		{Fieldname: "product", Fieldtype: "Link", Options: "Product"},
		{Fieldname: "quantity", Fieldtype: "Float"},
	}})
	sale := &doctype.DocType{Name: "Sale", Fields: []doctype.Field{
		{Fieldname: "receipt_number", Fieldtype: "Data"},
		{Fieldname: "items", Fieldtype: "Table", Options: "Sale Item"},
	}}

	fields := normalizeHookDocumentFields(sale, reg, map[string]any{
		"name":           "SALE-0001",
		"doc_status":     float64(0),
		"receipt_number": "RCPT-1",
		"items": []any{
			map[string]any{"name": "SI-0001", "product": "PROD-0001", "quantity": float64(2)},
		},
	})
	doc := &doctype.Document{DocType: "Sale", Fields: fields}

	if _, ok := fields["name"]; ok {
		t.Fatalf("system field name should not be copied into document fields")
	}
	items := doc.GetTable("items")
	if len(items) != 1 {
		t.Fatalf("items count: got %d, want 1 (%#v)", len(items), fields["items"])
	}
	if got := items[0].Name; got != "SI-0001" {
		t.Fatalf("items[0].name: got %q, want SI-0001", got)
	}
	if got := items[0].Get("product"); got != "PROD-0001" {
		t.Fatalf("items[0].product: got %#v, want PROD-0001", got)
	}
}

func TestDocumentFromMap_RoundTrip(t *testing.T) {
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "Sale Item", IsChildTable: true, Fields: []doctype.Field{
		{Fieldname: "product", Fieldtype: "Link", Options: "Product"},
		{Fieldname: "quantity", Fieldtype: "Float"},
	}})
	reg.Register(&doctype.DocType{Name: "Sale", Fields: []doctype.Field{
		{Fieldname: "receipt_number", Fieldtype: "Data"},
		{Fieldname: "items", Fieldtype: "Table", Options: "Sale Item"},
	}})

	src := doctype.NewDocument("Sale")
	src.Name = "SALE-0001"
	src.DocStatus = 0
	src.Set("receipt_number", "RCPT-1")
	child := doctype.NewDocument("Sale Item")
	child.Name = "SI-0001"
	child.Set("product", "PROD-0001")
	child.Set("quantity", float64(2))
	src.Set("items", []*doctype.Document{child})

	m := src.ToMap()
	doc := DocumentFromMap(reg, "Sale", m)
	if doc == nil {
		t.Fatal("DocumentFromMap returned nil")
	}
	if doc.Name != "SALE-0001" {
		t.Errorf("name = %q, want SALE-0001", doc.Name)
	}
	items := doc.GetTable("items")
	if len(items) != 1 {
		t.Fatalf("items count = %d, want 1", len(items))
	}
	if got := items[0].Get("product"); got != "PROD-0001" {
		t.Errorf("items[0].product = %#v, want PROD-0001", got)
	}
}

func TestDocumentFromMap_NilMap(t *testing.T) {
	if got := DocumentFromMap(doctype.NewRegistry(), "Sale", nil); got != nil {
		t.Errorf("DocumentFromMap(nil) = %v, want nil", got)
	}
}

func TestHookEnqueueFailedCount_Initialized(t *testing.T) {
	if got := HookEnqueueFailedCount(); got < 0 {
		t.Errorf("HookEnqueueFailedCount() = %d, want >= 0", got)
	}
}

func TestActiveHookScriptsLoadsOneDocTypeSnapshot(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sql mock: %v", err)
	}
	defer database.Close()
	columns := []string{
		"name", "site", "script_type", "doctype", "event", "method_path", "schedule",
		"priority", "is_active", "run_as", "timeout_ms", "script", "compiled_at", "compile_error",
		"created_by", "updated_by", "creation", "modified",
	}
	mock.ExpectQuery("SELECT name, site, script_type, doctype, event, method_path").
		WithArgs("test-site", true, string(script.TypeDocEvent), "Task").
		WillReturnRows(sqlmock.NewRows(columns))
	tx := &TxManager{SiteName: "test-site", ScriptStore: &script.Store{DB: database, Dialect: kdb.Resolve("mysql")}}
	for _, event := range []script.Event{script.EventValidate, script.EventBeforeInsert, script.EventBeforeSave, script.EventAfterInsert, script.EventAfterSave} {
		if _, err := tx.activeHookScripts("Task"); err != nil {
			t.Fatalf("load scripts for %s: %v", event, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expected one script-table query per DocType and command: %v", err)
	}
}

func TestCaptureAsyncAfterHooksTxFreezesScriptAndVerifiedExecution(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	mock.ExpectBegin()
	dbTx, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal("begin source transaction:", err)
	}
	columns := []string{"name", "site", "script_type", "doctype", "event", "method_path", "schedule", "priority", "is_active", "run_as", "timeout_ms", "script", "compiled_at", "compile_error", "created_by", "updated_by", "creation", "modified"}
	mock.ExpectQuery("SELECT name, site, script_type, doctype, event, method_path").
		WithArgs("tenant-a", true, string(script.TypeDocEvent), "Task").
		WillReturnRows(sqlmock.NewRows(columns).AddRow("task-after-save", "tenant-a", script.TypeDocEvent, "Task", script.EventAfterSave, "", "", 10, true, "", 5000, "return true", nil, nil, "", "", time.Now(), time.Now()))
	tenant := contract.TenantID("tenant-a")
	actor := contract.ActorContext{PrincipalID: "alice", PrincipalType: contract.PrincipalHuman, SubjectUserID: "alice", Site: "tenant-a"}
	execution, err := policy.NewVerifiedExecution(tenant, actor)
	if err != nil {
		t.Fatal("create verified execution:", err)
	}
	store := &capturedAsyncHookStore{}
	target := contract.ResourceRef{Namespace: "tenant", Name: "Task", Version: 1}
	handler := contract.ResourceRef{Namespace: "tenant", Name: "task-after-save", Version: 1}
	ownerModule := contract.ModuleDescriptor{ID: "workflow.task", Version: "1.0.0", Resources: []contract.OwnedResource{{Ref: target}}, ExtensionPoints: []contract.ExtensionPoint{{
		OwnerModuleID: "workflow.task", ID: "task.completed", Version: 1, Target: target, AllowedKinds: []string{string(contract.ExtensionPostCommit)},
		InputSchema: contract.SchemaRef{ID: "task-event", Version: 1}, OutputSchema: contract.SchemaRef{ID: "subscriber-result", Version: 1}, ExecutionPhase: "after_commit", FailurePolicy: "retry",
	}}}
	extenderModule := contract.ModuleDescriptor{ID: "audit.trail", Version: "1.0.0", Resources: []contract.OwnedResource{{Ref: handler}}, Extensions: []contract.ExtensionRegistration{{
		ID: "task-audit", ExtenderModuleID: "audit.trail", PointID: "workflow.task/task.completed", PointVersion: 1, Kind: contract.ExtensionPostCommit,
		ArtifactDigest: strings.Repeat("c", 64), HandlerRef: handler, Event: string(script.EventAfterSave), Order: 10,
	}}}
	moduleDescriptors := []contract.ModuleDescriptor{ownerModule, extenderModule}
	moduleExtensions, err := contract.OrderModuleExtensions(moduleDescriptors, nil)
	if err != nil {
		t.Fatal("resolve post-commit module extension:", err)
	}
	manager := &TxManager{
		DB: database, Transaction: dbTx, ScriptStore: &script.Store{DB: database, Dialect: kdb.Resolve("mysql")},
		AsyncHookInvocations: store, AsyncHookTenantID: tenant,
		AsyncHookSourceOperation: contract.OperationID(contract.NewOperationID()), AsyncHookConfigRevision: 42,
		Execution: execution, SiteName: "tenant-a", SkipHookScripts: []string{"other"},
		ModuleDescriptors: moduleDescriptors, ModuleExtensions: moduleExtensions,
	}
	dt := &doctype.DocType{Name: "Task"}
	doc := &doctype.Document{DocType: "Task", Name: "TASK-001", Fields: map[string]any{"title": "Ship"}}
	oldDoc := &doctype.Document{DocType: "Task", Name: "TASK-001", Fields: map[string]any{"title": "Draft"}}
	if err := manager.CaptureAsyncAfterHooksTx(context.Background(), dbTx, dt, script.EventAfterSave, doc, oldDoc); err != nil {
		t.Fatal("capture lifecycle invocation:", err)
	}
	if len(store.invocations) != 1 {
		t.Fatalf("captured invocations = %d, want 1", len(store.invocations))
	}
	got := store.invocations[0]
	var current, previous map[string]any
	if json.Unmarshal(got.Document, &current) != nil || json.Unmarshal(got.OldDocument, &previous) != nil {
		t.Fatal("captured document snapshots are invalid JSON")
	}
	if got.TenantID != tenant || got.SourceOperation != manager.AsyncHookSourceOperation || got.ConfigRevision != 42 || got.Sequence != 1 || got.Actor.PrincipalID != actor.PrincipalID || got.Record.Name != "task-after-save" || current["title"] != "Ship" || previous["title"] != "Draft" {
		t.Fatalf("captured invocation does not freeze source and execution: %+v current=%v previous=%v", got, current, previous)
	}
	if !manager.SuppressAsyncHookEvents {
		t.Fatal("durable capture must suppress post-commit compatibility enqueue")
	}
	mock.ExpectRollback()
	if err := dbTx.Rollback(); err != nil {
		t.Fatal("rollback source transaction:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}

	// A persisted handler resource is inert after its extension registration is
	// disabled, including when a later source transaction captures after-hooks.
	mock.ExpectBegin()
	disabledTx, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal("begin disabled extension source transaction:", err)
	}
	mock.ExpectQuery("SELECT name, site, script_type, doctype, event, method_path").
		WithArgs("tenant-a", true, string(script.TypeDocEvent), "Task").
		WillReturnRows(sqlmock.NewRows(columns).AddRow("task-after-save", "tenant-a", script.TypeDocEvent, "Task", script.EventAfterSave, "", "", 10, true, "", 5000, "return true", nil, nil, "", "", time.Now(), time.Now()))
	manager.Transaction = disabledTx
	manager.HookScriptsByDoctype = nil
	manager.ModuleExtensions = nil
	manager.AsyncHookSourceOperation = contract.OperationID(contract.NewOperationID())
	if err := manager.CaptureAsyncAfterHooksTx(context.Background(), disabledTx, dt, script.EventAfterSave, doc, oldDoc); err != nil {
		t.Fatal("capture with disabled module extension:", err)
	}
	if len(store.invocations) != 1 {
		t.Fatalf("disabled module extension produced new durable work: %d invocations", len(store.invocations))
	}
	mock.ExpectRollback()
	if err := disabledTx.Rollback(); err != nil {
		t.Fatal("rollback disabled extension transaction:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("disabled extension SQL expectations:", err)
	}
}

func TestLoadLifecycleHooksTxUsesCommandTransactionSnapshot(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	mock.ExpectBegin()
	dbTx, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal("begin command transaction:", err)
	}
	columns := []string{"name", "site", "script_type", "doctype", "event", "method_path", "schedule", "priority", "is_active", "run_as", "timeout_ms", "script", "compiled_at", "compile_error", "created_by", "updated_by", "creation", "modified"}
	mock.ExpectQuery("SELECT name, site, script_type, doctype, event, method_path").
		WithArgs("tenant-a", true, string(script.TypeDocEvent), "Invoice").
		WillReturnRows(sqlmock.NewRows(columns).AddRow("tax-validate", "tenant-a", script.TypeDocEvent, "Invoice", script.EventValidate, "", "", 10, true, "", 250, "return {result: {valid: true}}", nil, nil, "", "", time.Now(), time.Now()))
	manager := &TxManager{DB: database, Transaction: dbTx, Context: context.Background(), SiteName: "tenant-a", ScriptStore: &script.Store{DB: database, Dialect: kdb.Resolve("mysql")}}
	if err := manager.LoadLifecycleHooksTx(context.Background(), dbTx, &doctype.DocType{Name: "Invoice"}); err != nil {
		t.Fatal("load command lifecycle handlers:", err)
	}
	if got := manager.HookScriptsByDoctype["Invoice"]; len(got) != 1 || got[0].Name != "tax-validate" {
		t.Fatalf("transaction-bound lifecycle snapshot = %+v", got)
	}
	mock.ExpectRollback()
	if err := dbTx.Rollback(); err != nil {
		t.Fatal("rollback command transaction:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestCaptureAsyncAfterHooksTxPropagatesAppendFailure(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	mock.ExpectBegin()
	dbTx, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal("begin source transaction:", err)
	}
	columns := []string{"name", "site", "script_type", "doctype", "event", "method_path", "schedule", "priority", "is_active", "run_as", "timeout_ms", "script", "compiled_at", "compile_error", "created_by", "updated_by", "creation", "modified"}
	mock.ExpectQuery("SELECT name, site, script_type, doctype, event, method_path").
		WithArgs("tenant-a", true, string(script.TypeDocEvent), "Task").
		WillReturnRows(sqlmock.NewRows(columns).AddRow("task-after-save", "tenant-a", script.TypeDocEvent, "Task", script.EventAfterSave, "", "", 10, true, "", 5000, "return true", nil, nil, "", "", time.Now(), time.Now()))
	actor := contract.ActorContext{PrincipalID: "alice", PrincipalType: contract.PrincipalHuman, SubjectUserID: "alice", Site: "tenant-a"}
	execution, err := policy.NewVerifiedExecution(contract.TenantID("tenant-a"), actor)
	if err != nil {
		t.Fatal("create verified execution:", err)
	}
	manager := &TxManager{
		Transaction: dbTx, ScriptStore: &script.Store{DB: database, Dialect: kdb.Resolve("mysql")},
		AsyncHookInvocations: &capturedAsyncHookStore{err: errors.New("append failed")}, AsyncHookTenantID: "tenant-a",
		AsyncHookSourceOperation: contract.OperationID(contract.NewOperationID()), AsyncHookConfigRevision: 42,
		Execution: execution, SiteName: "tenant-a",
	}
	err = manager.CaptureAsyncAfterHooksTx(context.Background(), dbTx, &doctype.DocType{Name: "Task"}, script.EventAfterSave,
		&doctype.Document{DocType: "Task", Name: "TASK-001", Fields: map[string]any{"title": "Ship"}}, nil)
	if err == nil {
		t.Fatal("append failure must abort the source operation")
	}
	mock.ExpectRollback()
	if err := dbTx.Rollback(); err != nil {
		t.Fatal("rollback source transaction:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}
