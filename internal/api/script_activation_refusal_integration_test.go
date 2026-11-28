//go:build integration

package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/configstore"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/gin-gonic/gin"
)

// TestScriptPlanRefusalActivation checks that Studio activate and rollback refuse
// an owned script the planning check rejects with 409, before any DDL or journal
// row. Each dialect reads its own disposable DSN and is skipped when it is unset.
func TestScriptPlanRefusalActivation(t *testing.T) {
	dialects := []struct {
		name, dsnEnv, driver string
	}{
		{name: "mysql", dsnEnv: "KORA_F5_MYSQL_DSN", driver: "mysql"},
		{name: "postgres", dsnEnv: "KORA_F5_POSTGRES_DSN", driver: "postgres"},
	}
	cases := []struct {
		name     string
		action   string
		slug     string
		doctype  string
		ownerRow bool
		source   string
		wantCode string
	}{
		{name: "activate owner conflict", action: "activate", slug: "ac", doctype: "F5Ac", ownerRow: true, source: "body-a", wantCode: "pack.script_owned_elsewhere"},
		{name: "rollback owner conflict", action: "rollback", slug: "rc", doctype: "F5Rc", ownerRow: true, source: "body-a", wantCode: "pack.script_owned_elsewhere"},
		{name: "activate empty body", action: "activate", slug: "ab", doctype: "F5Ab", source: "", wantCode: "pack.script_body_missing"},
		{name: "rollback empty body", action: "rollback", slug: "rb", doctype: "F5Rb", source: "", wantCode: "pack.script_body_missing"},
	}
	for _, d := range dialects {
		t.Run(d.name, func(t *testing.T) {
			dsn := os.Getenv(d.dsnEnv)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s database", d.dsnEnv, d.name)
			}
			database, err := sql.Open(d.driver, dsn)
			if err != nil {
				t.Fatal("open disposable database:", err)
			}
			defer database.Close()
			database.SetMaxOpenConns(1)
			dialect := kdb.Resolve(d.name)
			if err := site.BootstrapSystemTables(context.Background(), database, dialect); err != nil {
				t.Fatal("bootstrap system tables:", err)
			}
			if d.name == "postgres" {
				// The PostgreSQL bootstrap declares change_list JSONB, and the activate
				// load query's COALESCE(change_list, '') cannot run on it. Until that is
				// fixed (open issue, see PR), use the TEXT column the PostgreSQL
				// config-version route test uses.
				if _, err := database.Exec(`ALTER TABLE _kora_config_version ALTER COLUMN change_list TYPE TEXT USING change_list::text`); err != nil {
					t.Fatal("retype change_list for activate load query:", err)
				}
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					runScriptRefusalCase(t, database, dialect, tc.action, tc.slug, tc.doctype, tc.ownerRow, tc.source, tc.wantCode)
				})
			}
		})
	}
}

// runScriptRefusalCase seeds one site with an Active version and a target version
// (draft for activate, superseded for rollback) whose owned script is refused, then
// asserts the 409, the absent table, and the untouched rows and journal.
func runScriptRefusalCase(t *testing.T, database *sql.DB, dialect kdb.Dialect, action, slug, doctypeName string, ownerRow bool, source, wantCode string) {
	t.Helper()
	siteName := "site-f5-" + slug
	scriptName := "f5." + slug + ".post"
	targetID := action + "-target-" + slug
	activeID := "active-" + slug

	if ownerRow {
		store := configstore.NewStore(database, dialect)
		// Another pack already owns the row the version's script would take.
		if err := store.SaveScripts([]*doctype.ScriptSnapshot{{
			Name: scriptName, ScriptType: "method", MethodPath: "f5." + slug, IsActive: true,
			OwnerPack: "pack-b", Source: "body-b", ScriptHash: scriptHash("body-b"),
		}}, map[string]string{scriptHash("body-b"): "body-b"}, siteName); err != nil {
			t.Fatal("seed owned script row:", err)
		}
	}
	dt := &doctype.DocType{Name: doctypeName, Module: "Test", Fields: []doctype.Field{{Fieldname: "subject", Fieldtype: "Data"}}}
	owned := &doctype.ScriptSnapshot{
		Name: scriptName, ScriptType: "method", MethodPath: "f5." + slug, IsActive: true,
		OwnerPack: "pack-a", Source: source, ScriptHash: scriptHash("body-a"),
	}
	targetConfig := doctype.ToSExpr(&doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{dt}, Scripts: []*doctype.ScriptSnapshot{owned}})
	activeConfig := doctype.ToSExpr(&doctype.ConfigSnapshot{})
	targetStatus, activeVersion, targetVersion := "Draft", 1, 2
	if action == "rollback" {
		targetStatus, activeVersion, targetVersion = "Superseded", 2, 1
	}
	insertScriptRefusalVersion(t, database, dialect, targetID, siteName, targetVersion, targetStatus, targetConfig)
	insertScriptRefusalVersion(t, database, dialect, activeID, siteName, activeVersion, "Active", activeConfig)

	registry := doctype.NewRegistry()
	handler := NewHandler(registry, &orm.TxManager{DB: database, Registry: registry, Dialect: dialect})
	recorder := httptest.NewRecorder()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/system/config/versions/"+targetID+"/"+action, nil)
	c.Request.Header.Set("Idempotency-Key", "f5-"+slug)
	c.Params = gin.Params{{Key: "id", Value: targetID}}
	c.Set("site_db", database)
	c.Set("site_registry", registry)
	c.Set("site_name", siteName)
	c.Set("site_id", "tenant-"+slug)
	c.Set("site_db_type", dialect.DriverName())
	c.Set("user", "tester")
	c.Set("user_roles", []string{"Administrator"})
	if action == "rollback" {
		handler.HandleConfigVersionRollback(c)
	} else {
		handler.HandleConfigVersionActivate(c)
	}

	if recorder.Code != http.StatusConflict {
		t.Fatalf("%s returned HTTP %d, want 409: %s", action, recorder.Code, recorder.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal("decode refusal body:", err)
	}
	if body.Error.Code != wantCode {
		t.Fatalf("refusal code = %q, want %q; body: %s", body.Error.Code, wantCode, recorder.Body.String())
	}
	assertScriptRefusalUnchanged(t, database, dialect, siteName, scriptName, targetID, activeID, dt.RawTableName(), targetStatus, ownerRow)
}

// assertScriptRefusalUnchanged checks that the refused request wrote no table, no
// journal row, no version status change, and no change to the script row.
func assertScriptRefusalUnchanged(t *testing.T, database *sql.DB, dialect kdb.Dialect, siteName, scriptName, targetID, activeID, table, targetStatus string, ownerRow bool) {
	t.Helper()
	if _, err := database.Exec("SELECT 1 FROM " + table + " WHERE 1 = 0"); err == nil {
		t.Fatalf("refused version created table %s", table)
	}
	var journalRows int
	if err := database.QueryRow(kdb.Rebind(dialect, `SELECT COUNT(*) FROM _kora_config_operation WHERE site = ?`), siteName).Scan(&journalRows); err != nil {
		t.Fatal("count activation journal rows:", err)
	}
	if journalRows != 0 {
		t.Fatalf("refused request left %d activation journal rows", journalRows)
	}
	var status string
	if err := database.QueryRow(kdb.Rebind(dialect, `SELECT status FROM _kora_config_version WHERE id = ?`), targetID).Scan(&status); err != nil {
		t.Fatal("read target version status:", err)
	}
	if status != targetStatus {
		t.Fatalf("target version status = %q, want %q", status, targetStatus)
	}
	if err := database.QueryRow(kdb.Rebind(dialect, `SELECT status FROM _kora_config_version WHERE id = ?`), activeID).Scan(&status); err != nil {
		t.Fatal("read active version status:", err)
	}
	if status != "Active" {
		t.Fatalf("active version status = %q, want Active", status)
	}
	var owner, body string
	err := database.QueryRow(kdb.Rebind(dialect, `SELECT owner_pack, script FROM _kora_script WHERE name = ? AND site = ?`), scriptName, siteName).Scan(&owner, &body)
	switch {
	case ownerRow && err != nil:
		t.Fatal("read owned script row:", err)
	case ownerRow && (owner != "pack-b" || body != "body-b"):
		t.Fatalf("owned script row changed to owner %q body %q", owner, body)
	case !ownerRow && err != sql.ErrNoRows:
		t.Fatalf("refused version wrote script row (owner %q, err %v)", owner, err)
	}
}

func insertScriptRefusalVersion(t *testing.T, database *sql.DB, dialect kdb.Dialect, id, siteName string, version int, status, config string) {
	t.Helper()
	isActive := 0
	if status == "Active" {
		isActive = 1
	}
	if _, err := database.Exec(kdb.Rebind(dialect, `INSERT INTO _kora_config_version
		(id, site, version, created_at, created_by, label, status, is_active, config, change_list)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		id, siteName, version, time.Now(), "tester", status, status, isActive, config, "[]"); err != nil {
		t.Fatal("insert config version fixture:", err)
	}
}

func scriptHash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}
