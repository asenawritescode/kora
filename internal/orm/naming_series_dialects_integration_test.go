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
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
)

func TestLiveNamingSeriesAllocatesPerPrefixAcrossDialects(t *testing.T) {
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
			for _, statement := range dialect.SystemTableSQL() {
				if strings.Contains(statement, "_kora_naming_series") {
					if _, err := database.Exec(statement); err != nil {
						t.Fatal(err)
					}
				}
			}
			docName := fmt.Sprintf("Loan%d", time.Now().UnixNano())
			dt := &doctype.DocType{Name: docName, Module: "Library", NamingSeries: "LN-.{branch}.-.YYYY.-.###",
				Fields: []doctype.Field{{Fieldname: "branch", Fieldtype: "Data", Label: "Branch"}}}
			table := dialect.QuoteIdent(dt.RawTableName())
			if _, err := database.Exec(fmt.Sprintf("CREATE TABLE %s (name VARCHAR(140) PRIMARY KEY)", table)); err != nil {
				t.Fatal(err)
			}
			defer database.Exec("DROP TABLE " + table)
			defer database.Exec(db.Rebind(dialect, "DELETE FROM _kora_naming_series WHERE doctype = ?"), docName)

			at := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
			// An existing name under the prefix seeds the counter so it is never reissued.
			if _, err := database.Exec(db.Rebind(dialect, fmt.Sprintf("INSERT INTO %s (name) VALUES (?)", table)), "LN-WEST-2026-041"); err != nil {
				t.Fatal(err)
			}
			allocate := func(branch string) string {
				tx, err := database.Begin()
				if err != nil {
					t.Fatal(err)
				}
				name, _, err := AllocateDocumentName(tx, dialect, dt, map[string]any{"branch": branch}, at)
				if err != nil {
					_ = tx.Rollback()
					t.Fatal(err)
				}
				if _, err := tx.Exec(db.Rebind(dialect, fmt.Sprintf("INSERT INTO %s (name) VALUES (?)", table)), name); err != nil {
					_ = tx.Rollback()
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				return name
			}
			for _, want := range []struct{ branch, name string }{
				{"WEST", "LN-WEST-2026-042"},
				{"WEST", "LN-WEST-2026-043"},
				{"EAST", "LN-EAST-2026-001"},
			} {
				if got := allocate(want.branch); got != want.name {
					t.Fatalf("branch %s name = %q, want %q", want.branch, got, want.name)
				}
			}
			tx, err := database.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, _, err := AllocateDocumentName(tx, dialect, dt, map[string]any{"branch": "x' OR '1'='1"}, at); err == nil {
				t.Fatal("unsafe field value was accepted into a series key")
			}
		})
	}
}
