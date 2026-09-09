package orm

import (
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

func TestInsert_AppliesDefaultsBeforeComputedFields(t *testing.T) {
	dbConn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer dbConn.Close()

	reg := doctype.NewRegistry()
	saleItem := &doctype.DocType{
		Name:         "Sale Item",
		IsChildTable: true,
		Fields: []doctype.Field{
			{Fieldname: "product", Fieldtype: "Link", Options: "Product"},
			{Fieldname: "quantity", Fieldtype: "Float", Default: "1"},
			{Fieldname: "unit_price", Fieldtype: "Currency"},
			{Fieldname: "line_total", Fieldtype: "Currency", Computed: "quantity * unit_price", ReadOnly: true},
		},
	}
	sale := &doctype.DocType{
		Name: "Sale",
		Fields: []doctype.Field{
			{Fieldname: "discount_amount", Fieldtype: "Currency", Default: "0"},
			{Fieldname: "subtotal", Fieldtype: "Currency", Computed: "SUM(items.line_total)", ReadOnly: true},
			{Fieldname: "total_amount", Fieldtype: "Currency", Computed: "subtotal - discount_amount", ReadOnly: true},
			{Fieldname: "items", Fieldtype: "Table", Options: "Sale Item"},
		},
	}
	reg.Register(saleItem)
	reg.Register(sale)

	doc := doctype.NewDocument("Sale")
	doc.Name = "SALE-TEST"
	doc.SetTable("items", []*doctype.Document{{Fields: map[string]any{"product": "PROD-0001", "quantity": float64(2), "unit_price": float64(70)}}})

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO `tabSale`")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO `tabSale__items`")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `tabSale__items` SET line_total = ?, modified = ? WHERE name = ?")).
		WithArgs(float64(140), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `tabSale` SET subtotal = ?, total_amount = ? WHERE name = ?")).
		WithArgs(float64(140), float64(140), "SALE-TEST").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	tx := &TxManager{DB: dbConn, Registry: reg, Dialect: db.Resolve("mysql")}
	if err := tx.Insert(sale, doc, "tester", "tester"); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if got := doc.Get("discount_amount"); got != float64(0) {
		t.Fatalf("discount_amount default: got %#v, want 0", got)
	}
	if got := doc.Get("total_amount"); got != float64(140) {
		t.Fatalf("total_amount: got %#v, want 140", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPopulateLinkedFieldsResolvesDocumentTitle(t *testing.T) {
	dbConn, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer dbConn.Close()

	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{
		Name:       "Product",
		TitleField: "code",
		Fields:     []doctype.Field{{Fieldname: "code", Fieldtype: "Data"}, {Fieldname: "price", Fieldtype: "Currency"}},
	})
	order := &doctype.DocType{
		Name:   "Order",
		Fields: []doctype.Field{{Fieldname: "product", Fieldtype: "Link", Options: "Product"}, {Fieldname: "unit_price", Fieldtype: "Currency", LinkedField: "product.price"}},
	}
	reg.Register(order)

	mock.ExpectQuery(`SELECT price FROM .* WHERE name = \? OR code = \? LIMIT 1`).
		WithArgs("CAKE-001", "CAKE-001").
		WillReturnRows(sqlmock.NewRows([]string{"price"}).AddRow(125.5))

	doc := doctype.NewDocument("Order")
	doc.Set("product", "CAKE-001")
	tx := &TxManager{DB: dbConn, Registry: reg, Dialect: db.Resolve("mysql")}
	if err := tx.populateLinkedFields(dbConn, order, doc); err != nil {
		t.Fatalf("populate linked fields: %v", err)
	}
	if got := doc.Get("unit_price"); got != 125.5 {
		t.Fatalf("linked value: got %#v, want 125.5", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
