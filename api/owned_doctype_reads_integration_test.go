//go:build integration

package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/schema"
	ksite "github.com/asenawritescode/kora/site"
	"github.com/gin-gonic/gin"
)

func serveOwnedRead(t testing.TB, h *Handler, db *sql.DB, reg *doctype.Registry, site, dbType, path string, roles []string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, path, nil)
	parts := strings.Split(strings.Trim(path, "/"), "/")
	name, _ := url.PathUnescape(parts[2])
	ctx.Params = gin.Params{{Key: "doctype", Value: name}}
	if len(parts) > 3 {
		ctx.Params = append(ctx.Params, gin.Param{Key: "name", Value: parts[3]})
	}
	ctx.Set("site_name", site)
	ctx.Set("site_db", db)
	ctx.Set("site_registry", reg)
	ctx.Set("site_db_type", dbType)
	ctx.Set("user", "reader@example.test")
	ctx.Set("user_roles", roles)
	ctx.Set("policy_execution", integrationTestExecution(t, site, "reader@example.test", roles))
	if len(parts) > 3 {
		h.HandleGet(ctx)
	} else {
		h.HandleList(ctx)
	}
	return rec
}

func assertOwnedDocTypeReadsViaMatrix(t *testing.T, h *Handler, db *sql.DB, reg *doctype.Registry, site, dbType string) {
	dt := &doctype.DocType{Name: "Owned Sale " + fmt.Sprintf("%x", time.Now().UnixNano()), SortField: "modified", SortOrder: "DESC", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}}}
	reg.Register(dt)
	for role, p := range map[string]doctype.Permission{
		"Reader": {Read: true}, "Writer": {Read: true, Create: true, Write: true, Delete: true},
	} {
		p.Doctype, p.Role = dt.Name, role
		reg.Permissions.SetPermission(&p)
	}
	if err := schema.MigrateSiteFromRegistry(db, site, reg, kdb.Resolve(dbType)); err != nil {
		t.Fatal(err)
	}
	path := "/api/resource/" + url.PathEscape(dt.Name)
	created := serveResourceMutationBodyWithDBType(t, h, db, reg, site, httpMethodPost, path, `{"title":"x"}`, "", []string{"Writer"}, dbType)
	if created.Code != http.StatusCreated {
		t.Fatalf("seed create = %d: %s", created.Code, created.Body.String())
	}
	name := requireRESTDocument(t, created.Body.Bytes(), dt.Name)["name"].(string)
	record := path + "/" + url.PathEscape(name)

	dt.ModuleOwnerID, dt.ModuleOwnershipMode = "pos.core", string(contract.MutationCommands)
	for _, tc := range []struct {
		access contract.ModuleAccess
		role   string
		path   string
		want   int
	}{
		{contract.ModulePublished, "Reader", path, 200},
		{contract.ModulePublished, "Reader", record, 200},
		{contract.ModulePublished, "Stranger", path, 403},
		{contract.ModulePublished, "Stranger", record, 403},
		{contract.ModulePrivate, "Reader", path, 403},
		{contract.ModulePrivate, "Writer", record, 403},
		{"", "Reader", path, 403},
	} {
		dt.ModuleAccess = tc.access
		if got := serveOwnedRead(t, h, db, reg, site, dbType, tc.path, []string{tc.role}); got.Code != tc.want {
			t.Errorf("GET %s access=%q role=%s = %d, want %d: %s", tc.path, tc.access, tc.role, got.Code, tc.want, got.Body.String())
		}
	}
	dt.ModuleAccess = contract.ModulePublished
	for _, m := range []struct {
		method resourceHTTPMethod
		path   string
	}{{httpMethodPost, path}, {httpMethodPut, record}, {httpMethodDelete, record}} {
		if got := serveResourceMutationBodyWithDBType(t, h, db, reg, site, m.method, m.path, `{"title":"y"}`, "", []string{"Writer"}, dbType); got.Code != http.StatusForbidden {
			t.Errorf("%s %s on owned DocType = %d, want 403: %s", m.method, m.path, got.Code, got.Body.String())
		}
	}
}

func TestPublishedOwnedDocTypeReadsViaMatrixMySQL(t *testing.T) {
	h, db, reg, site := newResourceIntegrationHandler(t)
	assertOwnedDocTypeReadsViaMatrix(t, h, db, reg, site, "mysql")
}

func TestPublishedOwnedDocTypeReadsViaMatrixPostgres(t *testing.T) {
	dsn := os.Getenv("KORA_API_LIVE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_API_LIVE_POSTGRES_DSN to a disposable PostgreSQL database")
	}
	db, dialect := openKernelDialectDatabase(t, "postgres", dsn)
	if err := bootstrapForOwnedReads(db, dialect); err != nil {
		t.Fatal(err)
	}
	reg := doctype.NewRegistry()
	reg.LoadFull(nil, []*doctype.Role{{Name: "Writer"}, {Name: "Reader"}}, nil)
	site := fmt.Sprintf("owned-reads-%x", time.Now().UnixNano())
	w := outbox.NewSQLWriter(dialect)
	h := NewHandler(reg, &orm.TxManager{DB: db, Registry: reg, Dialect: dialect, Outbox: w, SiteName: site})
	h.SiteOutboxes = map[string]outbox.Writer{site: w}
	assertOwnedDocTypeReadsViaMatrix(t, h, db, reg, site, "postgres")
}

func bootstrapForOwnedReads(db *sql.DB, dialect kdb.Dialect) error {
	return ksite.BootstrapSystemTables(db, dialect)
}
