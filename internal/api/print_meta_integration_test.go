//go:build integration

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"

	"github.com/asenawritescode/kora/internal/configstore"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/print"
	"github.com/asenawritescode/kora/internal/site"
)

// Set KORA_PRINT_META_MYSQL_DSN / KORA_PRINT_META_POSTGRES_DSN to disposable servers.
func TestPrintFormatMetaAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_PRINT_META_MYSQL_DSN"},
		{"postgres", "KORA_PRINT_META_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			fixture := disposabledb.Open(t, tc.driver, dsn)
			conn, name := fixture.DB, fixture.Name
			dialect := kdb.Resolve(tc.driver)
			if err := site.BootstrapSystemTables(context.Background(), conn, dialect); err != nil {
				t.Fatal(err)
			}
			reg := doctype.NewRegistry()
			const siteName = "print-meta.example.test"
			if err := site.ImportConfig(conn, reg, name, siteName, filepath.Join("..", "..", "config", "pos"), dialect); err != nil {
				t.Fatal(err)
			}
			snap, _, err := configstore.NewStore(conn, dialect).LoadActiveSnapshotContext(t.Context(), siteName)
			if err != nil {
				t.Fatal(err)
			}
			formats := map[string]*print.Format{}
			for _, raw := range snap.PrintFormats {
				f, err := print.Decode(raw)
				if err != nil {
					t.Fatal(err)
				}
				formats[f.Name] = f
			}
			svc := NewSiteRuntimeServices()
			svc.Replace(siteName, SiteRuntimeService{PrintFormats: formats})
			h := NewHandler(reg, nil)
			h.RuntimeServices = svc
			get := func(path string, roles []string) *httptest.ResponseRecorder {
				r := gin.New()
				r.Use(func(c *gin.Context) {
					c.Set("site_name", siteName)
					c.Set("site_registry", reg)
					c.Set("policy_execution", integrationTestExecution(t, siteName, "u@example.test", roles))
				})
				src := sitePrintSource{h: h}
				r.GET("/print-formats", NewPrintListHandler(src))
				r.GET("/print-formats/:format_name", NewPrintMetaHandler(src))
				w := httptest.NewRecorder()
				r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
				return w
			}
			if w := get("/print-formats/sale_receipt", []string{"Cashier"}); w.Code != 200 || !strings.Contains(w.Body.String(), `"paper":"thermal_80"`) {
				t.Fatalf("cashier: %d %s", w.Code, w.Body.String())
			}
			if w := get("/print-formats/sale_receipt", []string{"Stranger"}); w.Code != 403 {
				t.Fatalf("stranger get: %d %s", w.Code, w.Body.String())
			}
			if w := get("/print-formats", []string{"Cashier"}); !strings.Contains(w.Body.String(), `"sale_receipt"`) {
				t.Fatalf("cashier list: %s", w.Body.String())
			}
			for _, role := range []string{"Cashier", "Manager", "Owner"} {
				if w := get("/print-formats?record=Sale", []string{role}); w.Code != 200 || !strings.Contains(w.Body.String(), `"sale_record_receipt"`) {
					t.Fatalf("%s record list: %d %s", role, w.Code, w.Body.String())
				}
			}
			if w := get("/print-formats/sale_record_receipt", []string{"Stranger"}); w.Code != 403 {
				t.Fatalf("stranger record get: %d %s", w.Code, w.Body.String())
			}
			if w := get("/print-formats?record=Sale", []string{"Stranger"}); strings.Contains(w.Body.String(), `"sale_record_receipt"`) {
				t.Fatalf("stranger record list leaks: %s", w.Body.String())
			}
			if w := get("/print-formats", []string{"Stranger"}); strings.Contains(w.Body.String(), `"sale_receipt"`) {
				t.Fatalf("stranger list leaks: %s", w.Body.String())
			}
		})
	}
}
