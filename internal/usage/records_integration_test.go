//go:build integration

package usage

import (
	"context"
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

func TestRecordsCountsOnlyParentRowsAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_USAGE_MYSQL_DSN"},
		{"postgres", "KORA_USAGE_POSTGRES_DSN"},
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
			t.Cleanup(func() { _ = database.Close() })
			dialect := db.Resolve(tc.driver)
			suffix := fmt.Sprintf("%d", time.Now().UnixNano())
			parent := &doctype.DocType{Name: "UsageItem" + suffix, Module: "Usage", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data", Label: "Title"}}}
			child := &doctype.DocType{Name: "UsageLine" + suffix, Module: "Usage", IsChildTable: true, Fields: []doctype.Field{{Fieldname: "qty", Fieldtype: "Int", Label: "Qty"}}}
			single := &doctype.DocType{Name: "UsageSettings" + suffix, Module: "Usage", IsSingle: true, Fields: []doctype.Field{{Fieldname: "rate", Fieldtype: "Int", Label: "Rate"}}}
			registry := doctype.NewRegistry()
			var tables []string
			t.Cleanup(func() { // registered after Close, so it runs first
				for _, table := range tables {
					_, _ = database.Exec("DROP TABLE IF EXISTS " + table)
				}
			})
			for _, dt := range []*doctype.DocType{parent, child, single} {
				registry.Register(dt)
				for _, stmt := range dialect.CreateTable(dt) {
					if _, err = database.Exec(stmt); err != nil {
						t.Fatalf("create %s: %v", dt.Name, err)
					}
				}
				tables = append(tables, dialect.QuoteIdent(dt.RawTableName()))
			}
			insert := func(dt *doctype.DocType, rows int) {
				t.Helper()
				query := db.Rebind(dialect, fmt.Sprintf("INSERT INTO %s (name) VALUES (?)", dialect.QuoteIdent(dt.RawTableName())))
				for i := range rows {
					if _, err = database.Exec(query, fmt.Sprintf("%s-%d", dt.Name, i)); err != nil {
						t.Fatalf("insert into %s: %v", dt.Name, err)
					}
				}
			}
			insert(parent, 3)
			insert(child, 2)
			insert(single, 1)

			got, err := Records{DB: database, Dialect: dialect, Registry: registry}.Count(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got != 3 {
				t.Fatalf("record count = %d, want 3 (parent rows only)", got)
			}
		})
	}
}
