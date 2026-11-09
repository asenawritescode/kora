package sqlstore

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

func TestAuthorizationStoreReadsLiveMembershipSubjectAndStarter(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{{Name: "Invoice", Fields: []doctype.Field{{Fieldname: "amount", Fieldtype: "Currency"}}}}, nil, nil)
	store, err := NewAuthorizationStore(database, db.Resolve("mysql"), registry)
	if err != nil {
		t.Fatal("construct authorization store:", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT enabled, roles FROM _kora_user WHERE site = ? AND name = ?")).WithArgs("tenant-a.example.test", "user-a").WillReturnRows(sqlmock.NewRows([]string{"enabled", "roles"}).AddRow(true, "Reader, Auditor,Reader"))
	roles, active, err := store.UserRoles(context.Background(), "tenant-a.example.test", "user-a")
	if err != nil || !active || len(roles) != 2 || roles[0] != "Reader" || roles[1] != "Auditor" {
		t.Fatalf("live membership = (%v,%t,%v)", roles, active, err)
	}

	mock.ExpectBegin()
	docQuery := "SELECT amount, name, owner, creation, modified, modified_by, doc_status, revision FROM `tabInvoice` WHERE name = ?"
	mock.ExpectQuery(regexp.QuoteMeta(docQuery)).WithArgs("INV-1").WillReturnRows(sqlmock.NewRows([]string{"amount", "name", "owner", "creation", "modified", "modified_by", "doc_status", "revision"}).AddRow("8.5", "INV-1", "user-a", time.Now(), time.Now(), "user-a", 0, 4))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `owner` FROM `tabInvoice` WHERE `name` = ?")).WithArgs("INV-1").WillReturnRows(sqlmock.NewRows([]string{"owner"}).AddRow("user-a"))
	mock.ExpectCommit()
	subject, err := store.Subject(context.Background(), contract.SubjectRef{Kind: "Invoice", ID: "INV-1", Revision: "4"})
	if err != nil || subject.Owner != "user-a" || subject.Revision != "4" {
		t.Fatalf("subject authorization snapshot = (%+v,%v)", subject, err)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT actor_id FROM _kora_workflow_history WHERE tenant_id = ? AND instance_id = ? AND sequence = 1 AND kind = 'started'")).WithArgs("tenant-a", "01K7INSTANCE00000000000000000").WillReturnRows(sqlmock.NewRows([]string{"actor_id"}).AddRow("user-a"))
	starter, err := store.WorkflowStarter(context.Background(), "tenant-a", "01K7INSTANCE00000000000000000")
	if err != nil || starter != "user-a" {
		t.Fatalf("workflow starter = (%q,%v)", starter, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}
