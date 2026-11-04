package orm

import (
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/policy"
)

func entityOwnedInvoice() *doctype.DocType {
	return &doctype.DocType{
		Name: "Invoice", BusinessScopePolicy: contract.ScopeEntityOwned, BusinessScopeField: "legal_entity_id",
		Fields: []doctype.Field{{Fieldname: "legal_entity_id", Fieldtype: "Link", Options: "LegalEntity"}, {Fieldname: "amount", Fieldtype: "Currency"}},
	}
}

func TestEntityOwnedReadsIncludeVerifiedEntityPredicate(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	created := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT legal_entity_id, amount, name, owner, creation, modified, modified_by, doc_status, revision FROM `tabInvoice` WHERE name = ? AND (`legal_entity_id` = ?)")).
		WithArgs("INV-1", "entity-a").
		WillReturnRows(sqlmock.NewRows([]string{"legal_entity_id", "amount", "name", "owner", "creation", "modified", "modified_by", "doc_status", "revision"}).AddRow("entity-a", 12.0, "INV-1", "alice", created, created, "alice", 0, 1))
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	manager := &TxManager{DB: database, Dialect: db.Resolve("mysql")}
	doc, err := manager.GetDocWithScope(entityOwnedInvoice(), "INV-1", policy.RowScope{Business: scope})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Get("legal_entity_id") != "entity-a" {
		t.Fatalf("read crossed entity scope: %#v", doc.Get("legal_entity_id"))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEntityOwnedReadAndWritesFailClosedWithoutScopeOrOnForgedEntity(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	manager := &TxManager{DB: database, Dialect: db.Resolve("mysql")}
	dt := entityOwnedInvoice()
	if _, err := manager.GetDocWithScope(dt, "INV-1", policy.RowScope{}); err == nil {
		t.Fatal("entity-owned read without scope succeeded")
	}
	doc := doctype.NewDocument(dt.Name)
	doc.Set("legal_entity_id", "entity-b")
	if err := manager.InsertInTx(nil, dt, doc, "alice", "alice"); err == nil {
		t.Fatal("insert accepted a forged entity field without verified scope")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
