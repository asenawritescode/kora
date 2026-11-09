package ai

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
)

func TestApprovalStoreUsesTargetFingerprint(t *testing.T) {
	args := map[string]any{"yaml": "name: X"}
	target := ApprovalTarget{Site: "s", OperationID: "run-1", ToolName: "create_doctype_draft", Args: args}
	fp := approvalFingerprint("create_doctype_draft", args)
	if got := target.fingerprint(); got != fp {
		t.Fatalf("fingerprint = %q, want %q", got, fp)
	}

	tests := []struct {
		name string
		run  func(t *testing.T, db *sqlmockDB)
	}{
		{"EnsureApprovalPending", func(t *testing.T, d *sqlmockDB) {
			d.mock.ExpectExec("INSERT INTO _kora_ai_approval").
				WithArgs(sqlmock.AnyArg(), "s", "run-1", "alice", "agent", "create_doctype_draft", "pending_approval", fp, fp, 3, sqlmock.AnyArg(), nil, nil, "", "").
				WillReturnResult(sqlmock.NewResult(1, 1))
			req := ApprovalRequest{Target: target, ActorID: "alice", ActorType: "agent", RecordVersion: 3}
			if err := EnsureApprovalPending(context.Background(), d.db, req); err != nil {
				t.Fatal(err)
			}
		}},
		{"HasGrantedApproval", func(t *testing.T, d *sqlmockDB) {
			d.mock.ExpectQuery("SELECT COUNT").
				WithArgs("s", "run-1", "create_doctype_draft", fp).
				WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
			ok, err := HasGrantedApproval(context.Background(), d.db, target)
			if err != nil || !ok {
				t.Fatalf("HasGrantedApproval = %v, %v", ok, err)
			}
		}},
		{"GrantApprovalForOperation looks up the pending row", func(t *testing.T, d *sqlmockDB) {
			d.mock.ExpectQuery("SELECT id").
				WithArgs("s", "run-1", "create_doctype_draft", fp).
				WillReturnRows(sqlmock.NewRows([]string{"id"}))
			if _, err := GrantApprovalForOperation(context.Background(), d.db, target, "alice"); err == nil {
				t.Fatal("expected no pending approval to grant")
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newSQLMockDB(t)
			tt.run(t, d)
			if err := d.mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestApprovalStoreWithoutDatabase(t *testing.T) {
	target := ApprovalTarget{Site: "s", OperationID: "run-1", ToolName: "t"}
	if err := EnsureApprovalPending(context.Background(), nil, ApprovalRequest{Target: target}); err != nil {
		t.Fatalf("EnsureApprovalPending(nil db) = %v", err)
	}
	if ok, err := HasGrantedApproval(context.Background(), nil, target); ok || err != nil {
		t.Fatalf("HasGrantedApproval(nil db) = %v, %v", ok, err)
	}
	if _, err := GrantApprovalForOperation(context.Background(), nil, target, "alice"); err == nil {
		t.Fatal("GrantApprovalForOperation(nil db) should fail")
	}
}

func TestExecuteToolExecutionFallback(t *testing.T) {
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "PurchaseRequest"})
	valid := testAIExecution(t, "site", "alice", []string{doctype.AdminRole})
	tests := []struct {
		name        string
		txExecution bool
		reqExec     bool
		confirmed   bool
		want        string
	}{
		{"no execution anywhere is denied", false, false, false, "verified execution is required"},
		{"zero request execution falls back to the transaction manager's", true, false, false, "Unknown tool"},
		{"request execution is used", false, true, false, "Unknown tool"},
		{"confirmed call needs one too", false, false, true, "verified execution is required"},
		{"confirmed call falls back like an unconfirmed one", true, false, true, "Unknown tool"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database, _, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			tx := &orm.TxManager{DB: database, Dialect: db.Resolve("mysql"), Context: context.Background()}
			req := ToolRequest{Name: "no_such_tool_xyz", Owner: "alice", Site: "site", Confirmed: tt.confirmed, IdempotencyKey: "key-1"}
			if tt.txExecution {
				tx.Execution = valid
			}
			if tt.reqExec {
				req.Execution = valid
			}
			if got := ExecuteTool(tx, reg, req); !strings.Contains(got, tt.want) {
				t.Fatalf("ExecuteTool = %q, want it to contain %q", got, tt.want)
			}
		})
	}
}

type sqlmockDB struct {
	db   *sql.DB
	mock sqlmock.Sqlmock
}

func newSQLMockDB(t *testing.T) *sqlmockDB {
	t.Helper()
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return &sqlmockDB{db: database, mock: mock}
}
