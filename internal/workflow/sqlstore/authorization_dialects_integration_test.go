//go:build integration

package sqlstore

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func TestAuthorizationStoreLiveReadsAcrossSupportedDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_WORKFLOW_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_WORKFLOW_DISPOSABLE_POSTGRES_DSN"},
		{"libsql", "KORA_WORKFLOW_DISPOSABLE_LIBSQL_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s to an isolated disposable database", tc.env)
			}
			runAuthorizationStoreLiveReads(t, tc.driver, dsn)
		})
	}
}

func runAuthorizationStoreLiveReads(t *testing.T, driver, dsn string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	database, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatal("open disposable database:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(2)
	if err := database.PingContext(ctx); err != nil {
		t.Fatal("ping disposable database:", err)
	}
	dialect := db.Resolve(driver)
	suffix := contract.NewID()
	if len(suffix) < 12 {
		t.Fatalf("generated test identity too short: %q", suffix)
	}
	siteName := "workflow-auth-" + suffix[:12]
	userID := "user-" + suffix[:12]
	tenant := contract.TenantID("tenant-" + suffix)
	instanceID := contract.InstanceID(contract.NewID())
	doctypeName := "AuthInvoice" + suffix[:12]
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{{Name: doctypeName, Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}}}}, nil, nil)
	store, err := NewAuthorizationStore(database, dialect, registry)
	if err != nil {
		t.Fatal("construct workflow authorization store:", err)
	}
	table := dialect.QuoteIdent("tab" + doctypeName)
	for _, statement := range authorizationDDL(driver, table) {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("create disposable authorization fixture: %v\n%s", err, statement)
		}
	}
	t.Cleanup(func() {
		_, _ = database.ExecContext(context.Background(), db.Rebind(dialect, `DELETE FROM _kora_user WHERE site = ? AND name = ?`), siteName, userID)
		_, _ = database.ExecContext(context.Background(), db.Rebind(dialect, `DELETE FROM _kora_workflow_history WHERE tenant_id = ? AND instance_id = ?`), string(tenant), string(instanceID))
		_, _ = database.ExecContext(context.Background(), "DROP TABLE "+table)
	})
	var enabled any = true
	if driver == "libsql" {
		enabled = 1
	}
	if _, err := database.ExecContext(ctx, db.Rebind(dialect, `INSERT INTO _kora_user (site,name,enabled,roles) VALUES (?,?,?,?)`), siteName, userID, enabled, "Reader, Auditor"); err != nil {
		t.Fatal("insert isolated workflow member:", err)
	}
	if _, err := database.ExecContext(ctx, db.Rebind(dialect, `INSERT INTO `+table+` (name,owner,title,creation,modified,modified_by,doc_status,revision) VALUES (?,?,?,?,?,?,?,?)`), "INV-1", userID, "Test invoice", time.Now().UTC(), time.Now().UTC(), userID, 0, 9); err != nil {
		t.Fatal("insert isolated workflow subject:", err)
	}
	if _, err := database.ExecContext(ctx, db.Rebind(dialect, `INSERT INTO _kora_workflow_history (tenant_id,instance_id,sequence,step_id,kind,status,actor_id,detail,created_at) VALUES (?,?,?,?,?,?,?,?,?)`), string(tenant), string(instanceID), 1, "start", "started", "ready", userID, "", time.Now().UTC()); err != nil {
		t.Fatal("insert isolated workflow starter history:", err)
	}
	roles, active, err := store.UserRoles(ctx, siteName, contract.PrincipalID(userID))
	if err != nil || !active || len(roles) != 2 || roles[0] != "Reader" || roles[1] != "Auditor" {
		t.Fatalf("live member snapshot=(%v,%t,%v)", roles, active, err)
	}
	subject, err := store.Subject(ctx, contract.SubjectRef{Kind: doctypeName, ID: "INV-1", Revision: "9"})
	if err != nil || subject.Owner != contract.PrincipalID(userID) || subject.Revision != "9" {
		t.Fatalf("subject authorization=(%+v,%v)", subject, err)
	}
	starter, err := store.WorkflowStarter(ctx, tenant, instanceID)
	if err != nil || starter != contract.PrincipalID(userID) {
		t.Fatalf("workflow starter=(%q,%v)", starter, err)
	}
}

func authorizationDDL(driver, table string) []string {
	switch driver {
	case "mysql":
		return []string{
			`CREATE TABLE IF NOT EXISTS _kora_user (name VARCHAR(140) PRIMARY KEY,site VARCHAR(140) NOT NULL DEFAULT '',email VARCHAR(255) NOT NULL DEFAULT '',password_hash VARCHAR(255) NOT NULL DEFAULT '',full_name VARCHAR(255) NOT NULL DEFAULT '',enabled BOOLEAN NOT NULL DEFAULT TRUE,email_verified_at DATETIME(6),roles TEXT,creation DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),modified DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_history (tenant_id VARCHAR(160) NOT NULL,instance_id VARCHAR(36) NOT NULL,sequence BIGINT NOT NULL,step_id VARCHAR(64) NOT NULL,kind VARCHAR(48) NOT NULL,status VARCHAR(32) NOT NULL,actor_id VARCHAR(160),detail TEXT NOT NULL,created_at DATETIME(6) NOT NULL,PRIMARY KEY(tenant_id,instance_id,sequence)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
			"CREATE TABLE " + table + " (name VARCHAR(160) PRIMARY KEY,owner VARCHAR(160) NOT NULL,title TEXT,creation DATETIME(6),modified DATETIME(6),modified_by VARCHAR(160),doc_status INT NOT NULL,revision BIGINT NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4",
		}
	case "postgres":
		return []string{
			`CREATE TABLE IF NOT EXISTS _kora_user (name VARCHAR(140) PRIMARY KEY,site VARCHAR(140) NOT NULL DEFAULT '',email VARCHAR(255) NOT NULL DEFAULT '',password_hash VARCHAR(255) NOT NULL DEFAULT '',full_name VARCHAR(255) NOT NULL DEFAULT '',enabled BOOLEAN NOT NULL DEFAULT TRUE,email_verified_at TIMESTAMPTZ,roles TEXT,creation TIMESTAMPTZ NOT NULL DEFAULT NOW(),modified TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_history (tenant_id TEXT NOT NULL,instance_id TEXT NOT NULL,sequence BIGINT NOT NULL,step_id TEXT NOT NULL,kind TEXT NOT NULL,status TEXT NOT NULL,actor_id TEXT,detail TEXT NOT NULL,created_at TIMESTAMPTZ NOT NULL,PRIMARY KEY(tenant_id,instance_id,sequence))`,
			"CREATE TABLE " + table + " (name TEXT PRIMARY KEY,owner TEXT NOT NULL,title TEXT,creation TIMESTAMPTZ,modified TIMESTAMPTZ,modified_by TEXT,doc_status INT NOT NULL,revision BIGINT NOT NULL)",
		}
	default:
		return []string{
			`CREATE TABLE IF NOT EXISTS _kora_user (name VARCHAR(140) PRIMARY KEY,site VARCHAR(140) NOT NULL DEFAULT '',email VARCHAR(255) NOT NULL DEFAULT '',password_hash VARCHAR(255) NOT NULL DEFAULT '',full_name VARCHAR(255) NOT NULL DEFAULT '',enabled INTEGER NOT NULL DEFAULT 1,email_verified_at TEXT,roles TEXT,creation TEXT NOT NULL DEFAULT (datetime('now')),modified TEXT NOT NULL DEFAULT (datetime('now')))`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_history (tenant_id TEXT NOT NULL,instance_id TEXT NOT NULL,sequence INTEGER NOT NULL,step_id TEXT NOT NULL,kind TEXT NOT NULL,status TEXT NOT NULL,actor_id TEXT,detail TEXT NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(tenant_id,instance_id,sequence))`,
			"CREATE TABLE " + table + " (name TEXT PRIMARY KEY,owner TEXT NOT NULL,title TEXT,creation TEXT,modified TEXT,modified_by TEXT,doc_status INTEGER NOT NULL,revision INTEGER NOT NULL)",
		}
	}
}
