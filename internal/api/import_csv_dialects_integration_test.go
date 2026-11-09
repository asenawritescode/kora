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
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/schema"
	ksite "github.com/asenawritescode/kora/internal/site"
)

// Set KORA_API_LIVE_MYSQL_DSN / KORA_API_LIVE_POSTGRES_DSN to disposable servers.
func TestCSVImport500RowsThroughHandlerAcrossDialects(t *testing.T) {
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
			site := "csv-import-" + tc.driver + "-" + suffix
			name := "CsvItem" + suffix
			dt := &doctype.DocType{Name: name, Fields: []doctype.Field{
				{Fieldname: "sku", Fieldtype: "Data", Reqd: true, Unique: true},
				{Fieldname: "qty", Fieldtype: "Int"},
			}}
			reg := doctype.NewRegistry()
			reg.LoadFull([]*doctype.DocType{dt}, []*doctype.Role{{Name: doctype.AdminRole}}, []*doctype.Permission{{
				Doctype: name, Role: doctype.AdminRole, Read: true, Create: true, Write: true, Delete: true, Import: true,
			}})
			cleanupKernelDialectRecords(t, database, dialect, site, name)
			if err := schema.MigrateSiteFromRegistry(database, site, reg, dialect); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			writer := outbox.NewSQLWriter(dialect)
			h := NewHandler(reg, &orm.TxManager{DB: database, Registry: reg, Dialect: dialect, Outbox: writer, SiteName: site})
			h.SiteOutboxes = map[string]outbox.Writer{site: writer}

			post := func(csv string, spec map[string]any) map[string]any {
				t.Helper()
				sb, _ := json.Marshal(spec)
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
					t.Fatalf("import status %d: %s", rec.Code, rec.Body.String())
				}
				var out map[string]any
				_ = json.Unmarshal(rec.Body.Bytes(), &out)
				return out
			}
			count := func() int {
				return countDialectRows(t, database, dialect, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(dt.RawTableName()))
			}
			var sb strings.Builder
			sb.WriteString("SKU,Qty\n")
			for i := 1; i <= 500; i++ {
				fmt.Fprintf(&sb, "S%04d,%d\n", i, i)
			}
			mapping := map[string]string{"SKU": "sku", "Qty": "qty"}

			dry := post(sb.String(), map[string]any{"import_id": "dry-" + suffix, "target": map[string]string{"doctype": name}, "mapping": mapping, "dry_run": true})
			if dry["succeeded"] != float64(500) || count() != 0 {
				t.Fatalf("dry run: %v rows=%d", dry["succeeded"], count())
			}
			res := post(sb.String(), map[string]any{"import_id": "real-" + suffix, "target": map[string]string{"doctype": name}, "mapping": mapping})
			if res["total"] != float64(500) || res["succeeded"] != float64(500) || res["failed"] != float64(0) || count() != 500 {
				t.Fatalf("import: %v rows=%d", res, count())
			}
			var qty int
			if err := database.QueryRow(kdb.Rebind(dialect, "SELECT qty FROM "+dialect.QuoteIdent(dt.RawTableName())+" WHERE sku = ?"), "S0500").Scan(&qty); err != nil || qty != 500 {
				t.Fatalf("last row qty=%d err=%v", qty, err)
			}
			// Re-importing the same file hits the unique index on every row.
			dup := post(sb.String(), map[string]any{"import_id": "dup-" + suffix, "target": map[string]string{"doctype": name}, "mapping": mapping})
			if dup["failed"] != float64(500) || count() != 500 {
				t.Fatalf("duplicate import: failed=%v rows=%d", dup["failed"], count())
			}
		})
	}
}
