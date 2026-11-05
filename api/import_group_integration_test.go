//go:build integration

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/schema"
	ksite "github.com/asenawritescode/kora/site"
)

func TestCSVImportGroupByAcrossDialects(t *testing.T) {
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
			if err := ksite.BootstrapSystemTables(database, dialect); err != nil {
				t.Fatalf("bootstrap: %v", err)
			}
			suffix := fmt.Sprintf("%x", time.Now().UnixNano())
			site := "grp-import-" + tc.driver + "-" + suffix
			parent, childName := "GrpSale"+suffix, "GrpLine"+suffix
			reg := doctype.NewRegistry()
			reg.LoadFull([]*doctype.DocType{
				{Name: childName, IsChildTable: true, Fields: []doctype.Field{{Fieldname: "sku", Fieldtype: "Data"}, {Fieldname: "qty", Fieldtype: "Int"}}},
				{Name: parent, Fields: []doctype.Field{{Fieldname: "customer", Fieldtype: "Data"}, {Fieldname: "lines", Fieldtype: "Table", Options: childName}}},
			}, []*doctype.Role{{Name: doctype.AdminRole}}, []*doctype.Permission{{
				Doctype: parent, Role: doctype.AdminRole, Read: true, Create: true, Write: true, Delete: true, Import: true,
			}})
			cleanupKernelDialectRecords(t, database, dialect, site, parent, childName)
			if err := schema.MigrateSiteFromRegistry(database, site, reg, dialect); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			writer := outbox.NewSQLWriter(dialect)
			h := NewHandler(reg, &orm.TxManager{DB: database, Registry: reg, Dialect: dialect, Outbox: writer, SiteName: site})
			h.SiteOutboxes = map[string]outbox.Writer{site: writer}
			post := func(id, csv string) map[string]any {
				t.Helper()
				sb, _ := json.Marshal(map[string]any{"import_id": id, "group_by": "Sale No", "target": map[string]string{"doctype": parent},
					"mapping": map[string]string{"Customer": "customer", "SKU": "lines.sku", "Qty": "lines.qty"}})
				var buf bytes.Buffer
				mw := multipart.NewWriter(&buf)
				_ = mw.WriteField("spec", string(sb))
				fw, _ := mw.CreateFormFile("file", "x.csv")
				_, _ = fw.Write([]byte(csv))
				_ = mw.Close()
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/import", &buf)
				c.Request.Header.Set("Content-Type", mw.FormDataContentType())
				c.Set("site_name", site)
				c.Set("site_db", database)
				c.Set("site_registry", reg)
				c.Set("site_db_type", tc.driver)
				c.Set("user", "admin@example.test")
				c.Set("user_roles", []string{doctype.AdminRole})
				c.Set("policy_execution", integrationTestExecution(t, site, "admin@example.test", []string{doctype.AdminRole}))
				h.HandleImport(c)
				if rec.Code != http.StatusOK {
					t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
				}
				var out map[string]any
				_ = json.Unmarshal(rec.Body.Bytes(), &out)
				return out
			}
			count := func(table string) int {
				return countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(table))
			}
			csv := "Sale No,Customer,SKU,Qty\nS1,Bob,a,1\nS1,Bob,b,2\nS1,,c,3\nS2,Ann,d,4\nS2,Eve,e,5\nS1,Bob,f,6\n"
			res := post("g-"+suffix, csv)
			rows, _ := res["rows"].([]any)
			if res["total"] != float64(3) || res["succeeded"] != float64(1) || res["failed"] != float64(2) || len(rows) != 3 {
				t.Fatalf("import: %v", res)
			}
			// S1 ok; S2 inconsistent and the S1 repeat (not contiguous) fail alone.
			if count(reg.Get(parent).RawTableName()) != 1 || count(reg.Get(parent).RawTableName()+"__lines") != 3 {
				t.Fatalf("parents=%d lines=%d", count(reg.Get(parent).RawTableName()), count(reg.Get(parent).RawTableName()+"__lines"))
			}
			if again := post("g-"+suffix, csv); again["succeeded"] != float64(1) || count(reg.Get(parent).RawTableName()) != 1 {
				t.Fatalf("replay: %v parents=%d", again, count(reg.Get(parent).RawTableName()))
			}
		})
	}
}
