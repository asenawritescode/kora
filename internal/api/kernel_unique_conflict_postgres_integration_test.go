//go:build integration

package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/schema"
	ksite "github.com/asenawritescode/kora/internal/site"
	_ "github.com/lib/pq"
)

// TestLivePostgresResourceCRUDUniqueConflictMapsTo409 mirrors
// TestResourceCRUDUniqueConflictMapsTo409 on PostgreSQL: a duplicate unique
// value must answer 409 naming the field, not 503 DEPENDENCY_UNAVAILABLE.
func TestLivePostgresResourceCRUDUniqueConflictMapsTo409(t *testing.T) {
	dsn := os.Getenv("KORA_API_LIVE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_API_LIVE_POSTGRES_DSN to a disposable PostgreSQL database for unique conflict parity")
	}
	adminDB, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("open PostgreSQL database:", err)
	}
	if err = adminDB.Ping(); err != nil {
		_ = adminDB.Close()
		t.Fatal("ping PostgreSQL database:", err)
	}
	schemaName := fmt.Sprintf("kora_api_unique_%x", time.Now().UnixNano())
	if _, err = adminDB.Exec(`CREATE SCHEMA "` + schemaName + `"`); err != nil {
		_ = adminDB.Close()
		t.Fatal("create isolated PostgreSQL schema:", err)
	}
	t.Cleanup(func() {
		_, _ = adminDB.Exec(`DROP SCHEMA "` + schemaName + `" CASCADE`)
		_ = adminDB.Close()
	})
	parsedDSN, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("parse PostgreSQL DSN:", err)
	}
	query := parsedDSN.Query()
	query.Set("search_path", schemaName)
	parsedDSN.RawQuery = query.Encode()
	database, err := sql.Open("postgres", parsedDSN.String())
	if err != nil {
		t.Fatal("open isolated PostgreSQL schema:", err)
	}
	database.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = database.Close() })
	dialect := kdb.Resolve("postgres")
	if err := ksite.BootstrapSystemTables(context.Background(), database, dialect); err != nil {
		t.Fatal("bootstrap isolated PostgreSQL system tables:", err)
	}

	siteName := "postgres-unique-" + fmt.Sprintf("%x", time.Now().UnixNano())
	dt := &doctype.DocType{Name: "TestDoc", Fields: []doctype.Field{
		{Fieldname: "title", Fieldtype: "Data", Label: "Title", Reqd: true},
		{Fieldname: "serial", Fieldtype: "Data", Label: "Serial", Unique: true},
	}}
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{dt}, []*doctype.Role{{Name: doctype.AdminRole}}, []*doctype.Permission{{
		Doctype: dt.Name, Role: doctype.AdminRole, Read: true, Write: true, Create: true, Delete: true,
	}})
	if err := schema.MigrateSiteFromRegistry(database, siteName, registry, dialect); err != nil {
		t.Fatal("migrate PostgreSQL DocType:", err)
	}
	writer := outbox.NewSQLWriter(dialect)
	handler := NewHandler(registry, &orm.TxManager{DB: database, Registry: registry, Dialect: dialect, Outbox: writer, SiteName: siteName})
	handler.SiteOutboxes = map[string]outbox.Writer{siteName: writer}
	roles := []string{doctype.AdminRole}

	first := serveResourceMutationBodyWithDBType(t, handler, database, registry, siteName,
		httpMethodPost, "/api/resource/TestDoc", `{"title":"first","serial":"shared"}`, "", roles, "postgres")
	if first.Code != http.StatusCreated {
		t.Fatalf("create first record status = %d: %s", first.Code, first.Body.String())
	}
	second := serveResourceMutationBodyWithDBType(t, handler, database, registry, siteName,
		httpMethodPost, "/api/resource/TestDoc", `{"title":"second","serial":"other"}`, "", roles, "postgres")
	if second.Code != http.StatusCreated {
		t.Fatalf("create second record status = %d: %s", second.Code, second.Body.String())
	}
	name, _ := requireRESTDocument(t, second.Body.Bytes(), "TestDoc")["name"].(string)
	if name == "" {
		t.Fatal("second create response is missing its record name")
	}
	conflict := serveResourceMutationBodyWithDBType(t, handler, database, registry, siteName,
		httpMethodPut, "/api/resource/TestDoc/"+url.PathEscape(name), `{"serial":"shared"}`, "", roles, "postgres")
	if conflict.Code != http.StatusConflict {
		t.Fatalf("PostgreSQL unique write conflict status = %d, want 409: %s", conflict.Code, conflict.Body.String())
	}
	if body := conflict.Body.String(); !strings.Contains(body, "serial") {
		t.Fatalf("PostgreSQL unique conflict body does not name field serial: %s", body)
	}
}
