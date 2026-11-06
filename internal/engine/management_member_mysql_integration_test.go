//go:build integration

package engine

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/site"
	_ "github.com/go-sql-driver/mysql"
)

// With KORA_ENGINE_MEMBER_DISPOSABLE_MYSQL_DSN set to a disposable MySQL
// database, every TestManagementMember* test runs against it instead of SQLite.
func init() {
	if os.Getenv("KORA_ENGINE_MEMBER_DISPOSABLE_MYSQL_DSN") != "" {
		memberTestDatabase = openMySQLMemberDatabase
	}
}

var (
	memberMySQLOnce sync.Once
	memberMySQL     *sql.DB
	memberMySQLErr  error
)

func openMySQLMemberDatabase(t *testing.T) (*sql.DB, db.Dialect) {
	t.Helper()
	memberMySQLOnce.Do(func() {
		memberMySQL, memberMySQLErr = sql.Open("mysql", os.Getenv("KORA_ENGINE_MEMBER_DISPOSABLE_MYSQL_DSN"))
		if memberMySQLErr == nil {
			memberMySQLErr = site.BootstrapSystemTablesContext(context.Background(), memberMySQL, db.Resolve("mysql"))
		}
	})
	if memberMySQLErr != nil {
		t.Fatal(memberMySQLErr)
	}
	for _, table := range []string{"_kora_user", "_kora_role", "_kora_operation_audit"} {
		if _, err := memberMySQL.Exec("DELETE FROM " + table); err != nil {
			t.Fatal(err)
		}
	}
	return memberMySQL, db.Resolve("mysql")
}
