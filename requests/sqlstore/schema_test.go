package sqlstore

import (
	"strings"
	"testing"

	"github.com/asenawritescode/kora/db"
)

func TestSchemaStatementsCoverRequestLifecycleTablesForEachDialect(t *testing.T) {
	for _, dialectName := range []string{"libsql", "postgres", "mysql"} {
		t.Run(dialectName, func(t *testing.T) {
			statements, err := SchemaStatements(db.Resolve(dialectName))
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(statements, "\n")
			for _, table := range []string{
				"_kora_request", "_kora_request_assignment", "_kora_request_decision",
				"_kora_request_history", "_kora_request_read", "_kora_request_idempotency",
			} {
				if !strings.Contains(joined, "CREATE TABLE IF NOT EXISTS "+table) {
					t.Errorf("%s schema omitted %s", dialectName, table)
				}
			}
			for _, index := range []string{
				"idx_kora_request_status_due", "idx_kora_request_workflow_source", "idx_kora_request_requester_created",
				"idx_kora_request_assignment_actor_status", "uq_kora_request_final_decision",
				"idx_kora_request_idempotency_expiry",
			} {
				if !strings.Contains(joined, index) {
					t.Errorf("%s schema omitted required inbox/decision index %s", dialectName, index)
				}
			}
			for _, column := range []string{"assignment_mode", "supersedes_request_id", "resolved_at", "continuation_operation_id", "subject_digest", "previous_assignee_json", "new_assignee_json"} {
				if !strings.Contains(joined, column) {
					t.Errorf("%s schema omitted request state field %s", dialectName, column)
				}
			}
			if strings.Contains(joined, "DROP TABLE") || strings.Contains(joined, "TRUNCATE") {
				t.Fatalf("%s schema contains destructive DDL", dialectName)
			}
		})
	}
}

func TestSchemaStatementsRejectUnknownDialect(t *testing.T) {
	if _, err := SchemaStatements(nil); err == nil {
		t.Fatal("nil/unknown dialect accepted")
	}
	var postgres *db.PostgresDialect
	if _, err := SchemaStatements(postgres); err == nil {
		t.Fatal("typed nil dialect accepted")
	}
}
