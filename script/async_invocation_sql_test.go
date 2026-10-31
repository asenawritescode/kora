package script

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/policy"
)

func TestAsyncHookInvocationSchemaStatementsCoverSupportedDialects(t *testing.T) {
	for _, name := range []string{"mysql", "postgres", "libsql"} {
		t.Run(name, func(t *testing.T) {
			statements, err := AsyncHookInvocationSchemaStatements(kdb.Resolve(name))
			if err != nil || len(statements) == 0 {
				t.Fatalf("schema statements = %q, %v", statements, err)
			}
			joined := strings.Join(statements, "\n")
			for _, required := range []string{"_kora_script_invocation", "external_effect_started", "lease_fence", "payload_json"} {
				if !strings.Contains(joined, required) {
					t.Errorf("%s schema lacks %q", name, required)
				}
			}
		})
	}
}

func TestAppendAsyncHookInvocationUsesSourceTransaction(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	store := &Store{DB: database, Dialect: kdb.Resolve("mysql")}
	mock.ExpectBegin()
	tx, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal("begin source transaction:", err)
	}
	query := `INSERT INTO _kora_script_invocation (id, tenant_id, source_operation_id, sequence_no, status, payload_json, attempts, lease_owner, lease_fence, lease_until, external_effect_started, next_attempt_at, last_error, created_at, updated_at) VALUES (?, ?, ?, ?, 'pending', ?, 0, NULL, 0, NULL, FALSE, CURRENT_TIMESTAMP(6), '', CURRENT_TIMESTAMP(6), CURRENT_TIMESTAMP(6))`
	mock.ExpectExec(regexp.QuoteMeta(query)).WithArgs(sqlmock.AnyArg(), "tenant-a", sqlmock.AnyArg(), uint16(1), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	invocation := AsyncHookInvocation{
		TenantID: "tenant-a", Site: "tenant-a", SourceOperation: contract.OperationID(contract.NewOperationID()), ConfigRevision: 9, Sequence: 1,
		Doctype: "Invoice", Event: EventAfterSave,
		Record:   ScriptRecord{Name: "after-save", Site: "tenant-a", ScriptType: TypeDocEvent, DocType: "Invoice", Event: EventAfterSave, IsActive: true, TimeoutMs: 1000, Script: "return true"},
		Document: []byte(`{"name":"INV-0001","total":12}`),
		Actor:    contract.ActorContext{PrincipalID: "user-1", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", AuthenticatedAt: time.Now().UTC()},
	}
	if err := store.AppendAsyncHookTx(context.Background(), tx, invocation); err != nil {
		t.Fatal("append async hook invocation:", err)
	}
	mock.ExpectCommit()
	if err := tx.Commit(); err != nil {
		t.Fatal("commit source transaction:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestAppendAsyncHookInvocationRejectsInvalidGrant(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	mock.ExpectBegin()
	tx, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal("begin source transaction:", err)
	}
	defer tx.Rollback()
	invocation := AsyncHookInvocation{
		TenantID: "tenant-a", Site: "tenant-a", SourceOperation: contract.OperationID(contract.NewOperationID()), Sequence: 1,
		Doctype: "Invoice", Event: EventAfterSave,
		Record:   ScriptRecord{Name: "after-save", Site: "tenant-a", ScriptType: TypeDocEvent, TimeoutMs: 1000, Script: "return true"},
		Document: []byte(`{"name":"INV-0001"}`),
		Actor:    contract.ActorContext{PrincipalID: "user-1", PrincipalType: contract.PrincipalHuman, Site: "tenant-a"},
		Grants:   []policy.Grant{{Resource: "Invoice", Operation: "*"}},
	}
	if err := (&Store{DB: database, Dialect: kdb.Resolve("mysql")}).AppendAsyncHookTx(context.Background(), tx, invocation); err == nil {
		t.Fatal("invalid execution grant was persisted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}
