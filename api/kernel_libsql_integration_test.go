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
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func TestLiveLibSQLResourceCRUDUsesKernelAndPreservesRESTContract(t *testing.T) {
	dsn := os.Getenv("KORA_API_LIVE_LIBSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_API_LIVE_LIBSQL_DSN to a disposable LibSQL database for kernel CRUD parity")
	}
	dialect := kdb.Resolve("libsql")
	database, err := sql.Open("libsql", dsn)
	if err != nil {
		t.Fatal("open disposable LibSQL database:", err)
	}
	database.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Ping(); err != nil {
		t.Fatal("ping disposable LibSQL database:", err)
	}
	if err := ksite.BootstrapSystemTables(database, dialect); err != nil {
		t.Fatal("bootstrap LibSQL system tables:", err)
	}

	suffix := fmt.Sprintf("%x", time.Now().UnixNano())
	siteName := "libsql-kernel-" + suffix
	dt := &doctype.DocType{Name: "Kernel Test " + suffix, Fields: []doctype.Field{
		{Fieldname: "title", Fieldtype: "Data", Reqd: true},
	}}
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{dt}, []*doctype.Role{{Name: doctype.AdminRole}}, []*doctype.Permission{{
		Doctype: dt.Name, Role: doctype.AdminRole, Read: true, Write: true, Create: true, Delete: true,
	}})
	if err := schema.MigrateSiteFromRegistry(database, siteName, registry, dialect); err != nil {
		t.Fatal("migrate LibSQL DocType:", err)
	}
	t.Cleanup(func() {
		_, _ = database.Exec("DROP TABLE " + dialect.QuoteIdent(dt.RawTableName()))
	})
	writer := outbox.NewSQLWriter(dialect)
	handler := NewHandler(registry, &orm.TxManager{DB: database, Registry: registry, Dialect: dialect, Outbox: writer, SiteName: siteName})
	handler.SiteOutboxes = map[string]outbox.Writer{siteName: writer}
	path := "/api/resource/" + url.PathEscape(dt.Name)

	created := serveResourceMutationBodyWithDBType(t, handler, database, registry, siteName,
		httpMethodPost, path, `{"title":"Created on LibSQL"}`, "libsql-create-"+suffix, []string{doctype.AdminRole}, "libsql")
	if created.Code != http.StatusCreated {
		t.Fatalf("LibSQL create returned HTTP %d: %s", created.Code, created.Body.String())
	}
	doc := requireRESTDocument(t, created.Body.Bytes(), dt.Name)
	name, ok := doc["name"].(string)
	if !ok || name == "" || doc["title"] != "Created on LibSQL" {
		t.Fatalf("LibSQL create response contract mismatch: %#v", doc)
	}
	replayed := serveResourceMutationBodyWithDBType(t, handler, database, registry, siteName,
		httpMethodPost, path, `{"title":"Created on LibSQL"}`, "libsql-create-"+suffix, []string{doctype.AdminRole}, "libsql")
	if replayed.Code != http.StatusCreated || replayed.Header().Get("X-Kora-Replay") != "true" {
		t.Fatalf("LibSQL replay returned HTTP %d replay=%q: %s", replayed.Code, replayed.Header().Get("X-Kora-Replay"), replayed.Body.String())
	}
	updated := serveResourceMutationBodyWithDBType(t, handler, database, registry, siteName,
		httpMethodPut, path+"/"+name, `{"title":"Updated on LibSQL"}`, "", []string{doctype.AdminRole}, "libsql")
	if updated.Code != http.StatusOK {
		t.Fatalf("LibSQL update returned HTTP %d: %s", updated.Code, updated.Body.String())
	}
	deleted := serveResourceMutationBodyWithDBType(t, handler, database, registry, siteName,
		httpMethodDelete, path+"/"+name, "", "", []string{doctype.AdminRole}, "libsql")
	if deleted.Code != http.StatusOK {
		t.Fatalf("LibSQL delete returned HTTP %d: %s", deleted.Code, deleted.Body.String())
	}
	var records, audits, events int
	if err := database.QueryRow("SELECT COUNT(*) FROM "+dialect.QuoteIdent(dt.RawTableName())+" WHERE name = ?", name).Scan(&records); err != nil {
		t.Fatal("count LibSQL records:", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ? AND doc_name = ? AND status = 'completed'", siteName, name).Scan(&audits); err != nil {
		t.Fatal("count LibSQL operation audits:", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_outbox WHERE site = ? AND aggregate_id = ?", siteName, name).Scan(&events); err != nil {
		t.Fatal("count LibSQL outbox events:", err)
	}
	if records != 0 || audits != 3 || events != 3 {
		t.Fatalf("LibSQL CRUD effects records=%d audits=%d outbox=%d, want 0/3/3", records, audits, events)
	}
}
