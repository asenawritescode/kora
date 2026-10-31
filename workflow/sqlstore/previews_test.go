package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/workflow"
)

func TestWorkflowPreviewHistoryPersistsOnlyRedactedOutcomeAndSteps(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := NewDefinitionStore(database, &db.MySQLDialect{})
	if err != nil {
		t.Fatal(err)
	}
	steps := []workflow.PreviewStep{{StepID: "email", Kind: "action", Result: "would run email.send (not sent during preview)"}}
	encodedSteps, _ := json.Marshal(steps)
	digest := strings.Repeat("a", 64)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_workflow_preview (id,tenant_id,actor_id,definition_id,definition_version,definition_digest,status,outcome,failure,steps_json,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,CURRENT_TIMESTAMP(6))")).
		WithArgs("preview-1", "tenant-a", "operator-a", "notify", uint32(1), digest, "succeeded", "complete", "", string(encodedSteps)).WillReturnResult(sqlmock.NewResult(1, 1))
	if err := store.RecordPreview(context.Background(), "tenant-a", "operator-a", workflow.PreviewRun{ID: "preview-1", DefinitionID: "notify", DefinitionVersion: 1, DefinitionDigest: digest, Status: "succeeded", Outcome: "complete", Steps: steps}); err != nil {
		t.Fatal(err)
	}

	created := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	query := "SELECT id,definition_id,definition_version,definition_digest,status,outcome,failure,steps_json,created_at FROM _kora_workflow_preview WHERE tenant_id = ? ORDER BY created_at DESC,id DESC LIMIT ?"
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs("tenant-a", int64(20)).WillReturnRows(sqlmock.NewRows([]string{"id", "definition_id", "definition_version", "definition_digest", "status", "outcome", "failure", "steps_json", "created_at"}).AddRow("preview-1", "notify", uint32(1), digest, "succeeded", "complete", "", string(encodedSteps), created))
	runs, err := store.ListPreviews(context.Background(), "tenant-a", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != "preview-1" || runs[0].Status != "succeeded" || len(runs[0].Steps) != 1 {
		t.Fatalf("unexpected preview history: %#v", runs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowPreviewGateMatchesTenantDefinitionVersionAndDigest(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := NewDefinitionStore(database, &db.MySQLDialect{})
	if err != nil {
		t.Fatal(err)
	}
	query := "SELECT 1 FROM _kora_workflow_preview WHERE tenant_id=? AND definition_id=? AND definition_version=? AND definition_digest=? AND status='succeeded' LIMIT 1"
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs("tenant-a", "notify", uint32(2), strings.Repeat("a", 64)).WillReturnRows(sqlmock.NewRows([]string{"1"}).AddRow(1))
	passed, err := store.HasSuccessfulPreview(context.Background(), "tenant-a", "notify", 2, strings.Repeat("a", 64))
	if err != nil || !passed {
		t.Fatalf("successful matching preview = %v, %v", passed, err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs("tenant-a", "notify", uint32(2), strings.Repeat("b", 64)).WillReturnError(sql.ErrNoRows)
	passed, err = store.HasSuccessfulPreview(context.Background(), "tenant-a", "notify", 2, strings.Repeat("b", 64))
	if err != nil || passed {
		t.Fatalf("missing matching preview = %v, %v", passed, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
