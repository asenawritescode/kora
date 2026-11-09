package engine

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/script"
)

type durableLifecycleHookRunner struct {
	called  bool
	request script.ExecuteRequest
}

func (r *durableLifecycleHookRunner) Execute(_ context.Context, request script.ExecuteRequest) (*script.ExecuteResult, error) {
	r.called, r.request = true, request
	return &script.ExecuteResult{}, nil
}
func (*durableLifecycleHookRunner) Validate(string) error { return nil }
func (*durableLifecycleHookRunner) Close() error          { return nil }

type panicDurableLifecycleHookRunner struct{}

func (panicDurableLifecycleHookRunner) Execute(context.Context, script.ExecuteRequest) (*script.ExecuteResult, error) {
	panic("runner failure")
}
func (panicDurableLifecycleHookRunner) Validate(string) error { return nil }
func (panicDurableLifecycleHookRunner) Close() error          { return nil }

func TestDurableLifecycleHookRunnerPanicBecomesRecoverableFailure(t *testing.T) {
	_, err := executeDurableLifecycleHookSafely(panicDurableLifecycleHookRunner{}, context.Background(), script.ExecuteRequest{})
	if err == nil || err.Error() != "lifecycle hook panic: runner failure" {
		t.Fatalf("panic result error = %v, want recoverable lifecycle hook error", err)
	}
}

func TestProcessOneDurableLifecycleHookCommitsFencedReceipt(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	store := &script.Store{DB: database, Dialect: kdb.Resolve("mysql")}
	actor := contract.ActorContext{
		PrincipalID: "user-1", SubjectUserID: "alice@example.test", PrincipalType: contract.PrincipalHuman,
		Site: "tenant-a", Roles: []string{doctype.AdminRole}, AuthenticatedAt: time.Now().UTC(),
	}
	invocation := script.AsyncHookInvocation{
		TenantID: "tenant-a", Site: "tenant-a", SourceOperation: contract.OperationID(contract.NewOperationID()),
		ConfigRevision: 8, Sequence: 1, Doctype: "Task", Event: script.EventAfterSave,
		Record: script.ScriptRecord{Name: "task-after-save", Site: "tenant-a", ScriptType: script.TypeDocEvent,
			DocType: "Task", Event: script.EventAfterSave, IsActive: true, TimeoutMs: 5000, Script: "return true"},
		Document: json.RawMessage(`{"name":"TASK-1","title":"New"}`),
		Actor:    actor,
	}
	payload, err := json.Marshal(invocation)
	if err != nil {
		t.Fatal("encode invocation fixture:", err)
	}
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE _kora_script_invocation SET status = 'uncertain'").WithArgs("tenant-a").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, payload_json, attempts, lease_fence, external_effect_started FROM _kora_script_invocation WHERE tenant_id = ? AND ((status = 'pending' AND next_attempt_at <= CURRENT_TIMESTAMP(6)) OR (status = 'running' AND external_effect_started = FALSE AND lease_until <= CURRENT_TIMESTAMP(6))) ORDER BY next_attempt_at, id LIMIT ? FOR UPDATE SKIP LOCKED")).
		WithArgs("tenant-a", uint16(1)).WillReturnRows(sqlmock.NewRows([]string{"id", "payload_json", "attempts", "lease_fence", "external_effect_started"}).AddRow("invocation-id", payload, 0, 0, false))
	mock.ExpectExec("UPDATE _kora_script_invocation SET status = 'running'").WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "invocation-id", "tenant-a", uint64(0)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id FROM _kora_script_invocation").WithArgs("tenant-a", string(invocation.SourceOperation), uint16(1), sqlmock.AnyArg(), uint64(1)).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("invocation-id"))
	mock.ExpectExec("UPDATE _kora_script_invocation SET status = 'succeeded'").WithArgs("tenant-a", string(invocation.SourceOperation), uint16(1), sqlmock.AnyArg(), uint64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec("INSERT INTO _kora_script_execution").WithArgs(sqlmock.AnyArg(), "tenant-a", "task-after-save", string(script.TypeDocEvent), "Task", "TASK-1", string(script.EventAfterSave), "alice@example.test", sqlmock.AnyArg(), "success", "").WillReturnResult(sqlmock.NewResult(1, 1))

	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Task"})
	runner := &durableLifecycleHookRunner{}
	worked, err := processOneDurableLifecycleHook(context.Background(), runner, store, database, registry, kdb.Resolve("mysql"), nil,
		"tenant-a", "tenant-a", "worker-1", nil, nil, nil)
	if err != nil || !worked {
		t.Fatalf("process durable lifecycle invocation = (%t, %v), want (true, nil)", worked, err)
	}
	if !runner.called || runner.request.Provider == nil || runner.request.Script != invocation.Record.Script || runner.request.Document["title"] != "New" || runner.request.User != actor.SubjectUserID {
		t.Fatalf("worker did not execute frozen authorized invocation with provider: %+v", runner.request)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}
