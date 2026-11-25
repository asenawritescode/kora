//go:build integration

package api

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/configstore"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/schema"
	"github.com/asenawritescode/kora/internal/script"
	ksite "github.com/asenawritescode/kora/internal/site"
)

// Pack script tests load config packs as shipped, with their roles, permissions and
// scripts/<DocType>/<event>.js installed the way the Engine installs them on activation,
// into one fresh database per dialect. Every such test is named TestPackScripts<Pack>
// so CI runs them on PostgreSQL with -run 'TestPackScripts/postgres'.
const (
	packScriptsMySQLEnv    = "KORA_PACK_SCRIPTS_DISPOSABLE_MYSQL_DSN"
	packScriptsPostgresEnv = "KORA_PACK_SCRIPTS_DISPOSABLE_POSTGRES_DSN"
)

// packFixture is one disposable database with config packs and their scripts installed.
type packFixture struct {
	t       *testing.T
	handler *Handler
	db      *sql.DB
	reg     *doctype.Registry
	site    string
	dialect string
	roles   []string
}

// eachPackDialect runs scenario once on MySQL and once on PostgreSQL, each on a fresh
// database holding the given packs. A dialect without its DSN variable is skipped.
func eachPackDialect(t *testing.T, packs []string, scenario func(f *packFixture)) {
	t.Helper()
	for _, tc := range []struct{ dialect, env string }{
		{"mysql", packScriptsMySQLEnv},
		{"postgres", packScriptsPostgresEnv},
	} {
		t.Run(tc.dialect, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			scenario(openPackFixture(t, tc.dialect, dsn, packs))
		})
	}
}

func openPackFixture(t *testing.T, dialect, dsn string, packs []string) *packFixture {
	t.Helper()
	ctx := context.Background()
	database := disposabledb.Open(t, dialect, dsn).DB
	database.SetMaxOpenConns(4)
	dl := kdb.Resolve(dialect)
	if err := ksite.BootstrapSystemTables(ctx, database, dl); err != nil {
		t.Fatalf("bootstrap system tables: %v", err)
	}
	for _, ddl := range extensibilityScriptTables(dialect) {
		if _, err := database.Exec(ddl); err != nil {
			t.Fatalf("create script tables: %v", err)
		}
	}
	for _, ddl := range configstore.ConfigRevisionSchema(dl) {
		if _, err := database.Exec(ddl); err != nil {
			t.Fatalf("create config revision table: %v", err)
		}
	}
	if err := configstore.SeedConfigRevision(ctx, database, dl); err != nil {
		t.Fatalf("seed config revision: %v", err)
	}

	site := "packs-" + dialect
	reg := loadPackRegistry(t, packs)
	if err := schema.MigrateSiteFromRegistry(database, site, reg, dl); err != nil {
		t.Fatalf("migrate pack schema: %v", err)
	}
	writer := outbox.NewSQLWriter(dl)
	store := &script.Store{DB: database, Dialect: dl}
	handler := NewHandler(reg, &orm.TxManager{DB: database, Registry: reg, Dialect: dl, Outbox: writer, SiteName: site})
	handler.SiteOutboxes = map[string]outbox.Writer{site: writer}
	handler.SiteScriptStores = map[string]*script.Store{site: store}
	handler.ScriptRunner = script.NewEmbeddedRunner(script.DefaultEmbeddedConfig())
	for _, pack := range packs {
		installPackScripts(t, store, site, filepath.Join(pack, "scripts"))
	}
	return &packFixture{t: t, handler: handler, db: database, reg: reg, site: site, dialect: dialect, roles: []string{doctype.AdminRole}}
}

// loadPackRegistry parses the packs' record types, roles and permissions. The
// Administrator may do everything, as on a real workspace.
func loadPackRegistry(t *testing.T, packs []string) *doctype.Registry {
	t.Helper()
	var (
		docTypes    []*doctype.DocType
		roles       = []*doctype.Role{{Name: doctype.AdminRole}}
		permissions []*doctype.Permission
	)
	for _, pack := range packs {
		parsed, err := doctype.ParseConfigTree(pack)
		if err != nil {
			t.Fatalf("parse %s: %v", pack, err)
		}
		docTypes = append(docTypes, parsed...)
		packRoles, err := doctype.ParseRolesFile(filepath.Join(pack, "roles.yaml"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("roles of %s: %v", pack, err)
		}
		roles = append(roles, packRoles...)
		packPermissions, err := doctype.ParsePermissionsFile(filepath.Join(pack, "permissions.yaml"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("permissions of %s: %v", pack, err)
		}
		permissions = append(permissions, packPermissions...)
	}
	for _, recordType := range docTypes {
		permissions = append(permissions, &doctype.Permission{
			Doctype: recordType.Name, Role: doctype.AdminRole, Read: true, Write: true, Create: true, Delete: true, Submit: true,
		})
	}
	reg := doctype.NewRegistry()
	reg.LoadFull(docTypes, roles, permissions)
	return reg
}

// extensibilityScriptTables returns the DDL for the script store tables only.
func extensibilityScriptTables(dialect string) []string {
	var all []string
	if dialect == "postgres" {
		all = kdb.ExtensibilityTablesPostgres()
	} else {
		all = kdb.ExtensibilityTablesMySQL()
	}
	var out []string
	for _, ddl := range all {
		if strings.Contains(ddl, "_kora_script") && !strings.Contains(ddl, "_kora_extension") {
			out = append(out, ddl)
		}
	}
	return out
}

// installPackScripts stores every scripts/<DocType>/<event>.js of a pack as an active
// document-event script, the rows the Engine writes when a pack is activated. A pack
// without a scripts directory installs nothing.
func installPackScripts(t *testing.T, store *script.Store, site, root string) {
	t.Helper()
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		return
	}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Ext(path) != ".js" {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		doctypeName := filepath.Dir(rel)
		event := strings.TrimSuffix(filepath.Base(rel), ".js")
		return store.Insert(script.Record{
			Name: doctypeName + " " + event, Site: site, ScriptType: script.TypeDocEvent,
			DocType: doctypeName, Event: script.Event(event), IsActive: true,
			Script: string(body), TimeoutMs: 5000, CreatedBy: "pack", UpdatedBy: "pack",
		})
	})
	if err != nil {
		t.Fatalf("install pack scripts: %v", err)
	}
}

// as returns the same fixture acting with the given roles instead of Administrator.
func (f *packFixture) as(roles ...string) *packFixture {
	other := *f
	other.roles = roles
	return &other
}

// exec runs one statement against the fixture database (seeding rows a pack does not create).
func (f *packFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(kdb.Rebind(kdb.Resolve(f.dialect), query), args...); err != nil {
		f.t.Fatalf("exec %s: %v", query, err)
	}
}

// quote quotes an identifier for the fixture's dialect.
func (f *packFixture) quote(name string) string {
	return kdb.Resolve(f.dialect).QuoteIdent(name)
}

// send returns the status, the response body and the saved document (nil when refused).
func (f *packFixture) send(method resourceHTTPMethod, recordType, name, body string) (int, string, map[string]any) {
	f.t.Helper()
	path := "/api/resource/" + url.PathEscape(recordType)
	if name != "" {
		path += "/" + url.PathEscape(name)
	}
	recorder := serveResourceMutationBodyWithDBType(f.t, f.handler, f.db, f.reg, f.site, method, path, body, "", f.roles, f.dialect)
	if recorder.Code != http.StatusOK && recorder.Code != http.StatusCreated {
		return recorder.Code, recorder.Body.String(), nil
	}
	if method == httpMethodDelete {
		return recorder.Code, recorder.Body.String(), map[string]any{}
	}
	return recorder.Code, recorder.Body.String(), requireRESTDocument(f.t, recorder.Body.Bytes(), recordType)
}

// create saves a new record and returns its name; a refusal fails the test.
func (f *packFixture) create(recordType, body string) string {
	f.t.Helper()
	code, reply, doc := f.send(httpMethodPost, recordType, "", body)
	if doc == nil {
		f.t.Fatalf("create %s = %d: %s\nreply: %s", recordType, code, body, reply)
	}
	name, _ := doc["name"].(string)
	return name
}

// update saves changes to a record; a refusal fails the test.
func (f *packFixture) update(recordType, name, body string) {
	f.t.Helper()
	code, reply, doc := f.send(httpMethodPut, recordType, name, body)
	if doc == nil {
		f.t.Fatalf("update %s %s = %d: %s\nreply: %s", recordType, name, code, body, reply)
	}
}

// refused saves the record and returns the refusal reply; it fails the test when the save
// succeeds, or when the reply does not contain want.
func (f *packFixture) refused(method resourceHTTPMethod, recordType, name, body, want string) string {
	f.t.Helper()
	_, reply, doc := f.send(method, recordType, name, body)
	if doc != nil {
		f.t.Fatalf("%s %s %s was saved, want refused: %s", method, recordType, name, body)
	}
	if !strings.Contains(reply, want) {
		f.t.Fatalf("%s %s %s refused with %s, want it to mention %q", method, recordType, name, reply, want)
	}
	return reply
}

// value reads one column of one record as text.
func (f *packFixture) value(recordType, name, column string) string {
	f.t.Helper()
	dl := kdb.Resolve(f.dialect)
	query := "SELECT " + dl.QuoteIdent(column) + " FROM " + dl.QuoteIdent("tab"+recordType) + " WHERE name = ?"
	var out sql.NullString
	if err := f.db.QueryRow(kdb.Rebind(dl, query), name).Scan(&out); err != nil {
		f.t.Fatalf("%s %s.%s: %v", recordType, name, column, err)
	}
	return out.String
}

// amount reads a number column as a float.
func (f *packFixture) amount(recordType, name, column string) float64 {
	f.t.Helper()
	text := f.value(recordType, name, column)
	if text == "" {
		return 0
	}
	got, err := strconv.ParseFloat(text, 64)
	if err != nil {
		f.t.Fatalf("%s %s.%s is not a number: %v", recordType, name, column, err)
	}
	return got
}

// expect fails the test unless the column holds want.
func (f *packFixture) expect(recordType, name, column, want string) {
	f.t.Helper()
	if got := f.value(recordType, name, column); got != want {
		f.t.Fatalf("%s %s.%s = %q, want %q", recordType, name, column, got, want)
	}
}

// count returns how many rows of a table match where (a SQL condition with ? args).
func (f *packFixture) count(table, where string, args ...any) int {
	f.t.Helper()
	dl := kdb.Resolve(f.dialect)
	query := "SELECT COUNT(*) FROM " + dl.QuoteIdent(table)
	if where != "" {
		query += " WHERE " + where
	}
	var rows int
	if err := f.db.QueryRow(kdb.Rebind(dl, query), args...).Scan(&rows); err != nil {
		f.t.Fatalf("count %s: %v", table, err)
	}
	return rows
}
