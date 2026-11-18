//go:build integration

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/schema"
	ksite "github.com/asenawritescode/kora/internal/site"
)

// quantity_effect is an Engine feature (doctype.QuantityEffect), so these tests
// declare their own record types instead of loading a pack: a target record
// type with a stock quantity, a record that changes it by one signed amount,
// and a record with lines that change it per line. They run on MySQL and
// Postgres through the REST create handler: stock must change as part of the
// save, from any surface, and never without it.

const effectSite = "quantity-effect.example.test"

var (
	effectAdmin = []string{doctype.AdminRole}
	effectClerk = []string{"Clerk"}
)

var quantityEffectDialects = []struct{ driver, env string }{
	{"mysql", "KORA_QUANTITY_EFFECT_DISPOSABLE_MYSQL_DSN"},
	{"postgres", "KORA_QUANTITY_EFFECT_DISPOSABLE_POSTGRES_DSN"},
}

type effectHarness struct {
	h       *Handler
	db      *sql.DB
	reg     *doctype.Registry
	driver  string
	dialect kdb.Dialect
}

func effectDoctypes() []*doctype.DocType {
	return []*doctype.DocType{
		{Name: "Stock Item", Fields: []doctype.Field{
			{Fieldname: "product_name", Fieldtype: "Data", Reqd: true},
			{Fieldname: "stock_qty", Fieldtype: "Float", Default: "0", ReadOnly: true},
		}},
		{Name: "Stock Adjustment", AppendOnly: true, QuantityEffect: &doctype.QuantityEffect{
			TargetDocType: "Stock Item", TargetQuantityField: "stock_qty", SourceRecordField: "product",
			DeltaField: "delta", ReasonField: "reason",
			ReasonDirections: map[string]string{"Opening balance": "increase", "Found": "increase", "Damaged": "decrease", "Lost or stolen": "decrease"},
		}, Fields: []doctype.Field{
			{Fieldname: "reference", Fieldtype: "Data", Reqd: true, Unique: true},
			{Fieldname: "product", Fieldtype: "Link", Options: "Stock Item", Reqd: true},
			{Fieldname: "delta", Fieldtype: "Float", Reqd: true},
			{Fieldname: "reason", Fieldtype: "Select", Options: "Opening balance\nCount correction\nDamaged\nLost or stolen\nFound\nOther", Reqd: true},
		}},
		{Name: "Goods Receipt Item", IsChildTable: true, Fields: []doctype.Field{
			{Fieldname: "product", Fieldtype: "Link", Options: "Stock Item", Reqd: true},
			{Fieldname: "quantity", Fieldtype: "Float", Reqd: true},
			{Fieldname: "unit_cost", Fieldtype: "Currency"},
		}},
		{Name: "Goods Receipt", AppendOnly: true, QuantityEffect: &doctype.QuantityEffect{
			TargetDocType: "Stock Item", TargetQuantityField: "stock_qty", LinesField: "items",
			SourceRecordField: "product", DeltaField: "quantity", Direction: "increase",
		}, Fields: []doctype.Field{
			{Fieldname: "reference", Fieldtype: "Data", Reqd: true, Unique: true},
			{Fieldname: "supplier", Fieldtype: "Data", Reqd: true},
			{Fieldname: "items", Fieldtype: "Table", Options: "Goods Receipt Item", Reqd: true},
		}},
	}
}

func effectPermissions() []*doctype.Permission {
	return []*doctype.Permission{
		{Doctype: "Stock Item", Role: doctype.AdminRole, Read: true, Write: true, Create: true, Delete: true},
		{Doctype: "Stock Adjustment", Role: doctype.AdminRole, Read: true, Create: true, Delete: true},
		{Doctype: "Goods Receipt", Role: doctype.AdminRole, Read: true, Create: true, Delete: true},
		{Doctype: "Goods Receipt Item", Role: doctype.AdminRole, Read: true, Create: true, Delete: true},
		// A clerk may record adjustments and receipts and read stock items,
		// but may not edit a stock item directly.
		{Doctype: "Stock Item", Role: "Clerk", Read: true},
		{Doctype: "Stock Adjustment", Role: "Clerk", Read: true, Create: true},
		{Doctype: "Goods Receipt", Role: "Clerk", Read: true, Create: true},
		{Doctype: "Goods Receipt Item", Role: "Clerk", Read: true, Create: true},
	}
}

// forEachQuantityEffectDialect runs one check on a fresh database per dialect.
func forEachQuantityEffectDialect(t *testing.T, check func(t *testing.T, e *effectHarness)) {
	t.Helper()
	for _, d := range quantityEffectDialects {
		t.Run(d.driver, func(t *testing.T) {
			check(t, newEffectHarness(t, d.driver, d.env))
		})
	}
}

func newEffectHarness(t *testing.T, driver, env string) *effectHarness {
	t.Helper()
	dsn := os.Getenv(env)
	if dsn == "" {
		t.Skipf("set %s to a disposable %s database", env, driver)
	}
	database, dialect := openKernelDialectDatabase(t, driver, dsn)
	if err := ksite.BootstrapSystemTables(context.Background(), database, dialect); err != nil {
		t.Fatalf("bootstrap %s tables: %v", driver, err)
	}
	reg := doctype.NewRegistry()
	reg.LoadFull(effectDoctypes(), []*doctype.Role{{Name: doctype.AdminRole}, {Name: "Clerk", WorkspaceAccess: true}}, effectPermissions())
	if err := schema.MigrateSiteFromRegistry(database, effectSite, reg, dialect); err != nil {
		t.Fatalf("migrate %s quantity effect schema: %v", driver, err)
	}
	writer := outbox.NewSQLWriter(dialect)
	handler := NewHandler(reg, &orm.TxManager{DB: database, Registry: reg, Dialect: dialect, Outbox: writer, SiteName: effectSite})
	handler.SiteOutboxes = map[string]outbox.Writer{effectSite: writer}
	e := &effectHarness{h: handler, db: database, reg: reg, driver: driver, dialect: dialect}
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	for _, item := range []struct {
		name, product string
		stock         float64
	}{{"SODA", "Soda", 10}, {"WATER", "Water", 0}} {
		statement := kdb.Rebind(dialect, "INSERT INTO "+e.table("Stock Item")+" (name, owner, creation, modified, modified_by, doc_status, idx, product_name, stock_qty) VALUES (?,?,?,?,?,?,?,?,?)")
		if _, err := database.Exec(statement, item.name, "Administrator", now, now, "Administrator", 0, 0, item.product, item.stock); err != nil {
			t.Fatalf("seed %s: %v", item.name, err)
		}
	}
	return e
}

func (e *effectHarness) table(doctypeName string) string {
	return e.dialect.QuoteIdent(e.reg.Get(doctypeName).RawTableName())
}

func (e *effectHarness) post(t *testing.T, recordType string, body any, key string, roles []string) (int, string) {
	t.Helper()
	encoded, _ := json.Marshal(body)
	rec := serveResourceMutationBodyWithDBType(t, e.h, e.db, e.reg, effectSite, httpMethodPost, "/api/resource/"+recordType, string(encoded), key, roles, e.driver)
	return rec.Code, rec.Body.String()
}

func (e *effectHarness) put(t *testing.T, path, body string, roles []string) *httptest.ResponseRecorder {
	t.Helper()
	return serveResourceMutationBodyWithDBType(t, e.h, e.db, e.reg, effectSite, httpMethodPut, path, body, "", roles, e.driver)
}

func (e *effectHarness) stockOf(t *testing.T, product string) float64 {
	t.Helper()
	var qty float64
	if err := e.db.QueryRow(kdb.Rebind(e.dialect, "SELECT stock_qty FROM "+e.table("Stock Item")+" WHERE name = ?"), product).Scan(&qty); err != nil {
		t.Fatal(err)
	}
	return qty
}

func (e *effectHarness) countOf(t *testing.T, doctypeName string) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow("SELECT COUNT(*) FROM " + e.table(doctypeName)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *effectHarness) setStock(t *testing.T, product string, qty float64) {
	t.Helper()
	if _, err := e.db.Exec(kdb.Rebind(e.dialect, "UPDATE "+e.table("Stock Item")+" SET stock_qty = ? WHERE name = ?"), qty, product); err != nil {
		t.Fatal(err)
	}
}

func TestStockAdjustmentChangesStockWhenSavedThroughTheNormalCreate(t *testing.T) {
	forEachQuantityEffectDialect(t, func(t *testing.T, e *effectHarness) {
		code, body := e.post(t, "Stock%20Adjustment", map[string]any{"reference": "ADJ-1", "product": "SODA", "delta": 5, "reason": "Found"}, "", effectAdmin)
		if code != http.StatusCreated {
			t.Fatalf("status = %d: %s", code, body)
		}
		if got := e.stockOf(t, "SODA"); got != 15 {
			t.Fatalf("stock = %v, want 15", got)
		}
		var doc struct{ Data map[string]any }
		if err := json.Unmarshal([]byte(body), &doc); err != nil || doc.Data["reference"] != "ADJ-1" {
			t.Fatalf("the response must still be the created adjustment: %s", body)
		}
	})
}

func TestStockAdjustmentCanTakeStockOut(t *testing.T) {
	forEachQuantityEffectDialect(t, func(t *testing.T, e *effectHarness) {
		if code, body := e.post(t, "Stock%20Adjustment", map[string]any{"reference": "ADJ-2", "product": "SODA", "delta": -4, "reason": "Damaged"}, "", effectAdmin); code != http.StatusCreated {
			t.Fatalf("status = %d: %s", code, body)
		}
		if got := e.stockOf(t, "SODA"); got != 6 {
			t.Fatalf("stock = %v, want 6", got)
		}
	})
}

// The reason chosen decides the direction: "Damaged" with a plain 4 removes 4,
// and the adjustment keeps the signed number so its history reads true.
func TestAdjustmentReasonDecidesWhetherStockGoesUpOrDown(t *testing.T) {
	forEachQuantityEffectDialect(t, func(t *testing.T, e *effectHarness) {
		for i, tc := range []struct {
			reason string
			typed  int
			stock  float64
			stored float64
		}{
			{"Damaged", 4, 6, -4},
			{"Lost or stolen", 1, 9, -1},
			{"Found", 7, 17, 7},
			{"Count correction", -2, 8, -2},
		} {
			ref := "ADJ-R" + string(rune('A'+i))
			if code, body := e.post(t, "Stock%20Adjustment", map[string]any{"reference": ref, "product": "SODA", "delta": tc.typed, "reason": tc.reason}, "", effectAdmin); code != http.StatusCreated {
				t.Fatalf("%s: status = %d: %s", tc.reason, code, body)
			}
			if got := e.stockOf(t, "SODA"); got != tc.stock {
				t.Fatalf("after %s %d: stock = %v, want %v", tc.reason, tc.typed, got, tc.stock)
			}
			var stored float64
			if err := e.db.QueryRow(kdb.Rebind(e.dialect, "SELECT delta FROM "+e.table("Stock Adjustment")+" WHERE reference = ?"), ref).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if stored != tc.stored {
				t.Fatalf("%s: stored change = %v, want %v", tc.reason, stored, tc.stored)
			}
			e.setStock(t, "SODA", 10)
		}
	})
}

func TestDamagedMoreThanInStockIsRefused(t *testing.T) {
	forEachQuantityEffectDialect(t, func(t *testing.T, e *effectHarness) {
		if code, _ := e.post(t, "Stock%20Adjustment", map[string]any{"reference": "ADJ-X", "product": "SODA", "delta": 11, "reason": "Damaged"}, "", effectAdmin); code == http.StatusCreated {
			t.Fatal("removing more than is in stock must be refused")
		}
		if got := e.stockOf(t, "SODA"); got != 10 {
			t.Fatalf("stock = %v, want 10", got)
		}
		if n := e.countOf(t, "Stock Adjustment"); n != 0 {
			t.Fatalf("a refused adjustment saved %d rows", n)
		}
	})
}

func TestAdjustmentBelowZeroIsRefusedAndNothingIsSaved(t *testing.T) {
	forEachQuantityEffectDialect(t, func(t *testing.T, e *effectHarness) {
		code, body := e.post(t, "Stock%20Adjustment", map[string]any{"reference": "ADJ-3", "product": "SODA", "delta": -11, "reason": "Lost or stolen"}, "", effectAdmin)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", code, body)
		}
		if got := e.stockOf(t, "SODA"); got != 10 {
			t.Fatalf("stock = %v, want it unchanged at 10", got)
		}
		if n := e.countOf(t, "Stock Adjustment"); n != 0 {
			t.Fatalf("a refused adjustment left %d record(s) behind", n)
		}
	})
}

func TestRetryWithTheSameKeyDoesNotChangeStockTwice(t *testing.T) {
	forEachQuantityEffectDialect(t, func(t *testing.T, e *effectHarness) {
		body := map[string]any{"reference": "ADJ-4", "product": "SODA", "delta": 5, "reason": "Found"}
		for i := 0; i < 2; i++ {
			if code, out := e.post(t, "Stock%20Adjustment", body, "adj-key-4", effectAdmin); code != http.StatusCreated {
				t.Fatalf("attempt %d status = %d: %s", i+1, code, out)
			}
		}
		if got := e.stockOf(t, "SODA"); got != 15 {
			t.Fatalf("stock = %v after a retry, want 15 (added once)", got)
		}
		if n := e.countOf(t, "Stock Adjustment"); n != 1 {
			t.Fatalf("adjustments = %d, want 1", n)
		}
	})
}

func TestSomeoneWhoMayOnlyCreateTheRecordCanStillRaiseStockButNotEditTheProduct(t *testing.T) {
	forEachQuantityEffectDialect(t, func(t *testing.T, e *effectHarness) {
		if code, body := e.post(t, "Stock%20Adjustment", map[string]any{"reference": "ADJ-5", "product": "SODA", "delta": 2, "reason": "Found"}, "", effectClerk); code != http.StatusCreated {
			t.Fatalf("a clerk who may create adjustments was refused: %d %s", code, body)
		}
		if got := e.stockOf(t, "SODA"); got != 12 {
			t.Fatalf("stock = %v, want 12", got)
		}
		rec := e.put(t, "/api/resource/Stock%20Item/SODA", `{"product_name":"Renamed"}`, effectClerk)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("a clerk edited a stock item directly: %d %s", rec.Code, rec.Body.String())
		}
	})
}

func TestSomeoneWithoutCreatePermissionOnTheRecordCannotChangeStock(t *testing.T) {
	forEachQuantityEffectDialect(t, func(t *testing.T, e *effectHarness) {
		code, body := e.post(t, "Stock%20Adjustment", map[string]any{"reference": "ADJ-6", "product": "SODA", "delta": 2, "reason": "Found"}, "", []string{"Nobody"})
		if code == http.StatusCreated {
			t.Fatalf("an unauthorized create changed stock: %s", body)
		}
		if got := e.stockOf(t, "SODA"); got != 10 {
			t.Fatalf("stock = %v, want 10", got)
		}
	})
}

func TestGoodsReceiptRaisesStockForEveryLineInOneSave(t *testing.T) {
	forEachQuantityEffectDialect(t, func(t *testing.T, e *effectHarness) {
		receipt := map[string]any{"reference": "GR-1", "supplier": "Dar Traders", "items": []map[string]any{
			{"product": "SODA", "quantity": 24, "unit_cost": 70}, {"product": "WATER", "quantity": 12, "unit_cost": 30}, {"product": "SODA", "quantity": 6, "unit_cost": 70},
		}}
		if code, body := e.post(t, "Goods%20Receipt", receipt, "", effectClerk); code != http.StatusCreated {
			t.Fatalf("status = %d: %s", code, body)
		}
		if soda, water := e.stockOf(t, "SODA"), e.stockOf(t, "WATER"); soda != 40 || water != 12 {
			t.Fatalf("stock soda=%v water=%v, want 40 and 12", soda, water)
		}
	})
}

func TestGoodsReceiptWithAnUnknownProductSavesNothing(t *testing.T) {
	forEachQuantityEffectDialect(t, func(t *testing.T, e *effectHarness) {
		receipt := map[string]any{"reference": "GR-2", "supplier": "Dar Traders", "items": []map[string]any{
			{"product": "SODA", "quantity": 5, "unit_cost": 70}, {"product": "GHOST", "quantity": 5, "unit_cost": 70},
		}}
		code, body := e.post(t, "Goods%20Receipt", receipt, "", effectAdmin)
		if code == http.StatusCreated {
			t.Fatalf("a receipt with an unknown product was saved: %s", body)
		}
		if got := e.stockOf(t, "SODA"); got != 10 {
			t.Fatalf("soda stock = %v, want unchanged 10 (the whole save must roll back)", got)
		}
		if n := e.countOf(t, "Goods Receipt"); n != 0 {
			t.Fatalf("a refused receipt left %d record(s) behind", n)
		}
	})
}

func TestStockCannotBeTypedDirectlyOnTheProduct(t *testing.T) {
	forEachQuantityEffectDialect(t, func(t *testing.T, e *effectHarness) {
		rec := e.put(t, "/api/resource/Stock%20Item/SODA", `{"stock_qty":999}`, effectAdmin)
		if got := e.stockOf(t, "SODA"); got != 10 {
			t.Fatalf("stock was typed directly to %v (status %d: %s)", got, rec.Code, rec.Body.String())
		}
	})
}
