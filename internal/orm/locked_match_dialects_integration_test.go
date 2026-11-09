//go:build integration

package orm

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/policy"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
)

func TestLiveFirstMatchingNameLockedAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_CONFIG_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_CONFIG_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(tc.env))
			if dsn == "" {
				t.Skipf("set %s to a disposable database", tc.env)
			}
			database, err := sql.Open(tc.driver, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			dialect := db.Resolve(tc.driver)
			dt := &doctype.DocType{Name: fmt.Sprintf("Shift%d", time.Now().UnixNano()), Module: "Ops",
				Fields: []doctype.Field{{Fieldname: "desk", Fieldtype: "Data"}, {Fieldname: "state", Fieldtype: "Data"}}}
			table := dialect.QuoteIdent(dt.RawTableName())
			if _, err := database.Exec(fmt.Sprintf("CREATE TABLE %s (name VARCHAR(140) PRIMARY KEY, owner VARCHAR(140), desk VARCHAR(40), state VARCHAR(20))", table)); err != nil {
				t.Fatal(err)
			}
			defer database.Exec("DROP TABLE " + table)
			for _, row := range [][]string{{"S-1", "D1", "Closed"}, {"S-2", "D1", "Open"}, {"S-3", "D2", "Open"}} {
				if _, err := database.Exec(db.Rebind(dialect, fmt.Sprintf("INSERT INTO %s (name, owner, desk, state) VALUES (?, 'u', ?, ?)", table)), row[0], row[1], row[2]); err != nil {
					t.Fatal(err)
				}
			}
			tx, err := database.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			manager := &TxManager{DB: database, Dialect: dialect, Transaction: tx}
			name, found, err := manager.FirstMatchingNameLocked(dt, `[["desk","=","D1"],["state","=","Open"]]`, policy.RowScope{})
			if err != nil || !found || name != "S-2" {
				t.Fatalf("match = %q %v %v", name, found, err)
			}
			if _, found, err := manager.FirstMatchingNameLocked(dt, `[["desk","=","D9"]]`, policy.RowScope{}); err != nil || found {
				t.Fatalf("unexpected match: %v %v", found, err)
			}
			if _, _, err := manager.FirstMatchingNameLocked(dt, `[["desk; DROP TABLE x","=","D1"]]`, policy.RowScope{}); err == nil {
				t.Fatal("unknown filter field was accepted")
			}
		})
	}
}
