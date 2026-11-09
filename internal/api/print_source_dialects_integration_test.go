//go:build integration

package api

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
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
func TestPrintSourceRecordPermissionsAcrossDialects(t *testing.T) {
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
			site := "print-src-" + tc.driver + "-" + suffix
			name := "PrintSale" + suffix
			dt := &doctype.DocType{Name: name, Fields: []doctype.Field{{Fieldname: "total", Fieldtype: "Int"}}}
			reg := doctype.NewRegistry()
			reg.LoadFull([]*doctype.DocType{dt}, []*doctype.Role{{Name: "Reader"}, {Name: "Mine"}, {Name: "Nobody"}}, []*doctype.Permission{
				{Doctype: name, Role: "Reader", Read: true},
				{Doctype: name, Role: "Mine", Read: true, IfOwner: true},
			})
			cleanupKernelDialectRecords(t, database, dialect, site, name)
			if err := schema.MigrateSiteFromRegistry(database, site, reg, dialect); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			writer := outbox.NewSQLWriter(dialect)
			h := NewHandler(reg, &orm.TxManager{DB: database, Registry: reg, Dialect: dialect, Outbox: writer, SiteName: site})
			for _, row := range [][2]string{{"P-1", "alice"}, {"P-2", "bob"}} {
				ts := time.Now().UTC().Format("2006-01-02 15:04:05")
				if _, err := database.Exec(kdb.Rebind(dialect, "INSERT INTO "+dialect.QuoteIdent(dt.RawTableName())+" (name, owner, creation, modified, modified_by, doc_status, idx, total) VALUES (?,?,?,?,?,?,?,?)"),
					row[0], row[1], ts, ts, row[1], 0, 0, 42); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			for _, c := range []struct {
				label, user, role, rec string
				want                   error
			}{
				{"reader any", "carol", "Reader", "P-1", nil},
				{"reader missing", "carol", "Reader", "P-9", ErrPrintNotFound},
				{"no grant", "carol", "Nobody", "P-1", ErrPrintForbidden},
				{"owner own", "alice", "Mine", "P-1", nil},
				{"owner other", "alice", "Mine", "P-2", ErrPrintNotFound},
			} {
				t.Run(c.label, func(t *testing.T) {
					ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
					ctx.Request = httptest.NewRequest("GET", "/", nil)
					ctx.Set("site_name", site)
					ctx.Set("site_db", database)
					ctx.Set("site_registry", reg)
					ctx.Set("site_db_type", tc.driver)
					ctx.Set("user", c.user)
					ctx.Set("user_roles", []string{c.role})
					ctx.Set("policy_execution", routerTestExecutionForUser(site, site, c.user, []string{c.role}))
					rec, err := sitePrintSource{h: h}.Record(ctx, name, c.rec)
					if !errors.Is(err, c.want) || (c.want == nil && rec["total"] == nil) {
						t.Fatalf("got %v %v, want %v", rec, err, c.want)
					}
				})
			}
		})
	}
}
