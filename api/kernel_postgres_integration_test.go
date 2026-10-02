//go:build integration

package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/schema"
	ksite "github.com/asenawritescode/kora/site"
	_ "github.com/lib/pq"
)

func TestLivePostgresResourceCRUDUsesKernelAndPreservesRESTContract(t *testing.T) {
	dsn := os.Getenv("KORA_API_LIVE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_API_LIVE_POSTGRES_DSN to a disposable PostgreSQL database for kernel CRUD parity")
	}
	adminDB, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("open PostgreSQL database:", err)
	}
	if err := adminDB.Ping(); err != nil {
		_ = adminDB.Close()
		t.Fatal("ping PostgreSQL database:", err)
	}
	schemaName := fmt.Sprintf("kora_api_kernel_%x", time.Now().UnixNano())
	if _, err := adminDB.Exec(`CREATE SCHEMA "` + schemaName + `"`); err != nil {
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
	if err := database.Ping(); err != nil {
		t.Fatal("ping isolated PostgreSQL schema:", err)
	}
	dialect := kdb.Resolve("postgres")
	if err := ksite.BootstrapSystemTables(database, dialect); err != nil {
		t.Fatal("bootstrap isolated PostgreSQL system tables:", err)
	}

	suffix := fmt.Sprintf("%x", time.Now().UnixNano())
	siteName := "postgres-kernel-" + suffix
	dt := &doctype.DocType{Name: "Kernel Test " + suffix, Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data", Reqd: true}}}
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
	path := "/api/resource/" + url.PathEscape(dt.Name)
	created := serveResourceMutationBodyWithDBType(t, handler, database, registry, siteName,
		httpMethodPost, path, `{"title":"Created on PostgreSQL"}`, "postgres-kernel-create-"+suffix, []string{doctype.AdminRole}, "postgres")
	if created.Code != http.StatusCreated {
		t.Fatalf("PostgreSQL create returned HTTP %d: %s", created.Code, created.Body.String())
	}
	doc := requireRESTDocument(t, created.Body.Bytes(), dt.Name)
	name, ok := doc["name"].(string)
	if !ok || name == "" || doc["title"] != "Created on PostgreSQL" {
		t.Fatalf("PostgreSQL create response contract mismatch: %#v", doc)
	}
	replayed := serveResourceMutationBodyWithDBType(t, handler, database, registry, siteName,
		httpMethodPost, path, `{"title":"Created on PostgreSQL"}`, "postgres-kernel-create-"+suffix, []string{doctype.AdminRole}, "postgres")
	if replayed.Code != http.StatusCreated || replayed.Header().Get("X-Kora-Replay") != "true" {
		t.Fatalf("PostgreSQL replay returned HTTP %d replay=%q: %s", replayed.Code, replayed.Header().Get("X-Kora-Replay"), replayed.Body.String())
	}
	updated := serveResourceMutationBodyWithDBType(t, handler, database, registry, siteName,
		httpMethodPut, path+"/"+url.PathEscape(name), `{"title":"Updated on PostgreSQL"}`, "", []string{doctype.AdminRole}, "postgres")
	if updated.Code != http.StatusOK {
		t.Fatalf("PostgreSQL update returned HTTP %d: %s", updated.Code, updated.Body.String())
	}
	deleted := serveResourceMutationBodyWithDBType(t, handler, database, registry, siteName,
		httpMethodDelete, path+"/"+url.PathEscape(name), "", "", []string{doctype.AdminRole}, "postgres")
	if deleted.Code != http.StatusOK {
		t.Fatalf("PostgreSQL delete returned HTTP %d: %s", deleted.Code, deleted.Body.String())
	}
	var records, audits, events int
	if err := database.QueryRow(kdb.Rebind(dialect, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(dt.RawTableName())+" WHERE name = ?"), name).Scan(&records); err != nil {
		t.Fatal("count PostgreSQL records:", err)
	}
	if err := database.QueryRow(kdb.Rebind(dialect, "SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND doc_name = ? AND status = 'completed'"), siteName, name).Scan(&audits); err != nil {
		t.Fatal("count PostgreSQL operation audits:", err)
	}
	if err := database.QueryRow(kdb.Rebind(dialect, "SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id = ?"), siteName, name).Scan(&events); err != nil {
		t.Fatal("count PostgreSQL outbox events:", err)
	}
	if records != 0 || audits != 3 || events != 3 {
		t.Fatalf("PostgreSQL CRUD effects records=%d audits=%d outbox=%d, want 0/3/3", records, audits, events)
	}
}
