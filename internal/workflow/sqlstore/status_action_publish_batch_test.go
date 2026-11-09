package sqlstore

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/workflow"
)

func TestPublishImportedBatchCommitsAllDefinitionsTogether(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := NewDefinitionStore(database, &db.MySQLDialect{})
	definitions := []contract.WorkflowDefinition{statusActionTestDefinition("order-status-one", "One"), statusActionTestDefinition("order-status-two", "Two")}
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	for _, definition := range definitions {
		body, _ := json.Marshal(definition)
		digest, err := workflow.DefinitionDigest(definition)
		if err != nil {
			t.Fatal(err)
		}
		mock.ExpectQuery(regexp.QuoteMeta("SELECT digest FROM _kora_workflow_definition WHERE tenant_id=? AND definition_id=? AND version=?")).WithArgs("tenant-a", string(definition.ID), uint32(1)).WillReturnRows(sqlmock.NewRows([]string{"digest"}))
		mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_workflow_definition (tenant_id,definition_id,version,digest,definition_json,created_by,created_at) VALUES (?,?,?,?,?,?,?)")).WithArgs("tenant-a", string(definition.ID), uint32(1), digest, string(body), "system-import", now).WillReturnResult(sqlmock.NewResult(1, 1))
	}
	mock.ExpectCommit()
	if err := store.PublishImportedBatch(context.Background(), "tenant-a", "system-import", definitions, now); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPublishImportedBatchRollsBackOnConflict(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := NewDefinitionStore(database, &db.MySQLDialect{})
	first, second := statusActionTestDefinition("order-status-one", "One"), statusActionTestDefinition("order-status-two", "Two")
	now := time.Now().UTC()
	mock.ExpectBegin()
	body, _ := json.Marshal(first)
	digest, err := workflow.DefinitionDigest(first)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT digest FROM _kora_workflow_definition WHERE tenant_id=? AND definition_id=? AND version=?")).WithArgs("tenant-a", string(first.ID), uint32(1)).WillReturnRows(sqlmock.NewRows([]string{"digest"}))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_workflow_definition (tenant_id,definition_id,version,digest,definition_json,created_by,created_at) VALUES (?,?,?,?,?,?,?)")).WithArgs("tenant-a", string(first.ID), uint32(1), digest, string(body), "system-import", now).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT digest FROM _kora_workflow_definition WHERE tenant_id=? AND definition_id=? AND version=?")).WithArgs("tenant-a", string(second.ID), uint32(1)).WillReturnRows(sqlmock.NewRows([]string{"digest"}).AddRow("different-digest"))
	mock.ExpectRollback()
	if err := store.PublishImportedBatch(context.Background(), "tenant-a", "system-import", []contract.WorkflowDefinition{first, second}, now); err == nil {
		t.Fatal("conflicting second policy unexpectedly committed")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
