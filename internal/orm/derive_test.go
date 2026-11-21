package orm

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

// A linked value is looked up for a new row, a blank stored target or a changed
// link. An unchanged link on a stored row takes the stored value, whatever the
// client sent.
func TestPopulateLinkedFieldsCopiesOnlyWhenNeeded(t *testing.T) {
	order := &doctype.DocType{
		Name: "Order",
		Fields: []doctype.Field{
			{Fieldname: "product", Fieldtype: "Link", Options: "Product"},
			{Fieldname: "unit_price", Fieldtype: "Currency", LinkedField: "product.price"},
		},
	}
	product := &doctype.DocType{
		Name:       "Product",
		TitleField: "code",
		Fields:     []doctype.Field{{Fieldname: "code", Fieldtype: "Data"}, {Fieldname: "price", Fieldtype: "Currency"}},
	}
	cases := []struct {
		name       string
		stored     map[string]any // nil: a new row
		doc        map[string]any
		wantLookup bool
		want       any
	}{
		{"new row copies", nil, map[string]any{"product": "CAKE-001"}, true, 125.5},
		{"new row copies over a typed value, as before", nil, map[string]any{"product": "CAKE-001", "unit_price": 9.0}, true, 125.5},
		{"unchanged link restores the stored value over a client value", map[string]any{"product": "CAKE-001", "unit_price": 99.0}, map[string]any{"product": "CAKE-001", "unit_price": 9.0}, false, 99.0},
		{"unchanged link restores the stored value over a blank", map[string]any{"product": "CAKE-001", "unit_price": 99.0}, map[string]any{"product": "CAKE-001", "unit_price": ""}, false, 99.0},
		{"changed link copies", map[string]any{"product": "OTHER", "unit_price": 99.0}, map[string]any{"product": "CAKE-001", "unit_price": 99.0}, true, 125.5},
		{"blank stored value on unchanged link copies", map[string]any{"product": "CAKE-001", "unit_price": ""}, map[string]any{"product": "CAKE-001", "unit_price": ""}, true, 125.5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dbConn, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer dbConn.Close()
			reg := doctype.NewRegistry()
			reg.Register(product)
			reg.Register(order)
			if c.wantLookup {
				mock.ExpectQuery(`SELECT price FROM .* WHERE name = \? OR code = \? LIMIT 1`).
					WithArgs("CAKE-001", "CAKE-001").
					WillReturnRows(sqlmock.NewRows([]string{"price"}).AddRow(125.5))
			}
			doc := doctype.NewDocument("Order")
			for k, v := range c.doc {
				doc.Set(k, v)
			}
			var stored *doctype.Document
			if c.stored != nil {
				stored = doctype.NewDocument("Order")
				for k, v := range c.stored {
					stored.Set(k, v)
				}
			}
			tx := &TxManager{DB: dbConn, Registry: reg, Dialect: db.Resolve("mysql")}
			if err := tx.populateLinkedFields(dbConn, order, doc, stored); err != nil {
				t.Fatalf("populate linked fields: %v", err)
			}
			if got := doc.Get("unit_price"); got != c.want {
				t.Fatalf("unit_price: got %#v, want %#v", got, c.want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("lookups: %v", err)
			}
		})
	}
}
