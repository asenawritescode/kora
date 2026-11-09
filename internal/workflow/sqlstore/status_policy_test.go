package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/workflow"
)

func TestGetStatusPolicyInTxReadsPublishedStatusAction(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := NewDefinitionStore(database, &db.MySQLDialect{})
	if err != nil {
		t.Fatal(err)
	}
	definition := statusActionTestDefinition("order-status", "Order")
	body, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := workflow.DefinitionDigest(definition)
	if err != nil {
		t.Fatal(err)
	}
	query := "SELECT d.definition_id,d.version,d.digest,d.definition_json FROM _kora_workflow_definition d INNER JOIN (SELECT definition_id,MAX(version) AS version FROM _kora_workflow_definition WHERE tenant_id=? GROUP BY definition_id) latest ON latest.definition_id=d.definition_id AND latest.version=d.version WHERE d.tenant_id=? ORDER BY d.definition_id"
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs("tenant-a", "tenant-a").WillReturnRows(sqlmock.NewRows([]string{"definition_id", "version", "digest", "definition_json"}).AddRow(string(definition.ID), int64(1), digest, body))
	tx, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	statusPolicy, found, err := store.GetStatusPolicyInTx(context.Background(), tx, "tenant-a", "Order")
	if err != nil {
		t.Fatal(err)
	}
	if !found || statusPolicy == nil || statusPolicy.StateField != "status" || statusPolicy.InitialState != "Draft" || len(statusPolicy.Transitions) != 1 || statusPolicy.Transitions[0].Action != "Approve" {
		t.Fatalf("status policy=(%+v,%t)", statusPolicy, found)
	}
	mock.ExpectCommit()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetStatusPolicyInTxRejectsTamperedDefinition(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := NewDefinitionStore(database, &db.MySQLDialect{})
	definition := statusActionTestDefinition("order-status", "Order")
	body, _ := json.Marshal(definition)
	query := "SELECT d.definition_id,d.version,d.digest,d.definition_json FROM _kora_workflow_definition d INNER JOIN (SELECT definition_id,MAX(version) AS version FROM _kora_workflow_definition WHERE tenant_id=? GROUP BY definition_id) latest ON latest.definition_id=d.definition_id AND latest.version=d.version WHERE d.tenant_id=? ORDER BY d.definition_id"
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs("tenant-a", "tenant-a").WillReturnRows(sqlmock.NewRows([]string{"definition_id", "version", "digest", "definition_json"}).AddRow(string(definition.ID), int64(1), "wrong-digest", body))
	tx, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.GetStatusPolicyInTx(context.Background(), tx, "tenant-a", "Order"); err == nil {
		t.Fatal("tampered status-action definition was accepted")
	}
	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil && err != sql.ErrTxDone {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func statusActionTestDefinition(id, table string) contract.WorkflowDefinition {
	input, _ := json.Marshal(workflow.StatusActionConfig{StateField: "status", InitialState: "Draft", StatusTransition: workflow.StatusTransition{Action: "Approve", From: "Draft", To: "Approved", AllowedRoles: []string{"Approver"}}})
	return contract.WorkflowDefinition{ID: contract.DefinitionID(id), Version: 1, Entry: "move", Trigger: &contract.RecordTrigger{Kind: "record.updated", Table: table}, Steps: []contract.StepDefinition{
		{ID: "move", Kind: contract.StepAction, Action: &contract.ActionSpec{Name: "record.update_status", Version: 1, InputSchema: "kora.record.update_status", Input: input, Next: "finish"}},
		{ID: "finish", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "complete"}},
	}}
}
