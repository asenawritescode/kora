package sqlstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
)

func TestResumeDueTimedWaitCommitsTimeoutTransitionAndEventTogether(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := NewExecutionStore(database, &db.MySQLDialect{})
	if err != nil {
		t.Fatal(err)
	}
	definition := contract.WorkflowDefinition{ID: "flow-1", Version: 2, Entry: "pause", Steps: []contract.StepDefinition{
		{ID: "pause", Kind: contract.StepWait, Wait: &contract.WaitSpec{TimeoutSeconds: 10, OnTimeout: "expired"}},
		{ID: "expired", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "expired"}},
	}}
	body, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	databaseNow := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT i.id,i.current_step,i.revision,i.definition_id,i.definition_version,CURRENT_TIMESTAMP(6) FROM _kora_workflow_instance i JOIN _kora_workflow_process_timer t ON t.tenant_id=i.tenant_id AND t.instance_id=i.id AND t.step_id=i.current_step WHERE i.tenant_id=? AND i.status='waiting' AND t.due_at<=CURRENT_TIMESTAMP(6) AND NOT EXISTS (SELECT 1 FROM _kora_request r WHERE r.tenant_id=i.tenant_id AND r.source_instance_id=i.id AND r.source_step_id=i.current_step) ORDER BY t.due_at,i.id LIMIT ? FOR UPDATE SKIP LOCKED")).
		WithArgs("tenant-a", int64(5)).WillReturnRows(sqlmock.NewRows([]string{"id", "step", "revision", "definition_id", "version", "now"}).AddRow("instance-1", "pause", int64(8), "flow-1", int64(2), databaseNow))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT digest,definition_json FROM _kora_workflow_definition WHERE tenant_id=? AND definition_id=? AND version=?")).
		WithArgs("tenant-a", "flow-1", int64(2)).WillReturnRows(sqlmock.NewRows([]string{"digest", "body"}).AddRow(hex.EncodeToString(digest[:]), string(body)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(sequence),0)+1 FROM _kora_workflow_history WHERE tenant_id=? AND instance_id=?")).
		WithArgs("tenant-a", "instance-1").WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(int64(3)))
	mock.ExpectExec("UPDATE _kora_workflow_instance SET status='ready',current_step=.*WHERE tenant_id=.*").
		WithArgs("expired", "tenant-a", "instance-1", "pause", int64(8)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM _kora_workflow_process_timer WHERE tenant_id=? AND instance_id=? AND step_id=?")).
		WithArgs("tenant-a", "instance-1", "pause").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM _kora_workflow_subscription WHERE tenant_id=? AND instance_id=? AND step_id=?")).
		WithArgs("tenant-a", "instance-1", "pause").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO _kora_workflow_history .*VALUES ").
		WithArgs("tenant-a", "instance-1", uint64(3), "pause", "timeout", "expired").WillReturnResult(sqlmock.NewResult(0, 1))
	expectWorkflowSourceSequence(mock, "tenant-a", 1)
	mock.ExpectExec("INSERT INTO _kora_outbox .*VALUES ").
		WithArgs(sqlmock.AnyArg(), "tenant-a", int64(1), "kora.workflow.timeout", contract.CurrentVersion, "workflow_instance", "instance-1", sqlmock.AnyArg(), "pending", 0, databaseNow).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	resumed, err := store.ResumeDueTimedWaits(context.Background(), "tenant-a", 5)
	if err != nil || resumed != 1 {
		t.Fatalf("ResumeDueTimedWaits = %d, %v", resumed, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("timed wait transaction:", err)
	}
}

func TestResumeDueTimedWaitRollsBackWhenOutboxWriteFails(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := NewExecutionStore(database, &db.MySQLDialect{})
	definition := contract.WorkflowDefinition{ID: "flow-1", Version: 1, Entry: "pause", Steps: []contract.StepDefinition{
		{ID: "pause", Kind: contract.StepWait, Wait: &contract.WaitSpec{TimeoutSeconds: 10, OnTimeout: "expired"}},
	}}
	body, _ := json.Marshal(definition)
	digest := sha256.Sum256(body)
	databaseNow := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT i.id,i.current_step,i.revision,i.definition_id,i.definition_version,CURRENT_TIMESTAMP\\(6\\).*").WithArgs("tenant-a", int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "step", "revision", "definition_id", "version", "now"}).AddRow("instance-1", "pause", int64(2), "flow-1", int64(1), databaseNow))
	mock.ExpectQuery("SELECT digest,definition_json FROM _kora_workflow_definition.*").WithArgs("tenant-a", "flow-1", int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{"digest", "body"}).AddRow(hex.EncodeToString(digest[:]), string(body)))
	mock.ExpectQuery("SELECT COALESCE\\(MAX\\(sequence\\),0\\)\\+1 FROM _kora_workflow_history.*").WithArgs("tenant-a", "instance-1").
		WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(int64(1)))
	mock.ExpectExec("UPDATE _kora_workflow_instance SET status='ready'.*").WithArgs("expired", "tenant-a", "instance-1", "pause", int64(2)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM _kora_workflow_process_timer.*").WithArgs("tenant-a", "instance-1", "pause").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM _kora_workflow_subscription.*").WithArgs("tenant-a", "instance-1", "pause").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO _kora_workflow_history .*VALUES ").WillReturnResult(sqlmock.NewResult(0, 1))
	expectWorkflowSourceSequence(mock, "tenant-a", 1)
	mock.ExpectExec("INSERT INTO _kora_outbox .*VALUES ").WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()

	if _, err := store.ResumeDueTimedWaits(context.Background(), "tenant-a", 1); err == nil {
		t.Fatal("timed wait recovery accepted failed outbox persistence")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("failed timed wait transaction:", err)
	}
}
