//go:build integration

package api

import (
	"context"
	"database/sql"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/configstore"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/schema"
	"github.com/asenawritescode/kora/internal/script"
	ksite "github.com/asenawritescode/kora/internal/site"
)

// The parked purchasing pack (and the selling pack it needs) loaded as shipped, with its scripts
// run the way the Engine runs them. A supplier payment allocates to supplier bills; a bill's paid
// amount and status follow its payments.
const (
	erpPurchasingPack = "../../config/_parked/erp-purchasing"
	erpSellingPack    = "../../config/_parked/erpsell"
)

// erpPaymentsFixture is one disposable database with both packs and their scripts installed.
type erpPaymentsFixture struct {
	t       *testing.T
	handler *Handler
	db      *sql.DB
	reg     *doctype.Registry
	site    string
	dialect string
}

func TestERPPurchasingPaymentsFollowBillsAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ name, dialect, env string }{
		{"mysql", "mysql", "KORA_ERP_PURCHASING_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_ERP_PURCHASING_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			f := openERPPaymentsFixture(t, tc.dialect, dsn)
			f.runScenario()
		})
	}
}

func openERPPaymentsFixture(t *testing.T, dialect, dsn string) *erpPaymentsFixture {
	t.Helper()
	ctx := context.Background()
	database := disposabledb.Open(t, dialect, dsn).DB
	database.SetMaxOpenConns(4)
	dl := kdb.Resolve(dialect)
	if err := ksite.BootstrapSystemTables(ctx, database, dl); err != nil {
		t.Fatalf("bootstrap system tables: %v", err)
	}
	for _, ddl := range extensibilityScriptTables(dialect) {
		if _, err := database.Exec(ddl); err != nil {
			t.Fatalf("create script tables: %v", err)
		}
	}
	for _, ddl := range configstore.ConfigRevisionSchema(dl) {
		if _, err := database.Exec(ddl); err != nil {
			t.Fatalf("create config revision table: %v", err)
		}
	}
	if err := configstore.SeedConfigRevision(ctx, database, dl); err != nil {
		t.Fatalf("seed config revision: %v", err)
	}

	var docTypes []*doctype.DocType
	for _, pack := range []string{erpSellingPack, erpPurchasingPack} {
		parsed, err := doctype.ParseConfigTree(pack)
		if err != nil {
			t.Fatalf("parse %s: %v", pack, err)
		}
		docTypes = append(docTypes, parsed...)
	}
	permissions := make([]*doctype.Permission, 0, len(docTypes))
	for _, recordType := range docTypes {
		permissions = append(permissions, &doctype.Permission{
			Doctype: recordType.Name, Role: doctype.AdminRole, Read: true, Write: true, Create: true, Delete: true, Submit: true,
		})
	}
	site := "erp-payments-" + dialect
	reg := doctype.NewRegistry()
	reg.LoadFull(docTypes, []*doctype.Role{{Name: doctype.AdminRole}}, permissions)
	if err := schema.MigrateSiteFromRegistry(database, site, reg, dl); err != nil {
		t.Fatalf("migrate purchasing schema: %v", err)
	}
	writer := outbox.NewSQLWriter(dl)
	store := &script.Store{DB: database, Dialect: dl}
	handler := NewHandler(reg, &orm.TxManager{DB: database, Registry: reg, Dialect: dl, Outbox: writer, SiteName: site})
	handler.SiteOutboxes = map[string]outbox.Writer{site: writer}
	handler.SiteScriptStores = map[string]*script.Store{site: store}
	handler.ScriptRunner = script.NewEmbeddedRunner(script.DefaultEmbeddedConfig())
	installPackScripts(t, store, site, filepath.Join(erpPurchasingPack, "scripts"))

	for _, currency := range []string{"KES", "USD"} {
		if _, err := database.Exec(kdb.Rebind(dl, "INSERT INTO "+dl.QuoteIdent("tabCurrency")+" (name, code, currency_name, rate_to_base, enabled) VALUES (?, ?, ?, 1, 1)"), currency, currency, currency); err != nil {
			t.Fatalf("seed currency %s: %v", currency, err)
		}
	}
	return &erpPaymentsFixture{t: t, handler: handler, db: database, reg: reg, site: site, dialect: dialect}
}

// extensibilityScriptTables returns the DDL for the script store tables only.
func extensibilityScriptTables(dialect string) []string {
	var all []string
	if dialect == "postgres" {
		all = kdb.ExtensibilityTablesPostgres()
	} else {
		all = kdb.ExtensibilityTablesMySQL()
	}
	var out []string
	for _, ddl := range all {
		if strings.Contains(ddl, "_kora_script") && !strings.Contains(ddl, "_kora_extension") {
			out = append(out, ddl)
		}
	}
	return out
}

// installPackScripts stores every scripts/<DocType>/<event>.js of the pack as an active
// document-event script, the rows the Engine writes when a pack is activated.
func installPackScripts(t *testing.T, store *script.Store, site, root string) {
	t.Helper()
	count := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Ext(path) != ".js" {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		doctypeName := filepath.Dir(rel)
		event := strings.TrimSuffix(filepath.Base(rel), ".js")
		count++
		return store.Insert(script.Record{
			Name: doctypeName + " " + event, Site: site, ScriptType: script.TypeDocEvent,
			DocType: doctypeName, Event: script.Event(event), IsActive: true,
			Script: string(body), TimeoutMs: 5000, CreatedBy: "pack", UpdatedBy: "pack",
		})
	})
	if err != nil {
		t.Fatalf("install pack scripts: %v", err)
	}
	if count == 0 {
		t.Fatalf("pack %s has no scripts to install", root)
	}
}

// send returns the status and the saved document, nil when the call was refused.
func (f *erpPaymentsFixture) send(method resourceHTTPMethod, recordType, name, body string) (int, map[string]any) {
	f.t.Helper()
	path := "/api/resource/" + url.PathEscape(recordType)
	if name != "" {
		path += "/" + url.PathEscape(name)
	}
	recorder := serveResourceMutationBodyWithDBType(f.t, f.handler, f.db, f.reg, f.site, method, path, body, "", []string{doctype.AdminRole}, f.dialect)
	if recorder.Code != http.StatusOK && recorder.Code != http.StatusCreated {
		return recorder.Code, nil
	}
	return recorder.Code, requireRESTDocument(f.t, recorder.Body.Bytes(), recordType)
}

func (f *erpPaymentsFixture) create(recordType, body string) string {
	f.t.Helper()
	code, doc := f.send(httpMethodPost, recordType, "", body)
	if doc == nil {
		f.t.Fatalf("create %s = %d: %s", recordType, code, body)
	}
	name, _ := doc["name"].(string)
	return name
}

// refused reports whether saving the record is refused.
func (f *erpPaymentsFixture) refused(method resourceHTTPMethod, recordType, name, body string) bool {
	f.t.Helper()
	_, doc := f.send(method, recordType, name, body)
	return doc == nil
}

// value reads one column of one record as text.
func (f *erpPaymentsFixture) value(recordType, name, column string) string {
	f.t.Helper()
	dl := kdb.Resolve(f.dialect)
	query := "SELECT " + dl.QuoteIdent(column) + " FROM " + dl.QuoteIdent("tab"+recordType) + " WHERE name = ?"
	var out sql.NullString
	if err := f.db.QueryRow(kdb.Rebind(dl, query), name).Scan(&out); err != nil {
		f.t.Fatalf("%s %s.%s: %v", recordType, name, column, err)
	}
	return out.String
}

// amount reads a money column as a number.
func (f *erpPaymentsFixture) amount(recordType, name, column string) float64 {
	f.t.Helper()
	got, err := strconv.ParseFloat(f.value(recordType, name, column), 64)
	if err != nil {
		f.t.Fatalf("%s %s.%s is not a number: %v", recordType, name, column, err)
	}
	return got
}

// allocationRows counts the allocation rows a payment holds.
func (f *erpPaymentsFixture) allocationRows(payment string) int {
	f.t.Helper()
	dl := kdb.Resolve(f.dialect)
	query := "SELECT COUNT(*) FROM " + dl.QuoteIdent("tabSupplier Payment__allocations") + " WHERE parent = ?"
	var rows int
	if err := f.db.QueryRow(kdb.Rebind(dl, query), payment).Scan(&rows); err != nil {
		f.t.Fatalf("count allocations of %s: %v", payment, err)
	}
	return rows
}

// expectBill checks the three values a person sees on a bill.
func (f *erpPaymentsFixture) expectBill(bill, status string, paid, outstanding float64) {
	f.t.Helper()
	if got := f.value("Supplier Bill", bill, "status"); got != status {
		f.t.Fatalf("bill %s status = %q, want %q", bill, got, status)
	}
	if got := f.amount("Supplier Bill", bill, "amount_paid"); got != paid {
		f.t.Fatalf("bill %s amount paid = %v, want %v", bill, got, paid)
	}
	if got := f.amount("Supplier Bill", bill, "outstanding"); got != outstanding {
		f.t.Fatalf("bill %s outstanding = %v, want %v", bill, got, outstanding)
	}
}

func (f *erpPaymentsFixture) runScenario() {
	t := f.t
	supplier := f.create("Supplier", `{"supplier_name":"Dar Traders"}`)
	otherSupplier := f.create("Supplier", `{"supplier_name":"Mombasa Paper"}`)
	bill := func(reference, who, amount, currency string) string {
		body := `{"reference":"` + reference + `","supplier":"` + who + `","bill_date":"2026-11-13","amount":` + amount + `,"status":"Open"`
		if currency != "" {
			body += `,"currency":"` + currency + `"`
		}
		return f.create("Supplier Bill", body+`}`)
	}
	payment := func(reference, amount string, allocations, shortcut string) string {
		body := `{"reference":"` + reference + `","payment_date":"2026-11-14","supplier":"` + supplier + `","amount":` + amount
		if allocations != "" {
			body += `,"allocations":[` + allocations + `]`
		}
		if shortcut != "" {
			body += `,"supplier_bill":"` + shortcut + `"`
		}
		return f.create("Supplier Payment", body+`}`)
	}
	alloc := func(bill string, amount string) string {
		return `{"supplier_bill":"` + bill + `","amount":` + amount + `}`
	}

	var b1, p1, p2 string
	t.Run("new bill is open with nothing paid", func(t *testing.T) {
		b1 = bill("BILL-1", supplier, "1000", "")
		f.expectBill(b1, "Open", 0, 1000)
	})
	t.Run("partial payment leaves the bill partly paid", func(t *testing.T) {
		p1 = payment("PAY-1", "300", alloc(b1, "300"), "")
		if f.refused(httpMethodPut, "Supplier Payment", p1, `{"status":"Paid"}`) {
			t.Fatal("paying an open bill was refused")
		}
		f.expectBill(b1, "Partly Paid", 300, 700)
		if got := f.amount("Supplier Payment", p1, "unallocated_amount"); got != 0 {
			t.Fatalf("unallocated amount = %v, want 0", got)
		}
	})
	t.Run("payment for the rest makes the bill paid", func(t *testing.T) {
		p2 = payment("PAY-2", "700", alloc(b1, "700"), "")
		if f.refused(httpMethodPut, "Supplier Payment", p2, `{"status":"Paid"}`) {
			t.Fatal("paying the rest was refused")
		}
		f.expectBill(b1, "Paid", 1000, 0)
	})
	t.Run("voiding a payment reverses exactly its allocation", func(t *testing.T) {
		if f.refused(httpMethodPut, "Supplier Payment", p2, `{"status":"Voided"}`) {
			t.Fatal("voiding a paid payment was refused")
		}
		f.expectBill(b1, "Partly Paid", 300, 700)
		if got := f.amount("Supplier Payment", p2, "unallocated_amount"); got != 0 {
			t.Fatalf("voided payment unallocated amount = %v, want 0", got)
		}
	})
	t.Run("money beyond the listed bills stays as supplier credit", func(t *testing.T) {
		b2 := bill("BILL-2", supplier, "200", "")
		p3 := payment("PAY-3", "500", alloc(b2, "200"), "")
		if f.refused(httpMethodPut, "Supplier Payment", p3, `{"status":"Paid"}`) {
			t.Fatal("payment with credit was refused")
		}
		f.expectBill(b2, "Paid", 200, 0)
		if got := f.amount("Supplier Payment", p3, "unallocated_amount"); got != 300 {
			t.Fatalf("supplier credit = %v, want 300", got)
		}
	})
	t.Run("supplier bill shortcut pays up to outstanding and records a row", func(t *testing.T) {
		b3 := bill("BILL-3", supplier, "1000", "")
		p4 := payment("PAY-4", "1500", "", b3)
		if f.refused(httpMethodPut, "Supplier Payment", p4, `{"status":"Paid"}`) {
			t.Fatal("shortcut payment was refused")
		}
		f.expectBill(b3, "Paid", 1000, 0)
		if rows := f.allocationRows(p4); rows != 1 {
			t.Fatalf("shortcut recorded %d allocation rows, want 1", rows)
		}
		if got := f.amount("Supplier Payment", p4, "unallocated_amount"); got != 500 {
			t.Fatalf("shortcut supplier credit = %v, want 500", got)
		}
	})
	t.Run("allocating to another supplier's bill is refused", func(t *testing.T) {
		b4 := bill("BILL-4", otherSupplier, "100", "")
		p5 := payment("PAY-5", "50", alloc(b4, "50"), "")
		if !f.refused(httpMethodPut, "Supplier Payment", p5, `{"status":"Paid"}`) {
			t.Fatal("payment to the wrong supplier's bill was accepted")
		}
		f.expectBill(b4, "Open", 0, 100)
	})
	t.Run("allocating in another currency is refused", func(t *testing.T) {
		b5 := bill("BILL-5", supplier, "100", "USD")
		p6 := payment("PAY-6", "100", alloc(b5, "100"), "")
		if !f.refused(httpMethodPut, "Supplier Payment", p6, `{"status":"Paid"}`) {
			t.Fatal("payment in another currency was accepted")
		}
		f.expectBill(b5, "Open", 0, 100)
	})
	t.Run("over-allocation is refused", func(t *testing.T) {
		b6 := bill("BILL-6", supplier, "200", "")
		over := []struct{ amount, allocations string }{
			{"300", alloc(b6, "250")},                          // more than the bill's outstanding
			{"300", alloc(b6, "150") + "," + alloc(b6, "100")}, // two rows on one bill beyond outstanding
			{"100", alloc(b6, "150")},                          // more than the payment
		}
		for i, tc := range over {
			p := payment("PAY-OVER-"+strconv.Itoa(i+1), tc.amount, tc.allocations, "")
			if !f.refused(httpMethodPut, "Supplier Payment", p, `{"status":"Paid"}`) {
				t.Fatalf("over-allocation %s accepted", tc.allocations)
			}
		}
		f.expectBill(b6, "Open", 0, 200)
	})
	t.Run("a person cannot mark an unpaid bill paid", func(t *testing.T) {
		b7 := bill("BILL-7", supplier, "100", "")
		if !f.refused(httpMethodPut, "Supplier Bill", b7, `{"status":"Paid"}`) {
			t.Fatal("unpaid bill was set Paid by hand")
		}
		f.expectBill(b7, "Open", 0, 100)
	})
	t.Run("a direct edit of amount paid is ignored", func(t *testing.T) {
		b8 := bill("BILL-8", supplier, "100", "")
		_ = f.refused(httpMethodPut, "Supplier Bill", b8, `{"amount_paid":50}`)
		f.expectBill(b8, "Open", 0, 100)
	})
	t.Run("a bill with payments cannot be cancelled or reopened by hand", func(t *testing.T) {
		if !f.refused(httpMethodPut, "Supplier Bill", b1, `{"status":"Cancelled"}`) {
			t.Fatal("bill with payments was cancelled")
		}
		if !f.refused(httpMethodPut, "Supplier Bill", b1, `{"status":"Open"}`) {
			t.Fatal("bill with payments was reopened by hand")
		}
		f.expectBill(b1, "Partly Paid", 300, 700)
	})
	t.Run("a payment amount is fixed once paid", func(t *testing.T) {
		if !f.refused(httpMethodPut, "Supplier Payment", p1, `{"amount":999}`) {
			t.Fatal("a paid payment's amount was changed")
		}
	})
	t.Run("voiding back to zero reopens the bill as open", func(t *testing.T) {
		b9 := bill("BILL-9", supplier, "300", "")
		p9 := payment("PAY-9", "300", alloc(b9, "300"), "")
		if f.refused(httpMethodPut, "Supplier Payment", p9, `{"status":"Paid"}`) {
			t.Fatal("payment was refused")
		}
		f.expectBill(b9, "Paid", 300, 0)
		if f.refused(httpMethodPut, "Supplier Payment", p9, `{"status":"Voided"}`) {
			t.Fatal("voiding was refused")
		}
		f.expectBill(b9, "Open", 0, 300)
	})
	t.Run("an unpaid bill can be cancelled", func(t *testing.T) {
		b10 := bill("BILL-10", supplier, "100", "")
		if f.refused(httpMethodPut, "Supplier Bill", b10, `{"status":"Cancelled"}`) {
			t.Fatal("cancelling an unpaid bill was refused")
		}
		if got := f.value("Supplier Bill", b10, "status"); got != "Cancelled" {
			t.Fatalf("status = %q, want Cancelled", got)
		}
	})
}
