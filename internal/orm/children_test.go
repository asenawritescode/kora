package orm

import (
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

func TestReconcileChildren_InsertsUnnamedNewRows(t *testing.T) {
	dbConn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer dbConn.Close()

	parentDT := &doctype.DocType{Name: "Product"}
	childDT := &doctype.DocType{Name: "Stock Move", IsChildTable: true, Fields: []doctype.Field{
		{Fieldname: "movement_type", Fieldtype: "Data"},
		{Fieldname: "qty_change", Fieldtype: "Float"},
		{Fieldname: "reference", Fieldtype: "Data"},
	}}
	oldChildren := []*doctype.Document{
		{DocType: "Stock Move", Name: "SM-0001", Fields: map[string]any{"movement_type": "Opening Stock", "qty_change": float64(45), "reference": "Opening Stock"}},
	}
	newChildren := []*doctype.Document{
		{DocType: "Stock Move", Name: "SM-0001", Fields: map[string]any{"movement_type": "Opening Stock", "qty_change": float64(45), "reference": "Opening Stock"}},
		{DocType: "Stock Move", Fields: map[string]any{"movement_type": "Sale Issue", "qty_change": float64(-2), "reference": "SALE-0001"}},
	}

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO `tabProduct__stock_moves`")).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := reconcileChildren(dbConn, parentDT, "stock_moves", childDT, oldChildren, newChildren, "PROD-0001", db.Resolve("mysql")); err != nil {
		t.Fatalf("reconcileChildren: %v", err)
	}
	if newChildren[1].Name == "" {
		t.Fatalf("new child row name was not allocated")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
