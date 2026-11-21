//go:build integration

package orm_test

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/policy"
)

// A linked_field is copied from its source record on insert, when the link
// changes, and when the target is blank. A save that leaves the link alone keeps
// the stored value, so a later price change does not rewrite a sale already
// recorded. Run against disposable servers: KORA_ORM_DISPOSABLE_MYSQL_DSN and
// KORA_ORM_DISPOSABLE_POSTGRES_DSN.
func TestLinkedFieldKeepsStoredValueAcrossDialects(t *testing.T) {
	for _, c := range []struct{ driver, env string }{
		{"mysql", "KORA_ORM_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_ORM_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(c.driver, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(c.env))
			if dsn == "" {
				t.Skip("set " + c.env + " to a disposable server")
			}
			database := disposabledb.Open(t, c.driver, dsn)
			dialect := db.Resolve(c.driver)
			product, sale, item := linkedFieldDocTypes()
			registry := doctype.NewRegistry()
			registry.Register(product)
			registry.Register(sale)
			registry.Register(item)
			for _, ddl := range []string{
				linkedFieldTable(dialect, "tabProduct", false, "code", "selling_price"),
				linkedFieldTable(dialect, "tabSale", false, "status", "product", "unit_price"),
				linkedFieldTable(dialect, "tabSale__items", true, "product", "unit_price"),
			} {
				if _, err := database.DB.Exec(ddl); err != nil {
					t.Fatalf("create table: %v\n%s", err, ddl)
				}
			}
			s := &linkedFieldSite{t: t, database: database, dialect: dialect, sale: sale, product: product,
				manager: &orm.TxManager{DB: database.DB, Registry: registry, Dialect: dialect}}
			s.run()
		})
	}
}

// linkedFieldSite is one disposable database with the Product and Sale types.
type linkedFieldSite struct {
	t        *testing.T
	database *disposabledb.Database
	dialect  db.Dialect
	manager  *orm.TxManager
	product  *doctype.DocType
	sale     *doctype.DocType
}

func (s *linkedFieldSite) run() {
	t := s.t
	s.write(s.product, s.newProduct("P1", "100"), nil)
	s.write(s.product, s.newProduct("P2", "200"), nil)

	// 1. Insert copies the linked values into the child row and the parent.
	sale := doctype.NewDocument("Sale")
	sale.Name = "SALE-1"
	sale.Set("status", "Open")
	sale.Set("product", "P1")
	sale.SetTable("items", []*doctype.Document{linkedFieldItem("P1", "")})
	s.write(s.sale, sale, nil)
	s.expectSale("insert", "SALE-1", "100", "100")

	// 2. A price change and an unrelated save keep the stored values.
	s.setPrice("P1", "150")
	old := s.load("SALE-1")
	doc := s.load("SALE-1")
	doc.Set("status", "Settled")
	s.write(s.sale, doc, old)
	s.expectSale("unrelated save after price change", "SALE-1", "100", "100")

	// 3. Changing the link re-copies the new source value.
	old = s.load("SALE-1")
	doc = s.load("SALE-1")
	doc.Set("product", "P2")
	doc.GetTable("items")[0].Set("product", "P2")
	s.write(s.sale, doc, old)
	s.expectSale("link changed", "SALE-1", "200", "200")

	// 4. A blank linked value is filled from the current source value.
	s.setPrice("P2", "250")
	old = s.load("SALE-1")
	doc = s.load("SALE-1")
	doc.Set("unit_price", "")
	doc.GetTable("items")[0].Set("unit_price", "")
	s.write(s.sale, doc, old)
	s.expectSale("blank filled", "SALE-1", "250", "250")

	// 5. A child row added on save takes the source value; stored rows keep theirs.
	old = s.load("SALE-1")
	doc = s.load("SALE-1")
	doc.SetTable("items", append(doc.GetTable("items"), linkedFieldItem("P1", "")))
	s.write(s.sale, doc, old)
	reloaded := s.load("SALE-1")
	items := reloaded.GetTable("items")
	if len(items) != 2 {
		t.Fatalf("new child row: got %d rows, want 2", len(items))
	}
	for _, row := range items {
		want := map[string]string{"P1": "150", "P2": "250"}[row.GetString("product")]
		if got := s.stored("tabSale__items", row.Name, "unit_price"); got != want {
			t.Fatalf("child %s (%s): unit_price = %q, want %q", row.Name, row.GetString("product"), got, want)
		}
	}
}

func (s *linkedFieldSite) newProduct(code, price string) *doctype.Document {
	doc := doctype.NewDocument("Product")
	doc.Name = code
	doc.Set("code", code)
	doc.Set("selling_price", price)
	return doc
}

func linkedFieldItem(product, unitPrice string) *doctype.Document {
	row := doctype.NewDocument("Sale Item")
	row.Set("product", product)
	row.Set("unit_price", unitPrice)
	return row
}

// write derives the linked fields the way kernel validation does before a save,
// then inserts doc (old is nil) or saves it over old.
func (s *linkedFieldSite) write(dt *doctype.DocType, doc, old *doctype.Document) {
	s.t.Helper()
	if err := s.manager.DeriveFields(dt, doc, old); err != nil {
		s.t.Fatalf("derive %s %s: %v", dt.Name, doc.Name, err)
	}
	dbTx, err := s.database.DB.Begin()
	if err != nil {
		s.t.Fatal(err)
	}
	if old == nil {
		err = s.manager.InsertInTx(dbTx, dt, doc, "u1", "u1")
	} else {
		err = s.manager.SaveInTxWithScope(dbTx, dt, doc, "u1", policy.RowScope{}, old)
	}
	if err != nil {
		_ = dbTx.Rollback()
		s.t.Fatalf("write %s %s: %v", dt.Name, doc.Name, err)
	}
	if err := dbTx.Commit(); err != nil {
		s.t.Fatalf("commit %s: %v", doc.Name, err)
	}
}

func (s *linkedFieldSite) load(name string) *doctype.Document {
	s.t.Helper()
	doc, err := s.manager.GetDoc(s.sale, name, "")
	if err != nil {
		s.t.Fatalf("load %s: %v", name, err)
	}
	return doc
}

func (s *linkedFieldSite) setPrice(code, price string) {
	s.t.Helper()
	query := db.Rebind(s.dialect, "UPDATE "+s.dialect.QuoteIdent("tabProduct")+" SET selling_price = ? WHERE name = ?")
	if _, err := s.database.DB.Exec(query, price, code); err != nil {
		s.t.Fatalf("set price of %s: %v", code, err)
	}
}

// expectSale checks the parent's and its only child row's stored unit_price.
func (s *linkedFieldSite) expectSale(step, name, parentWant, childWant string) {
	s.t.Helper()
	if got := s.stored("tabSale", name, "unit_price"); got != parentWant {
		s.t.Fatalf("%s: parent unit_price = %q, want %q", step, got, parentWant)
	}
	items := s.load(name).GetTable("items")
	if len(items) != 1 {
		s.t.Fatalf("%s: got %d child rows, want 1", step, len(items))
	}
	if got := s.stored("tabSale__items", items[0].Name, "unit_price"); got != childWant {
		s.t.Fatalf("%s: child unit_price = %q, want %q", step, got, childWant)
	}
}

func (s *linkedFieldSite) stored(table, name, field string) string {
	s.t.Helper()
	var value sql.NullString
	query := db.Rebind(s.dialect, "SELECT "+field+" FROM "+s.dialect.QuoteIdent(table)+" WHERE name = ?")
	if err := s.database.DB.QueryRow(query, name).Scan(&value); err != nil {
		s.t.Fatalf("read %s.%s of %s: %v", table, field, name, err)
	}
	return value.String
}

func linkedFieldDocTypes() (product, sale, item *doctype.DocType) {
	product = &doctype.DocType{
		Name:       "Product",
		TitleField: "code",
		Fields: []doctype.Field{
			{Fieldname: "code", Fieldtype: "Data"},
			{Fieldname: "selling_price", Fieldtype: "Data"},
		},
	}
	item = &doctype.DocType{
		Name:         "Sale Item",
		IsChildTable: true,
		Fields: []doctype.Field{
			{Fieldname: "product", Fieldtype: "Link", Options: "Product"},
			{Fieldname: "unit_price", Fieldtype: "Data", LinkedField: "product.selling_price"},
		},
	}
	sale = &doctype.DocType{
		Name: "Sale",
		Fields: []doctype.Field{
			{Fieldname: "status", Fieldtype: "Data"},
			{Fieldname: "product", Fieldtype: "Link", Options: "Product"},
			{Fieldname: "unit_price", Fieldtype: "Data", LinkedField: "product.selling_price"},
			{Fieldname: "items", Fieldtype: "Table", Options: "Sale Item"},
		},
	}
	return product, sale, item
}

// linkedFieldTable creates a record table with the given text columns. Child
// tables carry parent columns and no revision, as the Engine's child tables do.
func linkedFieldTable(dialect db.Dialect, table string, child bool, columns ...string) string {
	defs := []string{"name VARCHAR(140) PRIMARY KEY", "owner VARCHAR(140)", "creation TIMESTAMP NULL",
		"modified TIMESTAMP NULL", "modified_by VARCHAR(140)", "doc_status INT", "idx INT"}
	if child {
		defs = append(defs, "parent VARCHAR(140)", "parentfield VARCHAR(140)", "parenttype VARCHAR(140)")
	} else {
		defs = append(defs, "revision INT NOT NULL DEFAULT 1")
	}
	for _, column := range columns {
		defs = append(defs, column+" VARCHAR(140)")
	}
	return "CREATE TABLE " + dialect.QuoteIdent(table) + " (" + strings.Join(defs, ", ") + ")"
}
