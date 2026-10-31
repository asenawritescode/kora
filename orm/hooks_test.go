package orm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/script"
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
	manager := &TxManager{
		DB: database, Transaction: dbTx, ScriptStore: &script.Store{DB: database, Dialect: kdb.Resolve("mysql")},
		AsyncHookInvocations: store, AsyncHookTenantID: tenant,
		AsyncHookSourceOperation: contract.OperationID(contract.NewOperationID()), AsyncHookConfigRevision: 42,
		Execution: execution, SiteName: "tenant-a", SkipHookScripts: []string{"other"},
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
