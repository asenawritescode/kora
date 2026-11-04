package orm

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/policy"
)

func TestGetDocUsesBoundTransactionInsteadOfAcquiringAnotherConnection(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create mock database:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	mock.ExpectBegin()
	tx, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal("begin transaction:", err)
	}
	defer tx.Rollback()

	created := time.Date(2026, 10, 14, 8, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT title, name, owner, creation, modified, modified_by, doc_status, revision FROM .* WHERE name = \?`).
		WithArgs("TASK-0001").
		WillReturnRows(sqlmock.NewRows([]string{"title", "name", "owner", "creation", "modified", "modified_by", "doc_status", "revision"}).
			AddRow("Bound", "TASK-0001", "user-1", created, created, "user-1", 0, 3))
	dt := &doctype.DocType{Name: "Task", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}}}
	manager := &TxManager{DB: database, Transaction: tx, Dialect: db.Resolve("mysql")}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	manager.Context = ctx
	doc, err := manager.GetDoc(dt, "TASK-0001", "")
	if err != nil {
		t.Fatalf("GetDoc with one-connection pool: %v", err)
	}
	if doc.Get("title") != "Bound" {
		t.Fatalf("loaded title = %#v, want Bound", doc.Get("title"))
	}
	if got, ok := doc.Get("modified").(time.Time); !ok || !got.Equal(created) {
		t.Fatalf("loaded modified = %#v, want persisted timestamp %s", doc.Get("modified"), created.Format(time.RFC3339Nano))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("transaction-bound query expectations: %v", err)
	}
}

func TestGetDocWithScopeCombinesOwnerAndAssignedUserSafely(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	created := time.Date(2026, 10, 18, 8, 0, 0, 0, time.UTC)
	mock.ExpectQuery("SELECT title, assigned_to, name, owner, creation, modified, modified_by, doc_status, revision FROM .* WHERE name = \\? AND \\(owner = \\? OR `assigned_to` = \\?\\)").
		WithArgs("TASK-1", "alice", "alice").
		WillReturnRows(sqlmock.NewRows([]string{"title", "assigned_to", "name", "owner", "creation", "modified", "modified_by", "doc_status", "revision"}).
			AddRow("Assigned task", "alice", "TASK-1", "creator", created, created, "creator", 0, 1))
	dt := &doctype.DocType{Name: "Task", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}, {Fieldname: "assigned_to", Fieldtype: "User"}}}
	manager := &TxManager{DB: database, Dialect: db.Resolve("mysql")}
	doc, err := manager.GetDocWithScope(dt, "TASK-1", policy.RowScope{UserID: "alice", Owner: true, AssignedFields: []string{"assigned_to"}})
	if err != nil {
		t.Fatalf("GetDocWithScope: %v", err)
	}
	if doc.Get("assigned_to") != "alice" {
		t.Fatalf("assigned_to = %#v", doc.Get("assigned_to"))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRowScopeRejectsUntrustedOrNonUserFields(t *testing.T) {
	manager := &TxManager{Dialect: db.Resolve("mysql")}
	for _, field := range []string{"unknown", "title", "assigned_to` OR 1=1 --"} {
		dt := &doctype.DocType{Name: "Task", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}, {Fieldname: "assigned_to", Fieldtype: "User"}}}
		if _, _, err := manager.rowScopePredicate(dt, policy.RowScope{UserID: "alice", AssignedFields: []string{field}}); err == nil {
			t.Errorf("row scope accepted invalid assignment field %q", field)
		}
	}
}

func TestScopedSaveAndDeleteRetainAssignmentPredicate(t *testing.T) {
	for _, operation := range []string{"save", "delete"} {
		t.Run(operation, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			mock.ExpectBegin()
			dbTx, err := database.Begin()
			if err != nil {
				t.Fatal(err)
			}
			manager := &TxManager{DB: database, Dialect: db.Resolve("mysql")}
			dt := &doctype.DocType{Name: "Task", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}, {Fieldname: "assigned_to", Fieldtype: "User"}}}
			old := doctype.NewDocument("Task")
			old.Name, old.Revision = "TASK-1", 7
			old.Set("title", "before")
			old.Set("assigned_to", "alice")
			scope := policy.RowScope{UserID: "alice", Owner: true, AssignedFields: []string{"assigned_to"}}
			if operation == "save" {
				doc := doctype.NewDocument("Task")
				doc.Name, doc.Revision = "TASK-1", 7
				doc.Set("title", "after")
				doc.Set("assigned_to", "bob")
				mock.ExpectExec("UPDATE `tabTask` SET title = \\?, assigned_to = \\?, modified = \\?, modified_by = \\?, doc_status = \\?, revision = revision \\+ 1 WHERE name = \\? AND revision = \\? AND \\(owner = \\? OR `assigned_to` = \\?\\)").
					WithArgs("after", "bob", sqlmock.AnyArg(), "alice", 0, "TASK-1", uint64(7), "alice", "alice").WillReturnResult(sqlmock.NewResult(0, 1))
				if err := manager.SaveInTxWithScope(dbTx, dt, doc, "alice", scope, old); err != nil {
					t.Fatalf("scoped save: %v", err)
				}
			} else {
				mock.ExpectExec("DELETE FROM `tabTask` WHERE name = \\? AND \\(owner = \\? OR `assigned_to` = \\?\\)").
					WithArgs("TASK-1", "alice", "alice").WillReturnResult(sqlmock.NewResult(0, 1))
				if err := manager.DeleteInTxWithScope(dbTx, dt, "TASK-1", scope, old); err != nil {
					t.Fatalf("scoped delete: %v", err)
				}
			}
			mock.ExpectRollback()
			if err := dbTx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
