package sqlstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
)

func TestPublishOwnerWorkflowDefinitionRejectedDuringDrain(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := NewDefinitionStore(database, &db.LibSQLDialect{})
	if err != nil {
		t.Fatal(err)
	}
	definition := contract.WorkflowDefinition{ID: "approval", Version: 1, OwnerModuleID: "finance.ar", Entry: "end", Steps: []contract.StepDefinition{{ID: "end", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "ok"}}}}
	now := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT command_digest, definition_id, version FROM _kora_workflow_publish_receipt WHERE tenant_id = ? AND actor_id = ? AND idempotency_key = ?")).WithArgs("tenant-a", "user-a", "publish-1").WillReturnRows(sqlmock.NewRows([]string{"command_digest", "definition_id", "version"}))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE _kora_module_snapshot SET revision=revision WHERE tenant_id=?")).WithArgs("tenant-a").WillReturnResult(sqlmock.NewResult(0, 1))
	snapshot, _ := json.Marshal(contract.ModuleSnapshot{TenantID: "tenant-a", Installations: []contract.ModuleInstallation{{TenantID: "tenant-a", ModuleID: "finance.ar", State: string(contract.ModuleDraining)}}})
	mock.ExpectQuery(regexp.QuoteMeta("SELECT snapshot_json FROM _kora_module_snapshot WHERE tenant_id=?")).WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"snapshot_json"}).AddRow(string(snapshot)))
	_, err = store.PublishDefinition(context.Background(), "tenant-a", definition, "user-a", "publish-1", now)
	var domainErr *contract.Error
	if !errors.As(err, &domainErr) || domainErr.Type != contract.CodePermissionDenied {
		t.Fatalf("publish during drain error=%v, want permission denied", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDefinitionSchemaStatementsCoverSupportedDialects(t *testing.T) {
	for name, dialect := range map[string]db.Dialect{"mysql": &db.MySQLDialect{}, "postgres": &db.PostgresDialect{}, "libsql": &db.LibSQLDialect{}} {
		statements, err := DefinitionSchemaStatements(dialect)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(statements) != 2 {
			t.Fatalf("%s schema statements = %d", name, len(statements))
		}
		for _, needle := range []string{"_kora_workflow_definition", "tenant_id", "PRIMARY KEY"} {
			if !containsAll(statements[0], needle) {
				t.Errorf("%s definition DDL missing %s", name, needle)
			}
		}
		if !containsAll(statements[1], "_kora_workflow_publish_receipt", "idempotency_key") {
			t.Errorf("%s receipt DDL incomplete", name)
		}
	}
}

func TestDefinitionStoreEnsureDropsRetiredRuntimeTables(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	dialect := &db.MySQLDialect{}
	store, err := NewDefinitionStore(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"_kora_workflow_audit", "_kora_workflow_timer", "_kora_workflow_actor"} {
		mock.ExpectExec(regexp.QuoteMeta("DROP TABLE IF EXISTS " + dialect.QuoteIdent(table))).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	definitionStatements, err := DefinitionSchemaStatements(dialect)
	if err != nil {
		t.Fatal(err)
	}
	executionStatements, err := ExecutionSchemaStatements(dialect)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range append(definitionStatements, executionStatements...) {
		mock.ExpectExec(regexp.QuoteMeta(statement)).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM _kora_workflow_definition WHERE JSON_EXTRACT(definition_json, '$.lifecycle') IS NOT NULL")).WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.Ensure(context.Background()); err != nil {
		t.Fatal("ensure unified workflow storage:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPublishDefinitionPersistsVersionAndScopedReceiptAtomically(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := NewDefinitionStore(database, &db.MySQLDialect{})
	if err != nil {
		t.Fatal(err)
	}
	definition := contract.WorkflowDefinition{ID: "approval", Version: 1, Entry: "end", Steps: []contract.StepDefinition{{ID: "end", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "ok"}}}}
	body, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	now := time.Date(2026, 10, 14, 8, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT command_digest, definition_id, version FROM _kora_workflow_publish_receipt WHERE tenant_id = ? AND actor_id = ? AND idempotency_key = ? FOR UPDATE")).WithArgs("tenant-a", "user-a", "publish-1").WillReturnRows(sqlmock.NewRows([]string{"command_digest", "definition_id", "version"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT digest FROM _kora_workflow_definition WHERE tenant_id = ? AND definition_id = ? AND version = ?")).WithArgs("tenant-a", "approval", uint32(1)).WillReturnRows(sqlmock.NewRows([]string{"digest"}))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_workflow_definition (tenant_id, definition_id, version, digest, definition_json, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)")).WithArgs("tenant-a", "approval", uint32(1), digest, string(body), "user-a", now).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_workflow_publish_receipt (tenant_id, actor_id, idempotency_key, command_digest, definition_id, version, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)")).WithArgs("tenant-a", "user-a", "publish-1", digest, "approval", uint32(1), now).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	ref, err := store.PublishDefinition(context.Background(), "tenant-a", definition, "user-a", "publish-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Digest != digest || ref.DefinitionID != "approval" || ref.Version != 1 {
		t.Fatalf("unexpected reference: %+v", ref)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPublishDefinitionRejectsChangedImmutableVersion(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := NewDefinitionStore(database, &db.MySQLDialect{})
	definition := contract.WorkflowDefinition{ID: "approval", Version: 1, Entry: "end", Steps: []contract.StepDefinition{{ID: "end", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "changed"}}}}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT command_digest, definition_id, version FROM _kora_workflow_publish_receipt WHERE tenant_id = ? AND actor_id = ? AND idempotency_key = ? FOR UPDATE")).WithArgs("tenant-a", "user-a", "publish-1").WillReturnRows(sqlmock.NewRows([]string{"command_digest", "definition_id", "version"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT digest FROM _kora_workflow_definition WHERE tenant_id = ? AND definition_id = ? AND version = ?")).WithArgs("tenant-a", "approval", uint32(1)).WillReturnRows(sqlmock.NewRows([]string{"digest"}).AddRow("different"))
	_, err = store.PublishDefinition(context.Background(), "tenant-a", definition, "user-a", "publish-1", time.Now())
	if err == nil {
		t.Fatal("changed immutable version was accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListLatestDefinitionsIsTenantScopedAndReturnsNewestVersions(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := NewDefinitionStore(database, &db.MySQLDialect{})
	if err != nil {
		t.Fatal(err)
	}
	query := `SELECT d.definition_id, d.version, d.digest FROM _kora_workflow_definition d
		INNER JOIN (SELECT definition_id, MAX(version) AS version FROM _kora_workflow_definition WHERE tenant_id = ? GROUP BY definition_id) latest
		ON latest.definition_id = d.definition_id AND latest.version = d.version WHERE d.tenant_id = ? ORDER BY d.definition_id`
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs("tenant-a", "tenant-a").WillReturnRows(sqlmock.NewRows([]string{"definition_id", "version", "digest"}).AddRow("leave-review", uint32(3), "digest-3").AddRow("purchase-approval", uint32(1), "digest-1"))
	refs, err := store.ListLatestDefinitions(context.Background(), "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0].DefinitionID != "leave-review" || refs[0].Version != 3 || refs[1].DefinitionID != "purchase-approval" {
		t.Fatalf("unexpected latest definitions: %+v", refs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func containsAll(value string, fragments ...string) bool {
	for _, fragment := range fragments {
		if !regexp.MustCompile(regexp.QuoteMeta(fragment)).MatchString(value) {
			return false
		}
	}
	return true
}
