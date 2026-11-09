//go:build integration

package api

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/schema"
	ksite "github.com/asenawritescode/kora/internal/site"
)

// TestPrintSourceRecordLinkTitles (#337): a printed record carries the target's
// title for each Link value, also for an actor who may not read the target and
// for child rows; a module-private target is never read and falls back to the
// stored value. Set KORA_API_LIVE_MYSQL_DSN / KORA_API_LIVE_POSTGRES_DSN.
func TestPrintSourceRecordLinkTitles(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_API_LIVE_MYSQL_DSN"},
		{"postgres", "KORA_API_LIVE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			database, dialect := openKernelDialectDatabase(t, tc.driver, dsn)
			if err := ksite.BootstrapSystemTables(context.Background(), database, dialect); err != nil {
				t.Fatalf("bootstrap: %v", err)
			}
			suffix := fmt.Sprintf("%x", time.Now().UnixNano())
			site := "print-title-" + tc.driver + "-" + suffix
			product, hidden, line, order := "TProduct"+suffix, "THidden"+suffix, "TLine"+suffix, "TOrder"+suffix
			title := func(name string) *doctype.DocType {
				return &doctype.DocType{Name: name, TitleField: "label", Fields: []doctype.Field{{Fieldname: "label", Fieldtype: "Data"}}}
			}
			productDT, hiddenDT := title(product), title(hidden)
			hiddenDT.ModuleOwnerID = "other.module" // private: published access not granted
			lineDT := &doctype.DocType{Name: line, IsChildTable: true, Fields: []doctype.Field{
				{Fieldname: "product", Fieldtype: "Link", Options: product}, {Fieldname: "qty", Fieldtype: "Float"}}}
			orderDT := &doctype.DocType{Name: order, Fields: []doctype.Field{
				{Fieldname: "secret", Fieldtype: "Link", Options: hidden},
				{Fieldname: "lines", Fieldtype: "Table", Options: line}}}
			reg := doctype.NewRegistry()
			reg.LoadFull([]*doctype.DocType{productDT, hiddenDT, lineDT, orderDT}, []*doctype.Role{{Name: "Reader"}}, []*doctype.Permission{
				{Doctype: order, Role: "Reader", Read: true},
			})
			cleanupKernelDialectRecords(t, database, dialect, site, product, hidden, line, order)
			if err := schema.MigrateSiteFromRegistry(database, site, reg, dialect); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			tx := &orm.TxManager{DB: database, Registry: reg, Dialect: dialect, Outbox: outbox.NewSQLWriter(dialect), SiteName: site}
			dbTx, err := database.Begin()
			if err != nil {
				t.Fatal(err)
			}
			doc := func(dt *doctype.DocType, name string, kv map[string]any) *doctype.Document {
				d := doctype.NewDocument(dt.Name)
				d.Name = name
				for k, v := range kv {
					d.Set(k, v)
				}
				return d
			}
			for _, row := range []struct {
				dt *doctype.DocType
				d  *doctype.Document
			}{
				{productDT, doc(productDT, "P-1", map[string]any{"label": "Maize Flour 2kg"})},
				{hiddenDT, doc(hiddenDT, "H-1", map[string]any{"label": "Cost Centre"})},
			} {
				if err := tx.InsertInTx(dbTx, row.dt, row.d, "root", "root"); err != nil {
					t.Fatalf("seed %s: %v", row.dt.Name, err)
				}
			}
			parent := doc(orderDT, "O-1", map[string]any{"secret": "H-1"})
			parent.SetTable("lines", []*doctype.Document{
				doc(lineDT, "L-1", map[string]any{"product": "P-1", "qty": 2}),
				doc(lineDT, "L-2", map[string]any{"product": "P-9", "qty": 1}),
			})
			if err := tx.InsertInTx(dbTx, orderDT, parent, "root", "root"); err != nil {
				t.Fatalf("seed order: %v", err)
			}
			if err := dbTx.Commit(); err != nil {
				t.Fatal(err)
			}

			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest("GET", "/", nil)
			ctx.Set("site_name", site)
			ctx.Set("site_db", database)
			ctx.Set("site_registry", reg)
			ctx.Set("site_db_type", tc.driver)
			ctx.Set("user", "carol")
			ctx.Set("user_roles", []string{"Reader"})
			ctx.Set("policy_execution", routerTestExecutionForUser(site, site, "carol", []string{"Reader"}))
			rec, err := sitePrintSource{h: NewHandler(reg, tx)}.Record(ctx, order, "O-1")
			if err != nil {
				t.Fatal(err)
			}
			if got := rec["secret_title"]; got != "H-1" {
				t.Fatalf("private target title = %v, want the stored value", got)
			}
			lines, _ := rec["lines"].([]map[string]any)
			if len(lines) != 2 {
				t.Fatalf("lines = %#v", rec["lines"])
			}
			want := map[any]string{"P-1": "Maize Flour 2kg", "P-9": "P-9"}
			for _, l := range lines {
				if l["product_title"] != want[l["product"]] {
					t.Fatalf("line %v title = %v, want %q", l["product"], l["product_title"], want[l["product"]])
				}
			}
		})
	}
}
